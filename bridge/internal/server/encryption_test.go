package server

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sodre90/term-bridge/internal/cmux"
	"github.com/sodre90/term-bridge/internal/e2e"
	"github.com/sodre90/term-bridge/internal/testutil"
)

// pairedSessions returns an e2e.Store with one device ("dev1-token-hash")
// already paired, plus its deviceID key and shared secret — mirroring how
// `term-bridge pair-device` (Task 15) populates the real store, without
// depending on that CLI.
func pairedSessions(t *testing.T) (sessions *e2e.Store, deviceID string, secret []byte) {
	t.Helper()
	agentPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate agent key: %v", err)
	}
	devicePriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate device key: %v", err)
	}
	secret, err = e2e.DeriveSharedSecret(agentPriv, devicePriv.PublicKey())
	if err != nil {
		t.Fatalf("DeriveSharedSecret: %v", err)
	}
	sessions, err = e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	deviceID = "dev1-token-hash"
	if err := sessions.AddDevice(deviceID, devicePriv.PublicKey(), secret); err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	return sessions, deviceID, secret
}

func TestEncryptionMiddlewarePassesThroughWhenSessionsNil(t *testing.T) {
	s := &Server{}
	called := false
	h := s.encryptionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("plain"))
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))
	if !called || rr.Body.String() != "plain" {
		t.Fatalf("expected untouched pass-through, got body=%q called=%v", rr.Body.String(), called)
	}
}

func TestEncryptionMiddlewareRejectsMissingDeviceID(t *testing.T) {
	sessions, _, _ := pairedSessions(t)
	s := &Server{sessions: sessions}
	h := s.encryptionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not run without a valid X-Device-ID")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
	if got := rr.Header().Get("X-Cmux-Encrypted"); got != "" {
		t.Fatalf("plaintext error must have no X-Cmux-Encrypted marker, got %q", got)
	}
}

func TestEncryptionMiddlewareRejectsUnknownDeviceID(t *testing.T) {
	// A present-but-unrecognized device id (e.g. this agent's local e2e
	// state was wiped) gets 409, distinct from the 401 for a wholly missing
	// header -- per the spec's error-handling section, the two should point
	// the app at different recovery UX (re-check auth vs. re-pair).
	sessions, _, _ := pairedSessions(t)
	s := &Server{sessions: sessions}
	h := s.encryptionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not run for an unrecognized device id")
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Device-ID", "nope")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
	if got := rr.Header().Get("X-Cmux-Encrypted"); got != "" {
		t.Fatalf("plaintext error must have no X-Cmux-Encrypted marker, got %q", got)
	}
}

func TestEncryptionMiddlewareDecryptsRequestEncryptsResponse(t *testing.T) {
	sessions, deviceID, secret := pairedSessions(t)
	s := &Server{sessions: sessions}
	var sawPlaintext []byte
	h := s.encryptionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		sawPlaintext, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("handler read body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	plaintextReq := []byte(`{"hello":"world"}`)
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

	req := httptest.NewRequest("POST", "/x", bytes.NewReader(envelope))
	req.Header.Set("X-Device-ID", deviceID)
	req.ContentLength = int64(len(envelope))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if string(sawPlaintext) != string(plaintextReq) {
		t.Fatalf("handler saw %q, want %q", sawPlaintext, plaintextReq)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if rr.Header().Get("X-Cmux-Encrypted") != "1" {
		t.Fatalf("want X-Cmux-Encrypted: 1, got %q", rr.Header().Get("X-Cmux-Encrypted"))
	}
	if strings.Contains(rr.Body.String(), `"ok"`) {
		t.Fatalf("response body must be encrypted, not plaintext: %s", rr.Body.String())
	}
	// The response was sealed by Store.EncryptBody, which always encrypts in
	// the DirAgentToDevice direction (see internal/e2e/envelope.go) -- the
	// agent's own Store.DecryptBody only ever opens DirDeviceToAgent
	// messages, so decrypting this response (as the paired device would)
	// means opening it directly rather than round-tripping through
	// DecryptBody. This mirrors TestEncryptBodyDeviceCanDecrypt in
	// internal/e2e/envelope_test.go and TestTrustedHandlerEncryptsSessionsWhenSessionsSet below.
	var respEnv struct {
		V  int    `json:"v"`
		N  uint64 `json:"n"`
		CT string `json:"ct"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &respEnv); err != nil {
		t.Fatalf("unmarshal response envelope: %v", err)
	}
	respCT, err := base64.StdEncoding.DecodeString(respEnv.CT)
	if err != nil {
		t.Fatalf("decode response ct: %v", err)
	}
	respPlain, err := e2e.Open(secret, e2e.Nonce(e2e.DirAgentToDevice, respEnv.N), respCT)
	if err != nil {
		t.Fatalf("decrypt response: %v", err)
	}
	if string(respPlain) != `{"ok":true}` {
		t.Fatalf("decrypted response = %q", respPlain)
	}
}

func TestEncryptionMiddlewareSkipsWebSocketUpgrade(t *testing.T) {
	sessions, deviceID, _ := pairedSessions(t)
	s := &Server{sessions: sessions}
	called := false
	h := s.encryptionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/terminal/x", nil)
	req.Header.Set("X-Device-ID", deviceID)
	req.Header.Set("Upgrade", "websocket")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !called {
		t.Fatal("a WebSocket upgrade request must reach the handler untouched, not be body-encrypted")
	}
}

// TestEncryptionMiddlewareEncryptsAndMarksNon2xxHandlerBody proves that a
// handler returning a non-2xx status still has its body encrypted and marked,
// which means status code alone cannot tell the client whether the body is
// encrypted -- only the X-Cmux-Encrypted marker can.
func TestEncryptionMiddlewareEncryptsAndMarksNon2xxHandlerBody(t *testing.T) {
	sessions, deviceID, secret := pairedSessions(t)
	s := &Server{sessions: sessions}
	h := s.encryptionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))

	plaintextReq := []byte(`{"hello":"world"}`)
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

	req := httptest.NewRequest("POST", "/x", bytes.NewReader(envelope))
	req.Header.Set("X-Device-ID", deviceID)
	req.ContentLength = int64(len(envelope))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
	if rr.Header().Get("X-Cmux-Encrypted") != "1" {
		t.Fatalf("want X-Cmux-Encrypted: 1 on non-2xx encrypted body, got %q", rr.Header().Get("X-Cmux-Encrypted"))
	}
	if strings.Contains(rr.Body.String(), `"error"`) {
		t.Fatalf("response body must be encrypted, not plaintext: %s", rr.Body.String())
	}
	var respEnv struct {
		V  int    `json:"v"`
		N  uint64 `json:"n"`
		CT string `json:"ct"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &respEnv); err != nil {
		t.Fatalf("unmarshal response envelope: %v", err)
	}
	respCT, err := base64.StdEncoding.DecodeString(respEnv.CT)
	if err != nil {
		t.Fatalf("decode response ct: %v", err)
	}
	respPlain, err := e2e.Open(secret, e2e.Nonce(e2e.DirAgentToDevice, respEnv.N), respCT)
	if err != nil {
		t.Fatalf("decrypt response: %v", err)
	}
	if string(respPlain) != `{"error":"nope"}` {
		t.Fatalf("decrypted response = %q, want {\"error\":\"nope\"}", respPlain)
	}
}

func TestTrustedHandlerEncryptsSessionsWhenSessionsSet(t *testing.T) {
	script := "#!/bin/sh\ncat <<'JSON'\n" + fakeWorkspaceList + "\nJSON\n"
	bin := testutil.WriteFakeCmux(t, script)
	s := New(&cmux.Client{Bin: bin}, nil)
	sessions, deviceID, secret := pairedSessions(t)
	s.SetSessions(sessions)

	const relayTok = "relay-secret"
	srv := httptest.NewServer(s.TrustedHandler(relayTok))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/sessions", nil)
	req.Header.Set("X-Relay-Token", relayTok)
	req.Header.Set("X-Device-ID", deviceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "workspaces") {
		t.Fatalf("response must be encrypted, saw plaintext: %s", raw)
	}
	var env struct {
		CT string `json:"ct"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	ct, err := base64.StdEncoding.DecodeString(env.CT)
	if err != nil {
		t.Fatalf("decode ct: %v", err)
	}
	plain, err := e2e.Open(secret, e2e.Nonce(e2e.DirAgentToDevice, 0), ct)
	if err != nil {
		t.Fatalf("decrypt response: %v", err)
	}
	if !strings.Contains(string(plain), "882CA6F0") {
		t.Fatalf("decrypted response missing expected workspace: %s", plain)
	}
}

func TestTrustedHandlerStillPlaintextWhenSessionsUnset(t *testing.T) {
	// Regression guard for the Global Constraint that encryption is strictly
	// opt-in: a TrustedHandler that never calls SetSessions must behave
	// exactly as before this feature existed.
	bin := testutil.WriteFakeCmux(t, "#!/bin/sh\necho '{}'\n")
	s := New(&cmux.Client{Bin: bin}, nil)
	const relayTok = "relay-secret"
	srv := httptest.NewServer(s.TrustedHandler(relayTok))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/sessions", nil)
	req.Header.Set("X-Relay-Token", relayTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "workspaces") {
		t.Fatalf("expected plaintext workspaces JSON, got: %s", raw)
	}
}
