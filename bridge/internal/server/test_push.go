package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/httpjson"
	"github.com/sodre90/term-bridge/internal/metrics"
	"github.com/sodre90/term-bridge/internal/push"
	"github.com/sodre90/term-bridge/internal/wire"
)

// testPushDeviceCooldown bounds how often either test-push entry point below
// will actually place an FCM call for the same device -- see
// docs/improvement-guide.md item 7.7's "rate-limit sensibly" requirement.
// 30s is generous for a manual "let me check push works" tap while still
// making the endpoint useless as a spam vector.
const testPushDeviceCooldown = 30 * time.Second

const (
	testPushTitle = "Test notification"
	testPushBody  = "This is a test push from cmux -- if you see this, push notifications are working."
)

// buildTestPushCiphertext returns the base64 e2e ciphertext of a test-push
// payload for deviceID, or ok=false when e2e isn't configured on this agent
// (s.sessions == nil) or deviceID isn't currently paired. It reuses the same
// pushPayload shape and EncryptFrame call buildEncryptedPush (push.go) uses
// for a real attention push, just for exactly one device instead of every
// ActiveDeviceIDs entry.
func (s *Server) buildTestPushCiphertext(deviceID string) (ciphertext string, ok bool) {
	if s.sessions == nil {
		return "", false
	}
	payload, err := json.Marshal(pushPayload{Title: testPushTitle, Body: testPushBody})
	if err != nil {
		return "", false
	}
	frame, err := s.sessions.EncryptFrame(deviceID, payload)
	if err != nil {
		return "", false
	}
	return base64.StdEncoding.EncodeToString(frame), true
}

// handleTestPushDevice lets a direct-mode-paired phone trigger one real,
// end-to-end push to itself -- docs/improvement-guide.md item 7.7. It sends
// through the exact same Pusher (s.pusher.Send) and, when e2e is configured,
// the exact same EncryptFrame path maybeSendPush uses for a real attention
// push; the only thing "test" about it is testPushTitle/testPushBody. Only
// ever mounted on DirectHandler (see direct.go): a relay-tunneled request
// has no real per-device identity to scope "itself" to -- handleTestPushEncrypt
// below is the relay-mode counterpart.
func (s *Server) handleTestPushDevice(w http.ResponseWriter, r *http.Request) {
	dev, ok := auth.DeviceFromContext(r.Context())
	if !ok {
		httpjson.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.pusher == nil {
		httpjson.Error(w, http.StatusServiceUnavailable, "push_not_configured")
		return
	}
	if dev.FCM == "" {
		httpjson.Error(w, http.StatusBadRequest, "device_not_registered_for_push")
		return
	}
	if !s.testPushCooldown.Allow(dev.TokenHash) {
		httpjson.Error(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	data := map[string]string{"type": "test", "slot": "direct"}
	if ciphertext, ok := s.buildTestPushCiphertext(dev.TokenHash); ok {
		data["e2e"] = ciphertext
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.pusher.Send(ctx, dev.FCM, "", "", data); err != nil {
		metrics.PushFailedTotal.Add(1)
		push.DropDeadToken(s.store, dev.FCM, err)
		slog.Warn("agent: test push send failed", "tenant_id", dev.TenantID, "device", dev.HashSuffix, "err", err)
		httpjson.Error(w, http.StatusBadGateway, "push_send_failed")
		return
	}
	metrics.PushSentTotal.Add(1)
	slog.Info("agent: test push sent", "tenant_id", dev.TenantID, "device", dev.HashSuffix)
	httpjson.Write(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleTestPushEncrypt is the relay-mode counterpart of handleTestPushDevice:
// the relay (internal/relay/testpush.go's handleTestPush) holds the FCM
// credentials and the calling device's identity for a relay-paired device,
// but only this agent holds the e2e keys needed to encrypt real content for
// it, so the relay calls this over the already-established trusted tunnel to
// get back one ciphertext. It names an arbitrary target deviceID in its
// body rather than authenticating one via X-Device-ID, so it must sit in
// front of encryptionMiddleware's device-header gate -- see TrustedHandler's
// doc comment (trusted.go) for how that's arranged. Never reachable from
// DirectHandler or by an ordinary device request.
func (s *Server) handleTestPushEncrypt(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var rq wire.TestPushDeviceReq
	if err := json.NewDecoder(r.Body).Decode(&rq); err != nil || rq.DeviceID == "" {
		httpjson.Error(w, http.StatusBadRequest, "missing device_id")
		return
	}
	ciphertext, ok := s.buildTestPushCiphertext(rq.DeviceID)
	if !ok {
		httpjson.Error(w, http.StatusConflict, "not_paired")
		return
	}
	httpjson.Write(w, http.StatusOK, wire.TestPushDeviceResp{Ciphertext: ciphertext})
}
