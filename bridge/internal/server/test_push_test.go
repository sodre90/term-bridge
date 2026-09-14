package server

import (
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
	"github.com/sodre90/term-bridge/internal/wire"
)

// TestHandleTestPushDeviceSendsEncryptedPayloadToCallingDeviceOnly proves the
// direct-mode endpoint reuses the real Pusher.Send + EncryptFrame code paths
// (not a fake/plaintext shortcut) and scopes delivery to exactly the calling
// device's own FCM token.
func TestHandleTestPushDeviceSendsEncryptedPayloadToCallingDeviceOnly(t *testing.T) {
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
	if err := authStore.SetFCMToken(tok, "fcm-this-device"); err != nil {
		t.Fatal(err)
	}

	s := New(&cmux.Client{}, authStore)
	s.SetSessions(sessions)
	fp := &fakePusher{}
	s.SetPusher(fp, dev.TenantID)

	srv := httptest.NewServer(s.DirectHandler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/devices/test-push", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	if fp.callCount() != 1 {
		t.Fatalf("want exactly 1 push call, got %d", fp.callCount())
	}
	c := fp.calls[0]
	if c.token != "fcm-this-device" {
		t.Fatalf("want push sent to the calling device's own token, got %q", c.token)
	}
	if c.title != "" || c.body != "" {
		t.Fatalf("real content must never be passed to Send as plaintext, got title=%q body=%q", c.title, c.body)
	}
	if c.data["type"] != "test" || c.data["slot"] != "direct" {
		t.Fatalf("unexpected push data: %+v", c.data)
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
	if got.Title != testPushTitle || got.Body != testPushBody {
		t.Fatalf("decrypted payload = %+v, want title=%q body=%q", got, testPushTitle, testPushBody)
	}
}

func TestHandleTestPushDeviceRejectsWithoutPusherConfigured(t *testing.T) {
	authStore, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	tenant, _ := authStore.CreateTenant()
	tok, _ := authStore.Issue(tenant, "phone", "test-pubkey-b64")
	authStore.SetFCMToken(tok, "fcm-1")

	s := New(&cmux.Client{}, authStore) // s.pusher never set
	srv := httptest.NewServer(s.DirectHandler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/devices/test-push", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503 push_not_configured, got %d", resp.StatusCode)
	}
}

func TestHandleTestPushDeviceRejectsDeviceWithNoFCMToken(t *testing.T) {
	authStore, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	tenant, _ := authStore.CreateTenant()
	tok, _ := authStore.Issue(tenant, "phone", "test-pubkey-b64")
	// No SetFCMToken call: this device never completed /devices/register.

	s := New(&cmux.Client{}, authStore)
	s.SetPusher(&fakePusher{}, tenant)
	srv := httptest.NewServer(s.DirectHandler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/devices/test-push", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 device_not_registered_for_push, got %d", resp.StatusCode)
	}
}

func TestHandleTestPushDeviceRateLimitsRepeatedCalls(t *testing.T) {
	authStore, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	tenant, _ := authStore.CreateTenant()
	tok, _ := authStore.Issue(tenant, "phone", "test-pubkey-b64")
	authStore.SetFCMToken(tok, "fcm-1")

	s := New(&cmux.Client{}, authStore)
	fp := &fakePusher{}
	s.SetPusher(fp, tenant)
	srv := httptest.NewServer(s.DirectHandler())
	defer srv.Close()

	post := func() int {
		req, _ := http.NewRequest("POST", srv.URL+"/devices/test-push", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := post(); got != http.StatusOK {
		t.Fatalf("first call: want 200, got %d", got)
	}
	if got := post(); got != http.StatusTooManyRequests {
		t.Fatalf("second call within the cooldown: want 429, got %d", got)
	}
	if fp.callCount() != 1 {
		t.Fatalf("rate-limited call must not reach the pusher, got %d calls", fp.callCount())
	}
}

func TestHandleTestPushDeviceNotMountedOnTrustedHandler(t *testing.T) {
	authStore, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(&cmux.Client{}, authStore)
	srv := httptest.NewServer(s.TrustedHandler("relay-secret"))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/devices/test-push", nil)
	req.Header.Set("X-Relay-Token", "relay-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 -- /devices/test-push must not exist on the relay-tunneled handler, got %d", resp.StatusCode)
	}
}

func TestHandleTestPushEncryptReturnsCiphertextForPairedDevice(t *testing.T) {
	s := New(&cmux.Client{}, nil)
	sessions, deviceID, secret := pairedSessions(t)
	s.SetSessions(sessions)

	srv := httptest.NewServer(s.TrustedHandler("relay-secret"))
	defer srv.Close()

	body, _ := json.Marshal(wire.TestPushDeviceReq{DeviceID: deviceID})
	req, _ := http.NewRequest("POST", srv.URL+"/internal/test-push-encrypt", strings.NewReader(string(body)))
	req.Header.Set("X-Relay-Token", "relay-secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out wire.TestPushDeviceResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	blob, err := base64.StdEncoding.DecodeString(out.Ciphertext)
	if err != nil {
		t.Fatalf("ciphertext not valid base64: %v", err)
	}
	_, plain, err := e2e.DecodeFrame(secret, e2e.DirAgentToDevice, blob)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	var got pushPayload
	if err := json.Unmarshal(plain, &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != testPushTitle || got.Body != testPushBody {
		t.Fatalf("decrypted payload = %+v, want title=%q body=%q", got, testPushTitle, testPushBody)
	}
}

func TestHandleTestPushEncryptRejectsUnpairedDevice(t *testing.T) {
	s := New(&cmux.Client{}, nil)
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetSessions(sessions)

	srv := httptest.NewServer(s.TrustedHandler("relay-secret"))
	defer srv.Close()

	body, _ := json.Marshal(wire.TestPushDeviceReq{DeviceID: "never-paired"})
	req, _ := http.NewRequest("POST", srv.URL+"/internal/test-push-encrypt", strings.NewReader(string(body)))
	req.Header.Set("X-Relay-Token", "relay-secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409 not_paired, got %d", resp.StatusCode)
	}
}

func TestHandleTestPushEncryptRequiresRelayToken(t *testing.T) {
	s := New(&cmux.Client{}, nil)
	sessions, deviceID, _ := pairedSessions(t)
	s.SetSessions(sessions)

	srv := httptest.NewServer(s.TrustedHandler("relay-secret"))
	defer srv.Close()

	body, _ := json.Marshal(wire.TestPushDeviceReq{DeviceID: deviceID})
	req, _ := http.NewRequest("POST", srv.URL+"/internal/test-push-encrypt", strings.NewReader(string(body)))
	// No X-Relay-Token header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 without a relay token, got %d", resp.StatusCode)
	}
}

func TestHandleTestPushEncryptNotMountedOnDirectHandler(t *testing.T) {
	authStore, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(&cmux.Client{}, authStore)
	srv := httptest.NewServer(s.DirectHandler())
	defer srv.Close()

	body, _ := json.Marshal(wire.TestPushDeviceReq{DeviceID: "whatever"})
	req, _ := http.NewRequest("POST", srv.URL+"/internal/test-push-encrypt", strings.NewReader(string(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 -- /internal/test-push-encrypt must not exist on the direct handler, got %d", resp.StatusCode)
	}
}
