package relay

import (
	"context"
	"expvar"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/cmux"
	"github.com/sodre90/term-bridge/internal/metrics"
	"github.com/sodre90/term-bridge/internal/server"
	"github.com/sodre90/term-bridge/internal/testutil"
	"github.com/sodre90/term-bridge/internal/tunnel"
)

// expvarIntValue reads an expvar.Map entry as an *expvar.Int, defaulting to 0
// for a key that hasn't been touched yet (Map.Get returns nil until the first
// Add/Set against that key).
func expvarIntValue(t *testing.T, m *expvar.Map, key string) int64 {
	t.Helper()
	v := m.Get(key)
	if v == nil {
		return 0
	}
	iv, ok := v.(*expvar.Int)
	if !ok {
		t.Fatalf("metric %q: got %T, want *expvar.Int", key, v)
	}
	return iv.Value()
}

// TestMetricsTunnelGaugeAndProxyRequestsMove drives a full agent-tunnel-up +
// proxied-request round trip and asserts internal/metrics.TunnelsActive (a
// gauge on relay.Registry) and ProxyRequestsTotal (proxy.go's per-tenant
// choke point) both move by the expected delta -- the docs/improvement-
// guide.md §6.2 accept criterion. Deltas, not absolute values: expvar vars
// are process-global, so other tests in this package touch the same
// counters; only the delta this test itself causes is deterministic.
func TestMetricsTunnelGaugeAndProxyRequestsMove(t *testing.T) {
	const ws = `{"workspaces":[]}`
	bin := testutil.WriteFakeCmux(t, "#!/bin/sh\ncat <<'JSON'\n"+ws+"\nJSON\n")
	agentSrv := server.New(&cmux.Client{Bin: bin}, nil)
	const relayTok = "relay-secret"
	trusted := agentSrv.TrustedHandler(relayTok)

	relayStore, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := relayStore.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	devTok, err := relayStore.Issue(tenantID, "phone", "test-device-pubkey-b64")
	if err != nil {
		t.Fatal(err)
	}
	rl := New(relayStore, nil, relayTok)
	relayHTTP := httptest.NewServer(rl.Handler())
	defer relayHTTP.Close()

	tunnelsBefore := metrics.TunnelsActive.Value()

	u := "ws" + strings.TrimPrefix(relayHTTP.URL, "http") + "/agent/tunnel"
	sess, err := tunnel.Dial(context.Background(), u, nil, http.Header{
		"X-Client-Cert-Cn":     {"CN=agent:" + tenantID},
		"X-Client-Cert-Verify": {"SUCCESS"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	go func() { _ = http.Serve(sess, trusted) }()

	waitFor(t, func() bool { return rl.reg.Get(tenantID) != nil })

	if got, want := metrics.TunnelsActive.Value(), tunnelsBefore+1; got != want {
		t.Fatalf("TunnelsActive after tunnel up = %d, want %d (was %d)", got, want, tunnelsBefore)
	}

	proxyBefore := expvarIntValue(t, metrics.ProxyRequestsTotal, tenantID)

	req, _ := http.NewRequest("GET", relayHTTP.URL+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+devTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 through relay, got %d", resp.StatusCode)
	}

	if got, want := expvarIntValue(t, metrics.ProxyRequestsTotal, tenantID), proxyBefore+1; got != want {
		t.Fatalf("ProxyRequestsTotal[%s] = %d, want %d", tenantID, got, want)
	}

	sess.Close()
	// Registry.Get reports a closed session as gone (sess.IsClosed()) before
	// Registry.Clear -- which is what actually updates TunnelsActive -- runs
	// in handleTunnel's own cleanup goroutine on <-sess.CloseChan(). Waiting
	// on Get alone races Clear; wait on the metric itself instead.
	waitFor(t, func() bool { return metrics.TunnelsActive.Value() == tunnelsBefore })
	if got, want := metrics.TunnelsActive.Value(), tunnelsBefore; got != want {
		t.Fatalf("TunnelsActive after tunnel down = %d, want %d", got, want)
	}
}

// TestMetricsProxyAgentOfflineMoves asserts ProxyAgentOfflineTotal is
// incremented, keyed by tenant, exactly when a proxied request fails because
// its tenant has no registered tunnel -- proxy.go's ErrorHandler branch for
// ErrAgentOffline.
func TestMetricsProxyAgentOfflineMoves(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	devTok, err := store.Issue(tenantID, "phone", "test-device-pubkey-b64")
	if err != nil {
		t.Fatal(err)
	}
	before := expvarIntValue(t, metrics.ProxyAgentOfflineTotal, tenantID)

	p := newProxy(NewRegistry(), "tok", NewConnTracker()) // no session registered for tenantID
	handler := auth.Require(store, p)

	req := httptest.NewRequest("GET", "http://relay/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+devTok)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rr.Code)
	}

	if got, want := expvarIntValue(t, metrics.ProxyAgentOfflineTotal, tenantID), before+1; got != want {
		t.Fatalf("ProxyAgentOfflineTotal[%s] = %d, want %d", tenantID, got, want)
	}
}

// TestMetricsDebugVarsServed exercises the accept criterion literally: an
// HTTP GET of /debug/vars on the relay's existing loopback listener shows
// the metrics package's counters.
func TestMetricsDebugVarsServed(t *testing.T) {
	rl := New(nil, nil, "tok")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/debug/vars")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 from /debug/vars, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Fatalf("want JSON content type, got %q", ct)
	}
}
