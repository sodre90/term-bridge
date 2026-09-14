package relay

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/sodre90/term-bridge/internal/auth"
)

// TestReconnectSwapsRegistryWhileRequestInFlight exercises an agent
// redialing (Registry.Set replacing the tenant's session, as handleTunnel in
// relay.go does on every fresh tunnel) while a device request is already
// streaming against the prior session.
//
// The actual behavior this asserts, read directly from the code rather than
// assumed: Registry.Set (registry.go) closes the prior session as soon as
// the new one is installed, and yamux's Session.Close force-closes every
// stream still open on that session. newProxy's DialContext (proxy.go)
// resolves the target session once per request via a fresh http.Transport
// dial with no retry logic anywhere in the proxy. So a request already in
// flight against the old session is NOT transparently retried against the
// new one -- it observes its stream die and fails. Only requests issued
// *after* the swap get routed to the new session. This test proves both
// halves: the in-flight request fails rather than silently succeeding or
// being retried, and a subsequent request is served correctly by the new
// session ("service continues").
func TestReconnectSwapsRegistryWhileRequestInFlight(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/reconnect.db")
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

	reg := NewRegistry()
	p := newProxy(reg, "relay-secret", NewConnTracker())
	handler := auth.Require(store, p)

	authedRequest := func() *http.Request {
		req := httptest.NewRequest("GET", "http://relay/sessions", nil)
		req.Header.Set("Authorization", "Bearer "+devTok)
		return req
	}

	// Old session: its handler blocks mid-request so the test can
	// deterministically swap the registry while a request is still in
	// flight against it, instead of racing a timing window.
	oldClientConn, oldAgentConn := net.Pipe()
	t.Cleanup(func() { oldClientConn.Close(); oldAgentConn.Close() })
	oldAgentSess, err := yamux.Server(oldAgentConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldRelaySess, err := yamux.Client(oldClientConn, nil)
	if err != nil {
		t.Fatal(err)
	}

	reqStarted := make(chan struct{})
	unblock := make(chan struct{})
	go func() {
		_ = http.Serve(oldAgentSess, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(reqStarted)
			<-unblock
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("stale"))
		}))
	}()

	oldStopped := make(chan struct{})
	reg.Set(tenantID, oldRelaySess, func() { close(oldStopped) })

	inFlightDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, authedRequest())
		inFlightDone <- rr
	}()

	select {
	case <-reqStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request never reached the old session's agent handler")
	}

	// Redial: install a new session for the same tenant, simulating the
	// agent process restarting and reconnecting while the first request is
	// still blocked mid-flight on the old one.
	newClientConn, newAgentConn := net.Pipe()
	t.Cleanup(func() { newClientConn.Close(); newAgentConn.Close() })
	newAgentSess, err := yamux.Server(newAgentConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	newRelaySess, err := yamux.Client(newClientConn, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = http.Serve(newAgentSess, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("fresh"))
		}))
	}()

	reg.Set(tenantID, newRelaySess, nil)

	// The swap must take effect immediately: the old session's stop func
	// runs, its session closes, and Get() now reports the new session.
	select {
	case <-oldStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("old session's stop func was not called on swap")
	}
	if !oldRelaySess.IsClosed() {
		t.Fatal("old session should be closed once replaced")
	}
	if reg.Get(tenantID) != newRelaySess {
		t.Fatal("registry should report the new session as current immediately after Set")
	}

	// Closing the old session force-closes the stream underneath the
	// in-flight request (confirmed against yamux's Session.Close, which
	// calls stream.forceClose on every open stream). Unblock the stale
	// handler -- its write lands on an already-dying stream and is
	// irrelevant -- and confirm the in-flight request surfaces as a failure
	// rather than silently succeeding or being retried against the new
	// session.
	close(unblock)
	rr := <-inFlightDone
	if rr.Code == http.StatusOK {
		t.Fatalf("in-flight request unexpectedly returned 200 (body=%q) after its session closed mid-flight; the code has no retry path, so this should have failed instead of silently succeeding via some other route", rr.Body.String())
	}

	// Service continues: a request issued after the swap is served by the
	// new session.
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, authedRequest())
	if rr2.Code != http.StatusOK || rr2.Body.String() != "fresh" {
		t.Fatalf("post-swap request should be served by the new session, got code=%d body=%q", rr2.Code, rr2.Body.String())
	}
}
