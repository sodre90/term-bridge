package relay

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/cmux"
	"github.com/sodre90/term-bridge/internal/e2e"
	"github.com/sodre90/term-bridge/internal/server"
	"github.com/sodre90/term-bridge/internal/tunnel"
)

// testPushHarness wires a full agent<->relay tunnel (mirroring
// TestRelayEndToEndSessions) plus a device paired end-to-end: a device
// bearer token issued by the relay's own auth.Store, and the SAME deviceID
// (that token's hash) paired into the agent's own e2e.Store -- exactly how a
// real device ends up known to both sides independently.
type testPushHarness struct {
	relayHTTP *httptest.Server
	rl        *Relay
	tenantID  string
	devTok    string
	devSecret []byte
	fp        *fakePusher
}

func newTestPushHarness(t *testing.T, connectAgent bool) *testPushHarness {
	t.Helper()
	agentSrv := server.New(&cmux.Client{}, nil)
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	agentSrv.SetSessions(sessions)
	const relayTok = "relay-secret"
	trusted := agentSrv.TrustedHandler(relayTok)

	relayStore, err := auth.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := relayStore.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}

	agentPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	devicePriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	devicePubB64 := base64.StdEncoding.EncodeToString(devicePriv.PublicKey().Bytes())
	devTok, err := relayStore.Issue(tenantID, "phone", devicePubB64)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := relayStore.Verify(devTok)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := e2e.DeriveSharedSecret(agentPriv, devicePriv.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.AddDevice(dev.TokenHash, devicePriv.PublicKey(), secret); err != nil {
		t.Fatal(err)
	}

	fp := &fakePusher{}
	rl := New(relayStore, nil, relayTok)
	rl.SetPusher(fp)
	relayHTTP := httptest.NewServer(rl.Handler())
	t.Cleanup(relayHTTP.Close)

	if connectAgent {
		u := "ws" + strings.TrimPrefix(relayHTTP.URL, "http") + "/agent/tunnel"
		sess, err := tunnel.Dial(context.Background(), u, nil, http.Header{
			"X-Client-Cert-Cn":     {"CN=agent:" + tenantID},
			"X-Client-Cert-Verify": {"SUCCESS"},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sess.Close() })
		go func() { _ = http.Serve(sess, trusted) }()
		waitFor(t, func() bool { return rl.reg.Get(tenantID) != nil })
	}

	return &testPushHarness{relayHTTP: relayHTTP, rl: rl, tenantID: tenantID, devTok: devTok, devSecret: secret, fp: fp}
}

func (h *testPushHarness) postTestPush(t *testing.T) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", h.relayHTTP.URL+"/devices/test-push", nil)
	req.Header.Set("Authorization", "Bearer "+h.devTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestRelayTestPushSendsRealEncryptedFCMToCallingDeviceOnly is the full
// relay-mode round trip: a device hits the relay's POST /devices/test-push,
// the relay fetches one real e2e ciphertext from the tenant's own agent over
// the live tunnel (fetchTestPushCiphertext -> agent's
// handleTestPushEncrypt), and sends it through the relay's own (fake, for
// this test) Pusher -- proving both halves of "reuse the real fanout/send
// path" the guide asks for: real EncryptFrame-produced ciphertext, real
// Pusher.Send call shape.
func TestRelayTestPushSendsRealEncryptedFCMToCallingDeviceOnly(t *testing.T) {
	h := newTestPushHarness(t, true)
	if err := h.rl.store.SetFCMToken(h.devTok, "fcm-relay-1"); err != nil {
		t.Fatal(err)
	}

	resp := h.postTestPush(t)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	if len(h.fp.calls) != 1 {
		t.Fatalf("want exactly 1 push call, got %d: %+v", len(h.fp.calls), h.fp.calls)
	}
	if h.fp.tokens[0] != "fcm-relay-1" {
		t.Fatalf("want push sent to the calling device's own token, got %q", h.fp.tokens[0])
	}
	data := h.fp.calls[0]
	if data["type"] != "test" || data["slot"] != "relay" {
		t.Fatalf("unexpected push data: %+v", data)
	}
	blobB64, ok := data["e2e"]
	if !ok {
		t.Fatalf("expected an e2e data key, got %+v", data)
	}
	blob, err := base64.StdEncoding.DecodeString(blobB64)
	if err != nil {
		t.Fatalf("e2e blob not valid base64: %v", err)
	}
	_, plain, err := e2e.DecodeFrame(h.devSecret, e2e.DirAgentToDevice, blob)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	var payload struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal(plain, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Title != "Test notification" {
		t.Fatalf("decrypted payload = %+v, want title %q", payload, "Test notification")
	}
}

func TestRelayTestPushRejectsWithoutPusherConfigured(t *testing.T) {
	h := newTestPushHarness(t, true)
	h.rl.push = nil // simulate FCM not configured, as SetPusher(nil) leaves it
	h.rl.store.SetFCMToken(h.devTok, "fcm-relay-1")

	resp := h.postTestPush(t)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503 push_not_configured, got %d", resp.StatusCode)
	}
}

func TestRelayTestPushRejectsDeviceWithNoFCMToken(t *testing.T) {
	h := newTestPushHarness(t, true)
	// No SetFCMToken call.

	resp := h.postTestPush(t)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 device_not_registered_for_push, got %d", resp.StatusCode)
	}
}

func TestRelayTestPushReportsAgentOfflineWhenTunnelDown(t *testing.T) {
	h := newTestPushHarness(t, false) // agent never dials in
	h.rl.store.SetFCMToken(h.devTok, "fcm-relay-1")

	resp := h.postTestPush(t)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503 agent_offline, got %d", resp.StatusCode)
	}
	if len(h.fp.calls) != 0 {
		t.Fatalf("must not call the pusher when the agent is offline, got %+v", h.fp.calls)
	}
}

func TestRelayTestPushRateLimitsRepeatedCalls(t *testing.T) {
	h := newTestPushHarness(t, true)
	h.rl.store.SetFCMToken(h.devTok, "fcm-relay-1")

	first := h.postTestPush(t)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first call: want 200, got %d", first.StatusCode)
	}
	second := h.postTestPush(t)
	second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second call within the cooldown: want 429, got %d", second.StatusCode)
	}
	if len(h.fp.calls) != 1 {
		t.Fatalf("rate-limited call must not reach the pusher, got %d calls", len(h.fp.calls))
	}
}

func TestRelayTestPushScopesToOwnTenantDevice(t *testing.T) {
	// A device from a different, unrelated tenant must never be able to
	// trigger (or receive) another tenant's test push -- auth.Require
	// already resolves dev.TenantID from the bearer token alone, so this
	// mostly documents that handleTestPush inherits that scoping rather than
	// trusting any caller-supplied tenant/device identifier.
	h := newTestPushHarness(t, true)
	h.rl.store.SetFCMToken(h.devTok, "fcm-relay-1")

	otherTenant, err := h.rl.store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	otherTok, err := h.rl.store.Issue(otherTenant, "phone-b", "unrelated-pubkey-b64")
	if err != nil {
		t.Fatal(err)
	}
	h.rl.store.SetFCMToken(otherTok, "fcm-other-tenant")

	req, _ := http.NewRequest("POST", h.relayHTTP.URL+"/devices/test-push", nil)
	req.Header.Set("Authorization", "Bearer "+otherTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// otherTenant has no agent tunnel connected in this harness, so this
	// must fail offline rather than somehow reach h's tenant's agent.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503 agent_offline for a tenant with no connected agent, got %d", resp.StatusCode)
	}
	for _, tok := range h.fp.tokens {
		if tok == "fcm-other-tenant" {
			t.Fatalf("push must never reach a different tenant's token: %+v", h.fp.tokens)
		}
	}
}
