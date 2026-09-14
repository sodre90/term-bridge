package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/cmux"
	"github.com/sodre90/term-bridge/internal/server"
	"github.com/sodre90/term-bridge/internal/testutil"
	"github.com/sodre90/term-bridge/internal/tunnel"
)

func TestRelayIsolatesTenants(t *testing.T) {
	const wsA = `{"workspaces":[{"id":"A","current_directory":"/a","preview":"tenant-a-secret","terminals":[{"id":"A-T1","current_directory":"/a","title":"~/a","is_focused":true,"is_ready":true}]}]}`
	const wsB = `{"workspaces":[{"id":"B","current_directory":"/b","preview":"tenant-b-secret","terminals":[{"id":"B-T1","current_directory":"/b","title":"~/b","is_focused":true,"is_ready":true}]}]}`
	binA := testutil.WriteFakeCmux(t, "#!/bin/sh\ncat <<'JSON'\n"+wsA+"\nJSON\n")
	binB := testutil.WriteFakeCmux(t, "#!/bin/sh\ncat <<'JSON'\n"+wsB+"\nJSON\n")
	const relayTok = "relay-secret"
	agentA := server.New(&cmux.Client{Bin: binA}, nil).TrustedHandler(relayTok)
	agentB := server.New(&cmux.Client{Bin: binB}, nil).TrustedHandler(relayTok)

	store, err := auth.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	tenantA, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	tenantB, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	devA, _ := store.Issue(tenantA, "phone-a", "test-device-pubkey-a")
	devB, _ := store.Issue(tenantB, "phone-b", "test-device-pubkey-b")

	rl := New(store, nil, relayTok)
	relayHTTP := httptest.NewServer(rl.Handler())
	defer relayHTTP.Close()

	dial := func(tenantID string, h http.Handler) {
		u := "ws" + strings.TrimPrefix(relayHTTP.URL, "http") + "/agent/tunnel"
		sess, err := tunnel.Dial(context.Background(), u, nil, http.Header{
			"X-Client-Cert-Cn":     {"CN=agent:" + tenantID},
			"X-Client-Cert-Verify": {"SUCCESS"},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sess.Close() })
		go func() { _ = http.Serve(sess, h) }()
	}
	dial(tenantA, agentA)
	dial(tenantB, agentB)

	waitFor(t, func() bool { return rl.reg.Get(tenantA) != nil && rl.reg.Get(tenantB) != nil })

	fetch := func(token string) string {
		req, _ := http.NewRequest("GET", relayHTTP.URL+"/sessions", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	bodyA := fetch(devA)
	if !strings.Contains(bodyA, "tenant-a-secret") {
		t.Fatalf("tenant A's device should see tenant A's data: %s", bodyA)
	}
	if strings.Contains(bodyA, "tenant-b-secret") {
		t.Fatalf("tenant A's device must never see tenant B's data: %s", bodyA)
	}

	bodyB := fetch(devB)
	if !strings.Contains(bodyB, "tenant-b-secret") {
		t.Fatalf("tenant B's device should see tenant B's data: %s", bodyB)
	}
	if strings.Contains(bodyB, "tenant-a-secret") {
		t.Fatalf("tenant B's device must never see tenant A's data: %s", bodyB)
	}
}

func TestRelayRevokedTenantCannotReconnectOrServeDevices(t *testing.T) {
	store, err := auth.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	devTok, _ := store.Issue(tenantID, "phone", "test-device-pubkey-b64")
	store.RevokeTenant(tenantID)

	rl := New(store, nil, "relay-secret")
	relayHTTP := httptest.NewServer(rl.Handler())
	defer relayHTTP.Close()

	req, _ := http.NewRequest("GET", relayHTTP.URL+"/agent/tunnel", nil)
	req.Header.Set("X-Client-Cert-Cn", "CN=agent:"+tenantID)
	req.Header.Set("X-Client-Cert-Verify", "SUCCESS")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("revoked tenant's agent must be refused a tunnel, got %d", resp.StatusCode)
	}

	req2, _ := http.NewRequest("GET", relayHTTP.URL+"/sessions", nil)
	req2.Header.Set("Authorization", "Bearer "+devTok)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked tenant's device token must stop verifying, got %d", resp2.StatusCode)
	}
}

// Device revocation is an agent-facing route reached with an agent's own
// mTLS identity, so it is a second place a tenant boundary could be crossed:
// the token hash it takes as an argument is not secret (it appears in logs
// and in X-Device-ID), so possessing one must not be enough to use it
// against a tenant you are not.
func TestRelayAgentCannotRevokeAnotherTenantsDevice(t *testing.T) {
	store, err := auth.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	tenantA, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	tenantB, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	devA, _ := store.Issue(tenantA, "phone-a", "test-device-pubkey-a")
	deviceA, err := store.Verify(devA)
	if err != nil {
		t.Fatal(err)
	}

	relayHTTP := httptest.NewServer(New(store, nil, "relay-secret").Handler())
	defer relayHTTP.Close()

	revokeAs := func(tenantID, tokenHash string) int {
		req, _ := http.NewRequest("POST", relayHTTP.URL+"/agent/devices/"+tokenHash+"/revoke", nil)
		req.Header.Set("X-Client-Cert-Cn", "CN=agent:"+tenantID)
		req.Header.Set("X-Client-Cert-Verify", "SUCCESS")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := revokeAs(tenantB, deviceA.TokenHash); code != http.StatusNotFound {
		t.Fatalf("cross-tenant revoke status = %d, want 404", code)
	}
	if _, err := store.Verify(devA); err != nil {
		t.Fatalf("tenant A's device must survive tenant B's revoke attempt: %v", err)
	}

	if code := revokeAs(tenantA, deviceA.TokenHash); code != http.StatusOK {
		t.Fatalf("own-tenant revoke status = %d, want 200", code)
	}
	if _, err := store.Verify(devA); err == nil {
		t.Fatal("tenant A revoking its own device should have removed it")
	}
}

// Self-revocation takes no identifier, so a device cannot aim it across the
// tenant boundary -- but it is a device-authenticated route on the relay's
// public surface, so prove that directly rather than by inspection.
func TestRelayDeviceSelfRevokeCannotReachAnotherTenant(t *testing.T) {
	store, err := auth.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	tenantA, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	tenantB, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	tokenA, err := store.Issue(tenantA, "phone-a", "test-device-pubkey-a")
	if err != nil {
		t.Fatal(err)
	}
	tokenB, err := store.Issue(tenantB, "phone-b", "test-device-pubkey-b")
	if err != nil {
		t.Fatal(err)
	}
	deviceA, err := store.Verify(tokenA)
	if err != nil {
		t.Fatal(err)
	}

	relayHTTP := httptest.NewServer(New(store, nil, "relay-secret").Handler())
	defer relayHTTP.Close()

	// Tenant B's device names tenant A's device every way the route could
	// possibly read one.
	body := `{"token_hash":"` + deviceA.TokenHash + `"}`
	req, _ := http.NewRequest("POST", relayHTTP.URL+"/devices/self-revoke", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenB)
	req.Header.Set("X-Device-ID", deviceA.TokenHash)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("self-revoke status = %d, want 200", resp.StatusCode)
	}

	if _, err := store.Verify(tokenA); err != nil {
		t.Fatalf("tenant A's device must survive tenant B's self-revoke: %v", err)
	}
	if _, err := store.Verify(tokenB); err == nil {
		t.Fatal("the caller's own token should be gone")
	}
}
