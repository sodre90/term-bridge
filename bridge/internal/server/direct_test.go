package server

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/cmux"
	"github.com/sodre90/term-bridge/internal/e2e"
	"github.com/sodre90/term-bridge/internal/testutil"
)

// directPairedDevice issues a real bearer token via store, then pairs its
// e2e shared secret keyed by that token's real hash -- unlike
// encryption_test.go's pairedSessions (which uses a fixed fake deviceID),
// this proves the two stores agree on the SAME id the way DirectHandler's
// injectDeviceID actually produces it.
func directPairedDevice(t *testing.T, store *auth.Store, sessions *e2e.Store) (token string, secret []byte) {
	t.Helper()
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	agentPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate agent key: %v", err)
	}
	devicePriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate device key: %v", err)
	}
	devicePubB64 := base64.StdEncoding.EncodeToString(devicePriv.PublicKey().Bytes())
	tok, err := store.Issue(tenantID, "phone", devicePubB64)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	dev, err := store.Verify(tok)
	if err != nil {
		t.Fatalf("issued token should verify: %v", err)
	}
	secret, err = e2e.DeriveSharedSecret(agentPriv, devicePriv.PublicKey())
	if err != nil {
		t.Fatalf("DeriveSharedSecret: %v", err)
	}
	if err := sessions.AddDevice(dev.TokenHash, devicePriv.PublicKey(), secret); err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	return tok, secret
}

func TestDirectHandlerRejectsMissingBearerToken(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, "#!/bin/sh\necho '{}'\n")
	authStore, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(&cmux.Client{Bin: bin}, authStore)
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetSessions(sessions)

	srv := httptest.NewServer(s.DirectHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 with no bearer token, got %d", resp.StatusCode)
	}
}

func TestDirectHandlerOverwritesForgedDeviceIDHeader(t *testing.T) {
	// Adversarial: a real, currently-paired device sends a valid bearer
	// token but a forged X-Device-ID naming a DIFFERENT (nonexistent)
	// device, with its request body encrypted under ITS OWN real secret.
	// injectDeviceID must overwrite the header with the token's own real
	// hash before encryptionMiddleware ever sees it -- if the forged value
	// won, decryption would fail with 409, not succeed.
	script := "#!/bin/sh\ncat <<'JSON'\n" + fakeWorkspaceList + "\nJSON\n"
	bin := testutil.WriteFakeCmux(t, script)
	authStore, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(&cmux.Client{Bin: bin}, authStore)
	s.SetSessions(sessions)

	tok, _ := directPairedDevice(t, authStore, sessions)

	srv := httptest.NewServer(s.DirectHandler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Device-ID", "someone-elses-hash")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 (forged X-Device-ID must be overwritten by the verified token's own id), got %d", resp.StatusCode)
	}
}

func TestDirectHandlerRejectsUnpairedDevice(t *testing.T) {
	// A valid bearer token whose e2e shared secret was never registered
	// (e.g. auth succeeded but pairing's second half never completed) must
	// fail closed at the encryption layer, not fall back to plaintext.
	bin := testutil.WriteFakeCmux(t, "#!/bin/sh\necho '{}'\n")
	authStore, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(&cmux.Client{Bin: bin}, authStore)
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetSessions(sessions)

	tenantID, err := authStore.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := authStore.Issue(tenantID, "phone", "unpaired-device-pubkey-b64")
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.DirectHandler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409 not_paired, got %d", resp.StatusCode)
	}
}

// Self-revoke is mounted outside encryptionMiddleware on purpose: the phone
// calls it while clearing the very shared secret an envelope would need, so
// a plain request with no envelope must reach it and work. If somebody
// "fixes" the mount to use DirectHandler's wrap, this fails -- which is the
// point.
func TestDirectHandlerSelfRevokeNeedsNoEncryptedEnvelope(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, "#!/bin/sh\necho '{}'\n")
	authStore, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(&cmux.Client{Bin: bin}, authStore)
	s.SetSessions(sessions)
	tok, _ := directPairedDevice(t, authStore, sessions)

	srv := httptest.NewServer(s.DirectHandler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/devices/self-revoke", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Cmux-Encrypted") != "" {
		t.Fatal("this route is outside the e2e layer, so the response must not be marked encrypted")
	}
	if _, err := authStore.Verify(tok); err == nil {
		t.Fatal("the caller's token should be gone")
	}
}
