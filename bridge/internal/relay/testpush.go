package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/httpjson"
	"github.com/sodre90/term-bridge/internal/metrics"
	pushpkg "github.com/sodre90/term-bridge/internal/push"
	"github.com/sodre90/term-bridge/internal/wire"
)

// handleTestPush lets a paired device trigger one real, end-to-end push to
// itself only -- docs/improvement-guide.md item 7.7: a 30-second round-trip
// check that push setup (FCM credentials + this device's own registration)
// actually works, instead of waiting to hit a real breakage. It sends
// through the exact same r.push.Send (the real push.Sender, wired in
// cmd/term-bridge-relay/serve.go) real attention pushes use, and the exact same
// per-device e2e encryption (via fetchTestPushCiphertext ->
// server.buildTestPushCiphertext, agent-side) real attention pushes use --
// the only thing "test" about it is the payload's title/body.
func (r *Relay) handleTestPush(w http.ResponseWriter, req *http.Request) {
	dev, ok := auth.DeviceFromContext(req.Context())
	if !ok {
		httpjson.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.push == nil {
		httpjson.Error(w, http.StatusServiceUnavailable, "push_not_configured")
		return
	}
	if dev.FCM == "" {
		httpjson.Error(w, http.StatusBadRequest, "device_not_registered_for_push")
		return
	}
	if !r.testPushCooldown.Allow(dev.TokenHash) {
		httpjson.Error(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	sess := r.reg.Get(dev.TenantID)
	if sess == nil {
		httpjson.Error(w, http.StatusServiceUnavailable, "agent_offline")
		return
	}

	ciphertext, err := fetchTestPushCiphertext(req.Context(), sess, r.relayToken, dev.TokenHash)
	if err != nil {
		slog.Warn("relay: test push encrypt failed", "tenant_id", dev.TenantID, "device", dev.HashSuffix, "err", err)
		httpjson.Error(w, http.StatusBadGateway, "agent_error")
		return
	}

	data := map[string]string{"type": "test", "slot": "relay"}
	if ciphertext != "" {
		data["e2e"] = ciphertext
	}
	sendCtx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	if err := r.push.Send(sendCtx, dev.FCM, "", "", data); err != nil {
		metrics.PushFailedTotal.Add(1)
		pushpkg.DropDeadToken(r.store, dev.FCM, err)
		slog.Warn("relay: test push send failed", "tenant_id", dev.TenantID, "device", dev.HashSuffix, "err", err)
		httpjson.Error(w, http.StatusBadGateway, "push_send_failed")
		return
	}
	metrics.PushSentTotal.Add(1)
	slog.Info("relay: test push sent", "tenant_id", dev.TenantID, "device", dev.HashSuffix)
	httpjson.Write(w, http.StatusOK, map[string]bool{"ok": true})
}

// fetchTestPushCiphertext asks the tenant's own agent, over its existing
// tunnel session, to build one e2e-encrypted test-push payload for deviceID
// -- the relay never holds the e2e keys needed to do this itself (it must
// stay blind to content). Mirrors pushmon.subscribeOnce's own direct tunnel
// dial, but as a single plain HTTP round trip instead of a long-lived
// websocket subscription. A 409 from the agent (e2e not configured, or this
// specific device isn't paired agent-side) is not treated as an error here:
// it returns ("", nil), same as buildEncryptedPush/maybeSendPush already
// treat missing e2e content as "send a generic push," not a failure.
func fetchTestPushCiphertext(ctx context.Context, sess *yamux.Session, relayToken, deviceID string) (string, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) { return sess.Open() },
		},
		Timeout: 10 * time.Second,
	}
	body, err := json.Marshal(wire.TestPushDeviceReq{DeviceID: deviceID})
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/internal/test-push-encrypt", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("X-Relay-Token", relayToken)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("agent test-push-encrypt: status %d", resp.StatusCode)
	}
	var out wire.TestPushDeviceResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Ciphertext, nil
}
