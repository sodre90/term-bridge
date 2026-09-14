package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/sodre90/term-bridge/internal/tunnel"
)

// tunnelAcceptServer starts an httptest.Server that upgrades every request to
// a yamux tunnel via tunnel.Accept and hands each accepted session to
// sessions. It never blocks the handler on the session's lifetime: once
// hijacked by the WebSocket upgrade, the connection is independent of the
// handler goroutine, so the caller is free to close the returned server or
// the session itself to end the tunnel.
func tunnelAcceptServer(t *testing.T, sessions chan<- *yamux.Session) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, err := tunnel.Accept(w, r)
		if err != nil {
			return
		}
		sessions <- sess
	}))
}

// TestDialAndServeCallsOnConnected drives dialAndServe against a real
// tunnel.Accept endpoint and asserts onConnected fires once the dial
// succeeds, before it blocks serving -- runAgent relies on this to flip its
// relay-tunnel-up status (see internal/status) the moment the tunnel is
// live, not only once it later ends.
func TestDialAndServeCallsOnConnected(t *testing.T) {
	sessions := make(chan *yamux.Session, 1)
	srv := tunnelAcceptServer(t, sessions)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	var connected atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	done := make(chan error, 1)
	go func() {
		done <- dialAndServe(context.Background(), wsURL, nil, handler, func() { connected.Store(true) })
	}()

	var serverSess *yamux.Session
	select {
	case serverSess = <-sessions:
	case <-time.After(2 * time.Second):
		t.Fatal("server never accepted a tunnel session")
	}
	// The server accepting its side of the handshake doesn't guarantee the
	// client's Dial (and dialAndServe's subsequent onConnected call) has
	// returned yet -- poll briefly rather than assuming synchronicity.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !connected.Load() {
		time.Sleep(10 * time.Millisecond)
	}
	if !connected.Load() {
		t.Fatal("onConnected was not called after a successful dial")
	}

	// Close the server-side session directly (rather than srv.Close(), which
	// only waits on non-hijacked requests and would never observe this
	// WebSocket-hijacked connection) so dialAndServe's client-side
	// http.Serve(sess, handler) unblocks.
	_ = serverSess.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dialAndServe did not return after the tunnel closed")
	}
}

// TestDialAndServeReturnsWhenContextEnds is the SIGTERM path: runAgent's
// context ends and dialAndServe must come back on its own, tearing the
// tunnel down so the relay sees the agent leave -- the tunnel used to outlive
// the context and hold the process in Accept until systemd aborted it.
func TestDialAndServeReturnsWhenContextEnds(t *testing.T) {
	sessions := make(chan *yamux.Session, 1)
	srv := tunnelAcceptServer(t, sessions)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- dialAndServe(ctx, wsURL, nil, handler, nil) }()

	var serverSess *yamux.Session
	select {
	case serverSess = <-sessions:
	case <-time.After(2 * time.Second):
		t.Fatal("server never accepted a tunnel session")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dialAndServe did not return after its context ended")
	}
	if _, err := serverSess.Accept(); err == nil {
		t.Fatal("the relay side of the tunnel is still open after the agent's context ended")
	}
}

// TestDialAndServeNilOnConnectedIsSafe exercises the real production call
// shape (runAgent always passes a non-nil callback, but the parameter is
// optional by contract -- guard it).
func TestDialAndServeNilOnConnectedIsSafe(t *testing.T) {
	sessions := make(chan *yamux.Session, 1)
	srv := tunnelAcceptServer(t, sessions)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	done := make(chan error, 1)
	go func() { done <- dialAndServe(context.Background(), wsURL, nil, handler, nil) }()

	var serverSess *yamux.Session
	select {
	case serverSess = <-sessions:
	case <-time.After(2 * time.Second):
		t.Fatal("server never accepted a tunnel session")
	}
	_ = serverSess.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dialAndServe did not return after the tunnel closed")
	}
}
