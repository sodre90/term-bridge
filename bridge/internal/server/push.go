package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/httpjson"
	"github.com/sodre90/term-bridge/internal/metrics"
	"github.com/sodre90/term-bridge/internal/push"
	"github.com/sodre90/term-bridge/internal/wire"
)

// Pusher sends an FCM data message to one registration token. push.Sender
// (internal/push) satisfies it -- see internal/relay.Pusher for the
// identical shape used relay-side; the two are duck-typed independently
// since relay and server are separate packages with no reason to share an
// interface type across a package boundary that otherwise has none.
type Pusher interface {
	Send(ctx context.Context, fcmToken, title, body string, data map[string]string) error
}

// pushPayload is the plaintext JSON encrypted per-device into
// EventFrame.EncryptedPush -- the only form a NeedsAttention frame's
// notification content ever takes once it leaves this agent, whether bound
// for relay-mode fan-out or FCM/Google's own servers.
type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// buildEncryptedPush encrypts f's notification content once per device this
// agent still considers currently paired (direct or relay-mediated alike --
// see e2e.Store.ActiveDeviceIDs), so relay-mode push never needs the real
// Title/Preview to build a notification: it only ever receives these opaque,
// per-device ciphertexts (see writeEventFrame's redaction of the relay's own
// subscription). Returns nil when e2e isn't configured (s.sessions == nil);
// callers then send push with no "e2e" data key at all, and the app shows a
// generic notification instead of silently dropping it.
func (s *Server) buildEncryptedPush(f wire.EventFrame) map[string]string {
	if s.sessions == nil {
		return nil
	}
	payload, err := json.Marshal(pushPayload{Title: f.PushTitle(), Body: f.PushBody()})
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, deviceID := range s.sessions.ActiveDeviceIDs() {
		frame, err := s.sessions.EncryptFrame(deviceID, payload)
		if err != nil {
			continue
		}
		out[deviceID] = base64.StdEncoding.EncodeToString(frame)
	}
	return out
}

// maybeSendPush fans a NeedsAttention frame's encrypted push payload out to
// every FCM token registered in this agent's own local device store
// (direct-mode pairs only -- the relay's separate store/pushmon subscription
// handles relay-paired devices completely independently and is untouched by
// this). No-op with zero store/network cost when direct mode or FCM aren't
// configured, the common case for an agent that hasn't opted into either.
func (s *Server) maybeSendPush(ctx context.Context, f wire.EventFrame) {
	if s.pusher == nil || s.store == nil {
		return
	}
	devices := s.store.TenantFCMDevices(s.directTenantID)
	if len(devices) == 0 {
		return
	}
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, dev := range devices {
		data := map[string]string{
			"type":         "attention",
			"feed_id":      f.FeedID,
			"workspace_id": f.WorkspaceID,
			"surface_id":   f.SurfaceID,
			"kind":         f.Kind,
			"slot":         "direct",
		}
		// Real notification content (title/body) is never passed to Send as
		// plaintext -- only this opaque per-device ciphertext, if one could
		// be built. Missing it (e2e not configured) is not treated as an
		// error: the app falls back to a generic, content-free notification.
		if blob, ok := f.EncryptedPush[dev.DeviceID]; ok {
			data["e2e"] = blob
		}
		if err := s.pusher.Send(sendCtx, dev.FCMToken, "", "", data); err != nil {
			metrics.PushFailedTotal.Add(1)
			push.DropDeadToken(s.store, dev.FCMToken, err)
			slog.Warn("agent: direct-mode push failed", "tenant_id", s.directTenantID, "kind", f.Kind, "workspace_id", f.WorkspaceID, "err", err)
			continue
		}
		metrics.PushSentTotal.Add(1)
	}
}

type registerDeviceRequest struct {
	FCMToken string `json:"fcm_token"`
}

// handleRegisterDevice stores a device's FCM registration token in this
// agent's own local store, keyed by the caller's own bearer token (NOT
// X-Device-ID, which carries a hash -- SetFCMToken hashes its argument
// itself; see auth.BearerToken's use here and identically at
// internal/relay/relay.go's handleRegister). Mounted only on
// DirectHandler()'s route set (see direct.go) -- never on TrustedHandler(),
// whose relay-tunneled requests have no real per-device bearer validation
// at the agent, so auth.BearerToken(r) would be meaningless there.
func (s *Server) handleRegisterDevice(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		httpjson.Error(w, http.StatusServiceUnavailable, "not_available")
		return
	}
	var rq registerDeviceRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&rq); err != nil || rq.FCMToken == "" {
		httpjson.Error(w, http.StatusBadRequest, "missing fcm_token")
		return
	}
	if err := s.store.SetFCMToken(auth.BearerToken(r), rq.FCMToken); err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			httpjson.Error(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		var tenantID, device string
		if dev, ok := auth.DeviceFromContext(r.Context()); ok {
			tenantID, device = dev.TenantID, dev.HashSuffix
		}
		slog.Error("agent: set fcm token", "tenant_id", tenantID, "device", device, "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal_error")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]bool{"ok": true})
}
