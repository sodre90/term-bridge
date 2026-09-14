package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sodre90/term-bridge/internal/httpjson"
	"github.com/sodre90/term-bridge/internal/wire"
)

// resolveAttention works out what an attention frame should say and whether it
// should be raised at all, and reports whether anyone still needs alerting.
//
// Title becomes the workspace's live title and Preview the pending prompt's
// own text — the actual question, or the tool a permission gate is holding.
// The event stream redacts that text but feed.list carries it verbatim, so
// this lookup is the only way a notification can say *what* the agent is
// asking rather than merely that it is asking.
//
// The notification content and YOLO's auto-approve both need the same two
// answers from cmux — the frame's workspace and the prompts pending in its cwd
// — so they share one pair of RPCs here rather than each making their own.
// That briefly blocks ingestEvents's single-goroutine scan loop, which is why
// it is worth not doing twice; a workspace with YOLO off spends nothing beyond
// these two calls.
//
// Returning false means YOLO silently approved the prompt, so the caller must
// clear NeedsAttention: something already answered must not alert the phone,
// on this agent's own push or on the relay's pushmon (which trusts that flag
// with no YOLO visibility of its own).
//
// Each step degrades independently: a failed workspace lookup leaves the cheap
// cwd-basename Title classify already set, and a prompt with no text worth
// showing (or no pending item at all, which is the normal case for an idle
// "waiting for your input" Notification) falls back to the workspace's status
// line and then to PushBody's hook-derived phrase.
func (s *Server) resolveAttention(ctx context.Context, f *wire.EventFrame) bool {
	if f.WorkspaceID == "" {
		return true
	}
	ws, ok := s.findWorkspace(ctx, f.WorkspaceID)
	if !ok {
		return true
	}
	if ws.Title != "" {
		f.Title = ws.Title
	}
	cwd := canonicalPath(ws.CWD)
	pending := s.listPendingItems(ctx)
	if mode := s.yoloMode(f.WorkspaceID); mode != "" && s.replyPendingPermissions(ctx, pending, cwd, mode) {
		return false
	}
	f.Preview = agentStatusLine(ws)
	if item, ok := newestPendingForCWD(pending, cwd); ok {
		if body := promptBody(item); body != "" {
			f.Preview = body
		}
	}
	return true
}

// agentStatusLine returns the workspace's preview only when the host
// recognised it as an agent-status line (Attention set). cmux's workspace
// preview is a general last-activity line, not a status field: it also
// carries cmux's own system banners (observed live on a workspace that was
// in fact waiting for input: "macOS is reporting sustained critical memory
// pressure. cmux has shed hidden resources; close idle workspaces…"), which
// as a notification body reads as the reason the agent needs you. Anything
// unrecognized is dropped in favour of a generic phrase.
func agentStatusLine(ws wire.Workspace) string {
	if ws.Attention == "" {
		return ""
	}
	return ws.Preview
}

// onEvent takes one classified frame off the host's event stream: it
// enriches and pushes attention frames, then broadcasts to every /events
// subscriber. Relay-side push (internal/relay's pushmon) subscribes to that
// same /events stream over a plaintext internal connection (see
// writeEventFrame) -- resolveAttention's real Title/Preview never reach it
// directly; buildEncryptedPush turns them into per-device ciphertexts first,
// which is the only form of this content the relay (or this agent's own
// direct-mode push) ever forwards to FCM.
func (s *Server) onEvent(ctx context.Context, f wire.EventFrame) {
	s.lastEventAt.Store(time.Now().UnixNano())
	if f.NeedsAttention {
		if s.resolveAttention(ctx, &f) {
			f.EncryptedPush = s.buildEncryptedPush(f)
			s.maybeSendPush(ctx, f)
		} else {
			f.NeedsAttention = false // YOLO already answered it
		}
	}
	s.hub.broadcast(f)
}

// RunEvents keeps the host's event stream flowing into the hub until ctx
// ends. It is started once by the agent's tunnel handler. The relay
// subscribes to this same /events stream over the tunnel to drive FCM push
// (internal/relay/pushmon.go).
func (s *Server) RunEvents(ctx context.Context) {
	s.host.RunEvents(ctx, func(f wire.EventFrame) { s.onEvent(ctx, f) })
}

// ---- WebSocket fan-out hub ----

type hub struct {
	mu    sync.Mutex
	conns map[chan wire.EventFrame]bool
}

func newHub() *hub { return &hub{conns: map[chan wire.EventFrame]bool{}} }

func (h *hub) register() chan wire.EventFrame {
	ch := make(chan wire.EventFrame, 32)
	h.mu.Lock()
	h.conns[ch] = true
	h.mu.Unlock()
	return ch
}

func (h *hub) unregister(ch chan wire.EventFrame) {
	h.mu.Lock()
	if h.conns[ch] {
		delete(h.conns, ch)
		close(ch)
	}
	h.mu.Unlock()
}

func (h *hub) broadcast(f wire.EventFrame) {
	h.mu.Lock()
	for ch := range h.conns {
		select {
		case ch <- f:
		default: // drop for a slow client rather than block the stream
		}
	}
	h.mu.Unlock()
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true }, // auth is token + mTLS, not Origin
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	var deviceID string
	if s.sessions != nil {
		// A present X-Device-ID means this is a device subscriber (the
		// relay's proxy Director injects it on every device-originated
		// request) -- gate on it being paired and encrypt its frames below.
		// A missing X-Device-ID means this is the relay's own internal
		// push-monitor subscription (internal/relay/pushmon.go dials
		// directly over the tunnel with only X-Relay-Token, which
		// RequireRelayToken has already checked by this point in
		// TrustedHandler) -- serve it plaintext so FCM fan-out keeps
		// working. This exposes no more than the relay already saw before
		// e2e encryption existed; only device-bound frames are e2e.
		deviceID = r.Header.Get("X-Device-ID")
		if deviceID != "" {
			if _, ok := s.sessions.SharedSecret(deviceID); !ok {
				httpjson.Error(w, http.StatusConflict, "not_paired")
				return
			}
		}
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	ch := s.hub.register()
	defer s.hub.unregister(ch)
	// Only device subscribers: an empty deviceID is the relay's own
	// push-monitor subscription, which no device revocation should ever tear
	// down. Unregistering is what actually ends the loop below -- closing the
	// socket alone would only surface at the next frame, and an idle stream
	// may not see one for hours.
	if deviceID != "" {
		defer s.sockets.track(deviceID, func() {
			s.hub.unregister(ch)
			_ = c.Close()
		})()
	}

	// Drain/await client close so a dead socket unblocks the writer.
	go func() {
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				_ = c.Close()
				return
			}
		}
	}()

	for f := range ch {
		_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := s.writeEventFrame(c, deviceID, f); err != nil {
			return
		}
	}
}

// writeEventFrame sends f as a plain JSON text frame when encryption is
// disabled (s.sessions == nil) or the caller is the relay's own internal
// push-monitor subscription (deviceID == ""), or as a binary e2e-encrypted
// frame otherwise. In a deployment with e2e enabled, the relay's own
// subscription additionally gets Title and Preview redacted -- a blind relay
// must never learn a tenant's workspace name or live status text. It still
// gets EncryptedPush's per-device ciphertexts and the routing metadata
// (kind, ids) it needs to fan out push notifications. When s.sessions == nil
// there is no device/relay distinction or blindness guarantee being made at
// all (a bare test/dev configuration), so nothing is redacted.
func (s *Server) writeEventFrame(c *websocket.Conn, deviceID string, f wire.EventFrame) error {
	if s.sessions == nil {
		return c.WriteJSON(f)
	}
	if deviceID == "" {
		f.Title = ""
		f.Preview = ""
		return c.WriteJSON(f)
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	frame, err := s.sessions.EncryptFrame(deviceID, raw)
	if err != nil {
		return err
	}
	return c.WriteMessage(websocket.BinaryMessage, frame)
}
