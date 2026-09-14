package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/cmux"
	"github.com/sodre90/term-bridge/internal/e2e"
	"github.com/sodre90/term-bridge/internal/wire"
)

type fakePusher struct {
	mu    sync.Mutex
	calls []struct {
		token, title, body string
		data               map[string]string
	}
}

func (p *fakePusher) Send(_ context.Context, tok, title, body string, data map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, struct {
		token, title, body string
		data               map[string]string
	}{tok, title, body, data})
	return nil
}

func (p *fakePusher) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func TestPushTitlePrefersWorkspaceTitle(t *testing.T) {
	f := wire.EventFrame{Title: "cmux-app"}
	if got := f.PushTitle(); got != "cmux-app" {
		t.Fatalf("PushTitle() = %q, want %q", got, "cmux-app")
	}
}

func TestPushTitleFallsBackWhenNoTitle(t *testing.T) {
	f := wire.EventFrame{}
	if got := f.PushTitle(); got != "Agent needs your attention" {
		t.Fatalf("PushTitle() = %q, want the generic fallback", got)
	}
}

func TestPushBodyPrefersPreview(t *testing.T) {
	f := wire.EventFrame{Preview: "Claude needs your permission", Kind: "AskUserQuestion"}
	if got := f.PushBody(); got != "Claude needs your permission" {
		t.Fatalf("PushBody() = %q, want the frame's Preview", got)
	}
}

func TestPushBodyMapsKnownKindsWithoutPreview(t *testing.T) {
	cases := map[string]string{
		"AskUserQuestion": "Has a question for you",
		"Notification":    "Needs your attention",
		"":                "Open cmux to reply",
		"unknown-kind":    "Open cmux to reply",
	}
	for kind, want := range cases {
		if got := (wire.EventFrame{Kind: kind}).PushBody(); got != want {
			t.Fatalf("PushBody() for kind %q = %q, want %q", kind, got, want)
		}
	}
}

func newPushTestServer(t *testing.T) (*Server, *auth.Store) {
	t.Helper()
	store, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(&cmux.Client{}, store)
	return s, store
}

func TestMaybeSendPushNoopWithoutPusher(t *testing.T) {
	s, store := newPushTestServer(t)
	tenant, _ := store.CreateTenant()
	tok, _ := store.Issue(tenant, "phone", "test-pubkey-b64")
	store.SetFCMToken(tok, "fcm-token-1")
	// s.pusher is nil (SetPusher never called) -- must not panic, must not
	// look anything up.
	s.maybeSendPush(context.Background(), wire.EventFrame{NeedsAttention: true, Kind: "permissionRequest"})
}

func TestMaybeSendPushNoopWithoutStore(t *testing.T) {
	s := New(&cmux.Client{}, nil) // store nil: direct mode off
	fp := &fakePusher{}
	s.SetPusher(fp, "some-tenant")
	s.maybeSendPush(context.Background(), wire.EventFrame{NeedsAttention: true, Kind: "permissionRequest"})
	if fp.callCount() != 0 {
		t.Fatalf("must not call pusher when store is nil, got %d calls", fp.callCount())
	}
}

// TestMaybeSendPushSendsToEveryRegisteredToken covers the s.sessions == nil
// case -- newPushTestServer never calls SetSessions, so this proves the
// no-e2e-configured fallback: push still reaches every token, with no real
// content, and no "e2e" data key at all. TestMaybeSendPushSendsEncryptedPayload
// below covers the real, always-on-in-production configuration.
func TestMaybeSendPushSendsToEveryRegisteredToken(t *testing.T) {
	s, store := newPushTestServer(t)
	tenant, _ := store.CreateTenant()
	tok1, _ := store.Issue(tenant, "phone-1", "test-pubkey-1")
	tok2, _ := store.Issue(tenant, "phone-2", "test-pubkey-2")
	store.SetFCMToken(tok1, "fcm-1")
	store.SetFCMToken(tok2, "fcm-2")

	fp := &fakePusher{}
	s.SetPusher(fp, tenant)

	s.maybeSendPush(context.Background(), wire.EventFrame{
		NeedsAttention: true, FeedID: "F1", WorkspaceID: "W1", SurfaceID: "S1",
		Title: "cmux-app", Preview: "Claude needs your permission", Kind: "permissionRequest",
	})

	if fp.callCount() != 2 {
		t.Fatalf("want 2 push calls (one per registered token), got %d", fp.callCount())
	}
	got := map[string]bool{}
	for _, c := range fp.calls {
		got[c.token] = true
		if c.data["type"] != "attention" || c.data["feed_id"] != "F1" || c.data["workspace_id"] != "W1" || c.data["kind"] != "permissionRequest" || c.data["slot"] != "direct" {
			t.Fatalf("unexpected push data: %+v", c.data)
		}
		if _, ok := c.data["e2e"]; ok {
			t.Fatalf("no e2e session configured -- e2e key must be absent, got %+v", c.data)
		}
		if c.title != "" || c.body != "" {
			t.Fatalf("real content must never be passed to Send as plaintext, got title=%q body=%q", c.title, c.body)
		}
	}
	if !got["fcm-1"] || !got["fcm-2"] {
		t.Fatalf("expected both tokens to receive push, got calls: %+v", fp.calls)
	}
}

// TestMaybeSendPushSendsEncryptedPayload exercises the real, always-on
// production configuration (runAgent unconditionally calls SetSessions):
// maybeSendPush must forward only the device's own encrypted payload from
// f.EncryptedPush, and that payload must decrypt back to the frame's real
// title/body -- proving the wire format is exactly what CmuxMessagingService
// on Android expects to decrypt.
func TestMaybeSendPushSendsEncryptedPayload(t *testing.T) {
	s, store := newPushTestServer(t)
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetSessions(sessions)
	tok, secret := directPairedDevice(t, store, sessions)
	dev, err := store.Verify(tok)
	if err != nil {
		t.Fatalf("issued token should verify: %v", err)
	}
	store.SetFCMToken(tok, "fcm-1")

	fp := &fakePusher{}
	s.SetPusher(fp, dev.TenantID)

	f := wire.EventFrame{
		NeedsAttention: true, FeedID: "F1", WorkspaceID: "W1", SurfaceID: "S1",
		Title: "cmux-app", Preview: "Claude needs your permission", Kind: "permissionRequest",
	}
	f.EncryptedPush = s.buildEncryptedPush(f)

	s.maybeSendPush(context.Background(), f)

	if fp.callCount() != 1 {
		t.Fatalf("want 1 push call, got %d", fp.callCount())
	}
	c := fp.calls[0]
	if c.title != "" || c.body != "" {
		t.Fatalf("real content must never be passed to Send as plaintext, got title=%q body=%q", c.title, c.body)
	}
	blobB64, ok := c.data["e2e"]
	if !ok {
		t.Fatalf("expected an e2e data key, got %+v", c.data)
	}
	blob, err := base64.StdEncoding.DecodeString(blobB64)
	if err != nil {
		t.Fatalf("e2e blob not valid base64: %v", err)
	}
	_, plain, err := e2e.DecodeFrame(secret, e2e.DirAgentToDevice, blob)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	var got pushPayload
	if err := json.Unmarshal(plain, &got); err != nil {
		t.Fatalf("unmarshal decrypted payload: %v", err)
	}
	if got.Title != "cmux-app" || got.Body != "Claude needs your permission" {
		t.Fatalf("decrypted payload = %+v, want title=cmux-app body=Claude needs your permission", got)
	}
}

func TestMaybeSendPushNoopWithNoRegisteredTokens(t *testing.T) {
	s, store := newPushTestServer(t)
	tenant, _ := store.CreateTenant()
	fp := &fakePusher{}
	s.SetPusher(fp, tenant)

	s.maybeSendPush(context.Background(), wire.EventFrame{NeedsAttention: true, Kind: "permissionRequest"})

	if fp.callCount() != 0 {
		t.Fatalf("no tokens registered -- want 0 calls, got %d", fp.callCount())
	}
}

func TestMaybeSendPushScopesToOwnTenant(t *testing.T) {
	s, store := newPushTestServer(t)
	tenantA, _ := store.CreateTenant()
	tenantB, _ := store.CreateTenant()
	tokA, _ := store.Issue(tenantA, "phone-a", "test-pubkey-a")
	tokB, _ := store.Issue(tenantB, "phone-b", "test-pubkey-b")
	store.SetFCMToken(tokA, "fcm-a")
	store.SetFCMToken(tokB, "fcm-b")

	fp := &fakePusher{}
	s.SetPusher(fp, tenantA) // Server only knows about tenantA

	s.maybeSendPush(context.Background(), wire.EventFrame{NeedsAttention: true, Kind: "permissionRequest"})

	if fp.callCount() != 1 || fp.calls[0].token != "fcm-a" {
		t.Fatalf("push must be scoped to directTenantID only, got calls: %+v", fp.calls)
	}
}

func newDirectRegisterTestServer(t *testing.T) (*Server, *auth.Store) {
	t.Helper()
	store, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(&cmux.Client{}, store)
	return s, store
}

func TestHandleRegisterDeviceStoresToken(t *testing.T) {
	s, store := newDirectRegisterTestServer(t)
	tenant, _ := store.CreateTenant()
	tok, _ := store.Issue(tenant, "phone", "test-pubkey-b64")

	srv := httptest.NewServer(s.DirectHandler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/devices/register", strings.NewReader(`{"fcm_token":"fcm-abc"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	devices := store.TenantFCMDevices(tenant)
	if len(devices) != 1 || devices[0].FCMToken != "fcm-abc" {
		t.Fatalf("token not stored correctly: %+v", devices)
	}
}

func TestHandleRegisterDeviceRejectsMissingFCMToken(t *testing.T) {
	s, store := newDirectRegisterTestServer(t)
	tenant, _ := store.CreateTenant()
	tok, _ := store.Issue(tenant, "phone", "test-pubkey-b64")

	srv := httptest.NewServer(s.DirectHandler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/devices/register", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for missing fcm_token, got %d", resp.StatusCode)
	}
}

func TestHandleRegisterDeviceRejectsOversizedBody(t *testing.T) {
	s, store := newDirectRegisterTestServer(t)
	tenant, _ := store.CreateTenant()
	tok, _ := store.Issue(tenant, "phone", "test-pubkey-b64")

	srv := httptest.NewServer(s.DirectHandler())
	defer srv.Close()

	oversized := `{"fcm_token":"` + strings.Repeat("a", 5<<10) + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/devices/register", strings.NewReader(oversized))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for a body over the 4KB cap, got %d", resp.StatusCode)
	}

	devices := store.TenantFCMDevices(tenant)
	if len(devices) != 0 {
		t.Fatalf("oversized request must not be stored: %+v", devices)
	}
}

func TestHandleRegisterDeviceRejectsInvalidBearer(t *testing.T) {
	s, _ := newDirectRegisterTestServer(t)

	srv := httptest.NewServer(s.DirectHandler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/devices/register", strings.NewReader(`{"fcm_token":"fcm-abc"}`))
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 for an invalid bearer token, got %d", resp.StatusCode)
	}
}

func TestHandleRegisterDeviceNotMountedOnTrustedHandler(t *testing.T) {
	s, _ := newDirectRegisterTestServer(t)
	srv := httptest.NewServer(s.TrustedHandler("relay-secret"))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/devices/register", strings.NewReader(`{"fcm_token":"fcm-abc"}`))
	req.Header.Set("X-Relay-Token", "relay-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 -- /devices/register must not exist on the relay-tunneled handler, got %d", resp.StatusCode)
	}
}

// TestHandleRegisterDeviceAcceptsRealEncryptedEnvelope exercises
// /devices/register through DirectHandler with SetSessions wired up, the way
// runAgent's production configuration always has it (agent.go's runAgent
// unconditionally calls SetSessions with a real e2e.Store). Every other test
// above uses newDirectRegisterTestServer, which never calls SetSessions, so
// they only ever exercised the plaintext passthrough (s.sessions == nil) --
// a configuration direct mode never actually runs in. This test sends a
// genuinely e2e-encrypted envelope, the same way a correctly slot-aware
// Android E2eInterceptor now does on the DIRECT slot, and confirms the FCM
// token really lands in the auth store through the real encrypted pipeline.
func TestHandleRegisterDeviceAcceptsRealEncryptedEnvelope(t *testing.T) {
	authStore, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}

	tok, secret := directPairedDevice(t, authStore, sessions)
	dev, err := authStore.Verify(tok)
	if err != nil {
		t.Fatalf("issued token should verify: %v", err)
	}

	s := New(&cmux.Client{}, authStore)
	s.SetSessions(sessions)

	plaintextReq := []byte(`{"fcm_token":"fcm-real"}`)
	ct, err := e2e.Seal(secret, e2e.Nonce(e2e.DirDeviceToAgent, 0), plaintextReq)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	envelope, err := json.Marshal(struct {
		V  int    `json:"v"`
		N  uint64 `json:"n"`
		CT string `json:"ct"`
	}{V: 1, N: 0, CT: base64.StdEncoding.EncodeToString(ct)})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	srv := httptest.NewServer(s.DirectHandler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/devices/register", strings.NewReader(string(envelope)))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for a real e2e-encrypted /devices/register request, got %d", resp.StatusCode)
	}

	devices := authStore.TenantFCMDevices(dev.TenantID)
	if len(devices) != 1 || devices[0].FCMToken != "fcm-real" {
		t.Fatalf("fcm token did not make it through the encrypted pipeline into the auth store: %+v", devices)
	}
}
