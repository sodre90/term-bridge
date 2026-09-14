package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sodre90/term-bridge/internal/e2e"
)

func testDevicePubkeyB64(t *testing.T) string {
	t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate device key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes())
}

// fakeRelay is the agent-facing half of the pairing protocol, standing in
// for a relay. Counters are read with atomic.LoadInt32 -- the handlers run on
// the server's own goroutines.
type fakeRelay struct {
	*httptest.Server
	polls    int32
	aborts   int32
	confirms int32
	// confirmStatus is what the confirm route answers, so a test can make the
	// last step of pairing fail. Set before pairDevice starts; 0 means 200.
	confirmStatus int32
	// beforeConfirm runs inside the confirm handler, which is how a test
	// observes what the agent had already done by the time it confirmed.
	beforeConfirm func()
}

// fakePairingRelay serves the agent-facing pairing endpoints pairDevice
// calls. The GET poll handler returns 500 for the first failFirstN polls
// (simulating a transient relay hiccup), reports "redeemed":false until the
// redeemAfter'th poll, and "redeemed":true from then on. polls counts total
// GET calls for the test to assert retry/poll-count behavior; aborts counts
// DELETEs, which is how a test sees the rollback of a refused pairing, and
// confirms counts the operator's yes reaching the server.
func fakePairingRelay(t *testing.T, code, devicePubkeyB64 string, redeemAfter, failFirstN int) *fakeRelay {
	t.Helper()
	rl := &fakeRelay{}
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /agent/pairing-code/"+code, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rl.aborts, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"revoked": true})
	})
	mux.HandleFunc("POST /agent/pairing-code/"+code+"/confirm", func(w http.ResponseWriter, r *http.Request) {
		if rl.beforeConfirm != nil {
			rl.beforeConfirm()
		}
		atomic.AddInt32(&rl.confirms, 1)
		if status := int(atomic.LoadInt32(&rl.confirmStatus)); status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	})
	mux.HandleFunc("POST /agent/pairing-code", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "expires_at": "2099-01-01T00:00:00Z", "tenant_id": "fake-tenant-id"})
	})
	mux.HandleFunc("GET /agent/pairing-code/"+code, func(w http.ResponseWriter, r *http.Request) {
		n := int(atomic.AddInt32(&rl.polls, 1))
		if n <= failFirstN {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if n < redeemAfter {
			_ = json.NewEncoder(w).Encode(map[string]any{"redeemed": false})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"redeemed":      true,
			"device_pubkey": devicePubkeyB64,
			"token_hash":    "fake-token-hash",
		})
	})
	rl.Server = httptest.NewServer(mux)
	t.Cleanup(rl.Close)
	return rl
}

func TestPairDeviceStopsOnRedemption(t *testing.T) {
	devicePub := testDevicePubkeyB64(t)
	rl := fakePairingRelay(t, "CODE1234", devicePub, 2, 0)

	identity, err := e2e.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "identity.key"))
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	err = pairDevice(rl.Client(), rl.URL, "https://phone.example/devices/pair", identity, sessions, io.Discard, strings.NewReader("y\n"), 10*time.Millisecond, time.Now().Add(2*time.Second))
	if err != nil {
		t.Fatalf("pairDevice: %v", err)
	}
	if got := atomic.LoadInt32(&rl.polls); got < 2 {
		t.Fatalf("expected pairDevice to poll at least twice before redemption, got %d", got)
	}
	if _, ok := sessions.SharedSecret("fake-token-hash"); !ok {
		t.Fatal("expected pairDevice to persist a shared secret for the redeemed device")
	}
}

func TestPairDeviceAbortsOnRejectedFingerprint(t *testing.T) {
	devicePub := testDevicePubkeyB64(t)
	rl := fakePairingRelay(t, "CODE1234", devicePub, 2, 0)

	identity, err := e2e.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "identity.key"))
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	err = pairDevice(rl.Client(), rl.URL, "https://phone.example/devices/pair", identity, sessions, io.Discard, strings.NewReader("n\n"), 10*time.Millisecond, time.Now().Add(2*time.Second))
	if err == nil {
		t.Fatal("expected pairDevice to abort when the fingerprint is rejected")
	}
	if _, ok := sessions.SharedSecret("fake-token-hash"); ok {
		t.Fatal("expected pairDevice not to persist a shared secret when the fingerprint is rejected")
	}
	// cmux-app-af1: redemption already handed the phone a bearer token, so
	// declining to store the e2e half isn't enough -- that token has to be
	// destroyed, or a refused pairing still leaves a working credential.
	if got := atomic.LoadInt32(&rl.aborts); got != 1 {
		t.Fatalf("expected the refused pairing to be revoked exactly once, got %d aborts", got)
	}
	// cmux-app-gmo: the phone is waiting on the confirm, so a refusal must
	// not send one -- otherwise the phone persists a pairing the operator
	// just rejected.
	if got := atomic.LoadInt32(&rl.confirms); got != 0 {
		t.Fatalf("a refused pairing confirmed itself %d times", got)
	}
}

func TestPairDeviceConfirmFingerprintFailsClosed(t *testing.T) {
	devicePub := testDevicePubkeyB64(t)
	rl := fakePairingRelay(t, "CODE1234", devicePub, 2, 0)

	identity, err := e2e.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "identity.key"))
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	err = pairDevice(rl.Client(), rl.URL, "https://phone.example/devices/pair", identity, sessions, io.Discard, strings.NewReader(""), 10*time.Millisecond, time.Now().Add(2*time.Second))
	if err == nil {
		t.Fatal("expected pairDevice to abort when confirmation input hits EOF")
	}
	if _, ok := sessions.SharedSecret("fake-token-hash"); ok {
		t.Fatal("expected pairDevice not to persist a shared secret when confirmation input hits EOF")
	}
	if got := atomic.LoadInt32(&rl.aborts); got != 1 {
		t.Fatalf("failing closed has to revoke the token too, not just skip the session; got %d aborts", got)
	}
}

func TestPairDeviceRetriesOnTransientError(t *testing.T) {
	devicePub := testDevicePubkeyB64(t)
	// Poll #1 and #2 return 500; poll #3 reports redeemed.
	rl := fakePairingRelay(t, "CODE1234", devicePub, 1, 2)

	identity, err := e2e.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "identity.key"))
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	err = pairDevice(rl.Client(), rl.URL, "https://phone.example/devices/pair", identity, sessions, io.Discard, strings.NewReader("y\n"), 10*time.Millisecond, time.Now().Add(2*time.Second))
	if err != nil {
		t.Fatalf("pairDevice: %v", err)
	}
	if got := atomic.LoadInt32(&rl.polls); got < 3 {
		t.Fatalf("expected pairDevice to survive 2 transient errors and keep polling, got %d polls", got)
	}
}

func TestPairDeviceTimesOut(t *testing.T) {
	devicePub := testDevicePubkeyB64(t)
	// redeemAfter is unreachably far given the short deadline below.
	rl := fakePairingRelay(t, "CODE1234", devicePub, 1000000, 0)

	identity, err := e2e.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "identity.key"))
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	err = pairDevice(rl.Client(), rl.URL, "https://phone.example/devices/pair", identity, sessions, io.Discard, strings.NewReader("y\n"), 10*time.Millisecond, time.Now().Add(50*time.Millisecond))
	if err == nil {
		t.Fatal("expected pairDevice to time out")
	}
}

// A rollback that didn't happen is the dangerous case: the phone is still
// holding a live token and only the operator can now get rid of it, so the
// failure has to reach them instead of being folded into the ordinary
// "pairing aborted" message.
func TestAbandonPairingEscalatesAFailedRevoke(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := abandonPairing(srv.Client(), srv.URL, "CODE1234", "pairing aborted", io.Discard)
	if err == nil {
		t.Fatal("expected an error when the revoke call fails")
	}
	if !strings.Contains(err.Error(), "FAILED") {
		t.Fatalf("error must say the token is still live, got %q", err)
	}
}

func TestAbandonPairingReportsASuccessfulRevoke(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"revoked": true})
	}))
	defer srv.Close()

	var out strings.Builder
	err := abandonPairing(srv.Client(), srv.URL, "CODE1234", "pairing aborted", &out)
	if err == nil || err.Error() != "pairing aborted" {
		t.Fatalf("the abort itself is still an error, and the reason must survive: %v", err)
	}
	if !strings.Contains(out.String(), "Revoked") {
		t.Fatalf("operator was not told the token was destroyed, got %q", out.String())
	}
}

func TestHttpsBaseFromRelayURL(t *testing.T) {
	cases := map[string]string{
		"wss://cmux.example.com/agent/tunnel": "https://cmux.example.com",
		"ws://localhost:8765/agent/tunnel":    "http://localhost:8765",
	}
	for in, want := range cases {
		got, err := httpsBaseFromRelayURL(in)
		if err != nil {
			t.Fatalf("httpsBaseFromRelayURL(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("httpsBaseFromRelayURL(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := httpsBaseFromRelayURL("not-a-url-with-bad-scheme://x"); err == nil {
		t.Fatal("expected an error for a non-ws(s) scheme")
	}
}

// cmux-app-gmo. The confirm is what releases the phone from holding its
// pairing in memory, so it has to happen exactly once and only after the
// agent has everything it needs to serve that phone.
func TestPairDeviceConfirmsOnceAfterPersistingTheSession(t *testing.T) {
	devicePub := testDevicePubkeyB64(t)
	rl := fakePairingRelay(t, "CODE1234", devicePub, 2, 0)

	identity, err := e2e.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "identity.key"))
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	var sessionReadyAtConfirm int32
	rl.beforeConfirm = func() {
		if _, ok := sessions.SharedSecret("fake-token-hash"); ok {
			atomic.StoreInt32(&sessionReadyAtConfirm, 1)
		}
	}

	err = pairDevice(rl.Client(), rl.URL, "https://phone.example/devices/pair", identity, sessions, io.Discard, strings.NewReader("y\n"), 10*time.Millisecond, time.Now().Add(2*time.Second))
	if err != nil {
		t.Fatalf("pairDevice: %v", err)
	}
	if got := atomic.LoadInt32(&rl.confirms); got != 1 {
		t.Fatalf("want exactly 1 confirm, got %d", got)
	}
	if got := atomic.LoadInt32(&rl.aborts); got != 0 {
		t.Fatalf("a successful pairing aborted %d times", got)
	}
	if atomic.LoadInt32(&sessionReadyAtConfirm) != 1 {
		t.Fatal("the agent confirmed before it could decrypt anything from that phone")
	}
}

// A pairing the agent cannot persist must reach the phone as a refusal, not
// as a confirm it will act on nor as a silence it waits out.
func TestPairDeviceAbortsWithoutConfirmingWhenTheSessionCannotBeStored(t *testing.T) {
	devicePriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	devicePub := base64.StdEncoding.EncodeToString(devicePriv.PublicKey().Bytes())
	rl := fakePairingRelay(t, "CODE1234", devicePub, 2, 0)

	identity, err := e2e.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "identity.key"))
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	// Claim this phone's shared secret under a different device id, which is
	// what AddDevice's reuse guard exists to reject.
	secret, err := e2e.DeriveSharedSecret(identity.Priv, devicePriv.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.AddDevice("some-earlier-token-hash", devicePriv.PublicKey(), secret); err != nil {
		t.Fatal(err)
	}

	err = pairDevice(rl.Client(), rl.URL, "https://phone.example/devices/pair", identity, sessions, io.Discard, strings.NewReader("y\n"), 10*time.Millisecond, time.Now().Add(2*time.Second))
	if err == nil {
		t.Fatal("expected pairDevice to fail when the session cannot be persisted")
	}
	if got := atomic.LoadInt32(&rl.confirms); got != 0 {
		t.Fatalf("a pairing that could not be stored confirmed itself %d times", got)
	}
	if got := atomic.LoadInt32(&rl.aborts); got != 1 {
		t.Fatalf("want exactly 1 abort, got %d", got)
	}
}

// If the confirm itself fails the phone would otherwise wait out its whole
// timeout, so the agent has to turn that into an explicit refusal -- and say
// so, since the operator has already answered yes and will be surprised.
func TestPairDeviceAbortsWhenTheConfirmFails(t *testing.T) {
	devicePub := testDevicePubkeyB64(t)
	rl := fakePairingRelay(t, "CODE1234", devicePub, 2, 0)
	atomic.StoreInt32(&rl.confirmStatus, http.StatusInternalServerError)

	identity, err := e2e.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "identity.key"))
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	err = pairDevice(rl.Client(), rl.URL, "https://phone.example/devices/pair", identity, sessions, io.Discard, strings.NewReader("y\n"), 10*time.Millisecond, time.Now().Add(2*time.Second))
	if err == nil {
		t.Fatal("expected pairDevice to fail when the confirm call fails")
	}
	if !strings.Contains(err.Error(), "confirm pairing") {
		t.Fatalf("the error must name the failed confirm, got %q", err)
	}
	if got := atomic.LoadInt32(&rl.aborts); got != 1 {
		t.Fatalf("want exactly 1 abort, got %d", got)
	}
}
