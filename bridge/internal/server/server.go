// Package server exposes the bridge's HTTP/WebSocket API. Every route except
// /pair requires a device bearer token (auth.Require); the public edge adds
// mTLS in front of all of it.
package server

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/sodre90/cmux-bridge/internal/auth"
	"github.com/sodre90/cmux-bridge/internal/cmux"
	"github.com/sodre90/cmux-bridge/internal/e2e"
	"github.com/sodre90/cmux-bridge/internal/host"
	"github.com/sodre90/cmux-bridge/internal/host/cmuxhost"
	"github.com/sodre90/cmux-bridge/internal/ratelimit"
	"github.com/sodre90/cmux-bridge/internal/yolo"
)

// Server holds the dependencies shared by all handlers.
type Server struct {
	host         host.Host
	store        *auth.Store
	hub          *hub
	terminalPoll time.Duration // how often WS /terminal re-replays for output
	replayGrace  time.Duration // how long WS /terminal holds a pane whose replays are failing
	// sessions is nil unless SetSessions is called (only by runAgent's
	// production wiring). Nil means the plaintext code path every existing
	// test exercises; non-nil enables the opt-in e2e encryption layer.
	sessions *e2e.Store
	// yolo is nil unless SetYoloStore is called (only by runAgent's production
	// wiring). Nil means every workspace behaves as if YOLO mode is off (no
	// stored modes, no auto-reply) -- the plaintext-equivalent default every
	// existing test exercises.
	yolo *yolo.Store
	// pusher is nil unless SetPusher is called (only by runAgent's production
	// wiring, and only when direct mode + FCM config are both present). Nil
	// means direct-mode attention frames are never pushed -- the
	// plaintext-equivalent default every existing test exercises.
	pusher Pusher
	// directTenantID scopes pusher's token lookup to direct mode's one
	// implicit tenant. Only meaningful together with pusher, so both are set
	// by the same call.
	directTenantID string
	// lastEventAt is UnixNano of the last frame ingestEvents processed off
	// the `cmux events --reconnect` stream (0 means "none yet"), read by
	// LastEventAt for the cmux-bridge status subcommand.
	lastEventAt atomic.Int64
	// testPushCooldown bounds how often a single direct-mode device may
	// trigger handleTestPushDevice (see test_push.go) -- defense-in-depth
	// against a paired device turning "send test notification" into a
	// push-spam vector against itself and its own FCM quota.
	testPushCooldown *ratelimit.Cooldown
	// sockets holds live device WebSockets so revocation can reach the ones
	// that authenticated before it happened (see conntrack.go).
	sockets *socketTracker
	// attachments is nil unless SetAttachmentStore is called (runAgent's
	// production wiring, and the tests that exercise it). Nil means an
	// "attach" frame is refused with a failed ack and nothing is written.
	attachments *AttachmentStore
}

// SetAttachmentStore enables image attachments on the terminal socket. Same
// post-construction-setter idiom as SetSessions/SetYoloStore.
func (s *Server) SetAttachmentStore(store *AttachmentStore) { s.attachments = store }

// LastEventAt returns when this agent last processed a frame from its cmux
// events stream, or the zero time if none has arrived yet.
func (s *Server) LastEventAt() time.Time {
	ns := s.lastEventAt.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// SetYoloStore enables per-workspace YOLO auto-reply. Called only by
// runAgent's production wiring; no test calls this, so pre-existing tests
// continue to exercise the "always off" code path unchanged.
func (s *Server) SetYoloStore(store *yolo.Store) { s.yolo = store }

// SetPusher enables agent-native push for direct-mode-paired devices,
// scoped to tenantID (direct mode's one implicit tenant -- the same value
// already passed to MountDirectPairing). Called only by runAgent's
// production wiring. Deliberately not a New() parameter: pusher is
// optional, orthogonal config, following the same post-construction-setter
// idiom as SetSessions/SetYoloStore rather than growing New()'s signature
// and breaking every existing test call site.
func (s *Server) SetPusher(p Pusher, tenantID string) {
	s.pusher = p
	s.directTenantID = tenantID
}

// New constructs a Server fronting cmux. Kept as the one-line spelling
// every caller and test already uses; NewWithHost is the general form.
func New(c *cmux.Client, s *auth.Store) *Server {
	return NewWithHost(cmuxhost.New(c), s)
}

// NewWithHost constructs a Server over any terminal backend.
func NewWithHost(h host.Host, s *auth.Store) *Server {
	return &Server{
		host:             h,
		store:            s,
		hub:              newHub(),
		terminalPoll:     250 * time.Millisecond,
		replayGrace:      terminalReplayGrace,
		testPushCooldown: ratelimit.NewCooldown(testPushDeviceCooldown),
		sockets:          newSocketTracker(),
	}
}

// Handler returns the fully-wired HTTP handler (device-bearer auth on every
// route; the public edge adds mTLS in front).
func (s *Server) Handler() http.Handler {
	return s.routes(s.authWrap)
}
