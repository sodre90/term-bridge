package server

import (
	"crypto/subtle"
	"net/http"

	"github.com/sodre90/term-bridge/internal/auth"
)

// RequireRelayToken gates a handler behind a static shared token sent by the
// relay as X-Relay-Token. On the agent this replaces device-bearer auth: the
// relay is the device gatekeeper, and the only reachable path to the agent is
// the mutually-authenticated tunnel. Constant-time compare so the token can't
// be recovered by timing (mirrors relay.requireEdge).
func RequireRelayToken(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Relay-Token")), want) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// routes wires the API onto a mux using wrap as the per-route middleware.
// Device registration (/devices/register) is deliberately not mounted here:
// it's added directly onto DirectHandler()'s route set (see direct.go)
// instead, because this function's wrap is shared by TrustedHandler (the
// relay-tunneled path, with no real per-device bearer validation at the
// agent) and DirectHandler (which does have one) -- the route only makes
// sense for the latter.
func (s *Server) routes(wrap func(http.Handler) http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /version", wrap(http.HandlerFunc(s.handleVersion)))
	mux.Handle("GET /sessions", wrap(http.HandlerFunc(s.handleSessions)))
	mux.Handle("GET /events", wrap(http.HandlerFunc(s.handleEvents)))
	mux.Handle("GET /terminal/{id}", wrap(http.HandlerFunc(s.handleTerminal)))
	mux.Handle("GET /feed/pending", wrap(http.HandlerFunc(s.handleFeedPending)))
	mux.Handle("POST /feed/{id}/reply", wrap(http.HandlerFunc(s.handleFeedReply)))
	mux.Handle("POST /sessions/{id}/rename", wrap(http.HandlerFunc(s.handleRenameWorkspace)))
	mux.Handle("POST /sessions/{id}/yolo-mode", wrap(http.HandlerFunc(s.handleSetYoloMode)))
	mux.Handle("POST /sessions", wrap(http.HandlerFunc(s.handleCreateWorkspace)))
	mux.Handle("DELETE /sessions/{id}", wrap(http.HandlerFunc(s.handleCloseWorkspace)))
	mux.Handle("POST /sessions/{id}/select", wrap(http.HandlerFunc(s.handleSelectWorkspace)))
	mux.Handle("GET /sessions/{id}/layout", wrap(http.HandlerFunc(s.handleLayout)))
	mux.Handle("POST /sessions/{id}/panes", wrap(http.HandlerFunc(s.handleCreatePane)))
	mux.Handle("DELETE /sessions/{id}/panes/{surfaceId}", wrap(http.HandlerFunc(s.handleCloseSurface)))
	return mux
}

// TrustedHandler is the handler the Mac agent serves over the relay tunnel:
// device-bearer auth is replaced by the relay-token check, and the opt-in
// e2e encryption layer (SetSessions) wraps the whole route set.
//
// /internal/test-push-encrypt (see test_push.go's handleTestPushEncrypt) is
// mounted on a separate outer mux ahead of the encryptionMiddleware-wrapped
// route set, rather than inside routes() like every other trusted route: it
// is called by the relay itself (never a device) naming an arbitrary target
// deviceID in its body, so it has no X-Device-ID of its own for
// encryptionMiddleware to gate on. An exact mux pattern always wins over the
// "/" catch-all regardless of registration order, so this reaches
// handleTestPushEncrypt directly -- the same effect /events' Upgrade-header
// bypass already has for the relay's own plaintext push-monitor
// subscription, just arranged via mux precedence instead of a header check
// since this route is a plain POST, not a websocket upgrade.
func (s *Server) TrustedHandler(relayToken string) http.Handler {
	relayWrap := func(h http.Handler) http.Handler {
		return RequireRelayToken(relayToken, h)
	}
	encrypted := s.encryptionMiddleware(s.routes(relayWrap))

	top := http.NewServeMux()
	top.Handle("POST /internal/test-push-encrypt", relayWrap(http.HandlerFunc(s.handleTestPushEncrypt)))
	top.Handle("/", encrypted)
	return top
}

// authWrap is the production device-bearer middleware used by Handler.
func (s *Server) authWrap(h http.Handler) http.Handler { return auth.Require(s.store, h) }
