package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/wire"
)

// fcm is variadic so the existing cases keep exercising the zero value --
// the "push not configured" shape -- while the FCM cases pass a real one.
func newDirectPairingServer(t *testing.T, fcm ...wire.FCMClientConfig) (srv *httptest.Server, store *auth.Store, tenantID string) {
	t.Helper()
	store, err := auth.Open(filepath.Join(t.TempDir(), "direct-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err = store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	var cfg wire.FCMClientConfig
	if len(fcm) > 0 {
		cfg = fcm[0]
	}
	mux := http.NewServeMux()
	MountDirectPairing(mux, store, tenantID, cfg)
	return httptest.NewServer(mux), store, tenantID
}

func TestDirectNewPairingCodeIssuesCode(t *testing.T) {
	srv, _, tenantID := newDirectPairingServer(t)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/agent/pairing-code", "application/json", strings.NewReader(`{"agent_pubkey":"agent-pubkey-b64"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var body wire.PairingCodeResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code == "" || body.ExpiresAt == "" {
		t.Fatalf("unexpected body: %+v", body)
	}
	if body.TenantID != tenantID {
		t.Fatalf("TenantID = %q, want %q", body.TenantID, tenantID)
	}
}

func TestDirectNewPairingCodeRejectsMissingAgentPubkey(t *testing.T) {
	srv, _, _ := newDirectPairingServer(t)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/agent/pairing-code", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestDirectPairingCodeStatusPendingThenRedeemed(t *testing.T) {
	srv, store, tenantID := newDirectPairingServer(t)
	defer srv.Close()

	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	poll := func() wire.PairingCodeStatusResp {
		resp, err := http.Get(srv.URL + "/agent/pairing-code/" + code)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", resp.StatusCode)
		}
		var body wire.PairingCodeStatusResp
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	if got := poll(); got.Redeemed {
		t.Fatalf("code should not be redeemed yet: %+v", got)
	}

	tok, _, ok := store.RedeemPairingCode(code, "phone", "device-pubkey-b64")
	if !ok || tok == "" {
		t.Fatal("redeem should succeed")
	}

	got := poll()
	if !got.Redeemed || got.DevicePubkey != "device-pubkey-b64" {
		t.Fatalf("unexpected status after redeem: %+v", got)
	}
}

func TestDirectPairingCodeStatusUnknownCodeIs404(t *testing.T) {
	srv, _, _ := newDirectPairingServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/agent/pairing-code/bogus")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestDirectPairingCodeInfoReturnsAgentPubkey(t *testing.T) {
	srv, store, tenantID := newDirectPairingServer(t)
	defer srv.Close()

	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/devices/pair-info/" + code)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var body wire.PairingCodeInfoResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.AgentPubkey != "agent-pubkey-b64" || body.TenantID != tenantID {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestDirectPairingCodeInfoRejectsUnknownCode(t *testing.T) {
	srv, _, _ := newDirectPairingServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/devices/pair-info/bogus")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("want 410, got %d", resp.StatusCode)
	}
}

func TestDirectDevicePairRedeemsCode(t *testing.T) {
	srv, store, tenantID := newDirectPairingServer(t)
	defer srv.Close()

	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	body := `{"code":"` + code + `","device_pubkey":"device-pubkey-b64","name":"my-phone"}`
	resp, err := http.Post(srv.URL+"/devices/pair", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var got wire.DevicePairResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Token == "" || got.TenantID != tenantID {
		t.Fatalf("unexpected response: %+v", got)
	}
	dev, err := store.Verify(got.Token)
	if err != nil || dev.Name != "my-phone" || dev.DevicePubkey != "device-pubkey-b64" {
		t.Fatalf("unexpected device: %+v (err=%v)", dev, err)
	}
}

// TestDirectDevicePairNotRateLimited asserts direct mode's /devices/pair
// stays unthrottled (pairing.AllowAll) unlike the relay's copy -- this
// listener only binds this Mac's own Tailscale IPv4 address, so it's never
// internet-reachable the way the relay's is; Tailscale's own network ACLs
// are its access-control boundary.
func TestDirectDevicePairNotRateLimited(t *testing.T) {
	srv, store, tenantID := newDirectPairingServer(t)
	defer srv.Close()

	pair := func() int {
		code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		body := `{"code":"` + code + `","device_pubkey":"device-pubkey-b64","name":"my-phone"}`
		resp, err := http.Post(srv.URL+"/devices/pair", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := pair(); got != http.StatusOK {
		t.Fatalf("first pairing attempt: want 200, got %d", got)
	}
	if got := pair(); got != http.StatusOK {
		t.Fatalf("second immediate pairing attempt: want 200 (direct mode is unthrottled), got %d", got)
	}
}

func TestDirectDevicePairRejectsUnknownCode(t *testing.T) {
	srv, _, _ := newDirectPairingServer(t)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/devices/pair", "application/json", strings.NewReader(`{"code":"bogus","device_pubkey":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("want 410, got %d", resp.StatusCode)
	}
}

// The direct listener mints tokens on redemption exactly as the relay does,
// so the operator's refusal has to take them back here too (cmux-app-af1).
// Direct mode's abort is reachable without a client cert -- pairing.
// ConstantTenant resolves every caller to this Mac's one tenant, and
// Tailscale's ACLs are this listener's boundary.
func TestDirectAbortPairingRevokesTheRedeemedToken(t *testing.T) {
	srv, store, tenantID := newDirectPairingServer(t)
	defer srv.Close()

	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, ok := store.RedeemPairingCode(code, "phone", "device-pubkey-b64")
	if !ok {
		t.Fatal("redeem should succeed")
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/agent/pairing-code/"+code, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var body wire.AbortPairingResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Revoked {
		t.Fatal("want revoked=true for a pairing that had already been redeemed")
	}
	if _, err := store.Verify(tok); err == nil {
		t.Fatal("a refused pairing's token still verifies")
	}
}

func TestDirectAbortPairingUnknownCodeIs404(t *testing.T) {
	srv, _, _ := newDirectPairingServer(t)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/agent/pairing-code/bogus", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

// cmux-app-gmo. Both mounts serve the confirmation routes byte-identically;
// direct mode's ConstantTenant resolves every caller to this Mac's one
// tenant, so there is no CN to present here.
func TestDirectConfirmPairingMovesTheStatusThePhoneIsWatching(t *testing.T) {
	srv, store, tenantID := newDirectPairingServer(t)
	defer srv.Close()

	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := store.RedeemPairingCode(code, "phone", "device-pubkey-b64"); !ok {
		t.Fatal("redeem should succeed")
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/agent/pairing-code/"+code+"/confirm", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm = %d, want 200", resp.StatusCode)
	}

	statusResp, err := http.Get(srv.URL + "/devices/pair-status/" + code)
	if err != nil {
		t.Fatal(err)
	}
	defer statusResp.Body.Close()
	var body wire.PairStatusResp
	if err := json.NewDecoder(statusResp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.State != auth.PairingConfirmed {
		t.Fatalf("state = %q, want confirmed", body.State)
	}
}

func TestDirectConfirmPairingUnknownCodeIs404(t *testing.T) {
	srv, _, _ := newDirectPairingServer(t)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/agent/pairing-code/bogus/confirm", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

// redeemForFCM runs one full redemption against srv and returns the decoded
// response, so the FCM cases differ only in how the server was configured.
func redeemForFCM(t *testing.T, srv *httptest.Server, store *auth.Store, tenantID string) wire.DevicePairResp {
	t.Helper()
	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"code":"` + code + `","device_pubkey":"device-pubkey-b64","name":"my-phone"}`
	resp, err := http.Post(srv.URL+"/devices/pair", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var got wire.DevicePairResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDirectDevicePairCarriesFCMClientConfig(t *testing.T) {
	want := wire.FCMClientConfig{
		ProjectID: "my-project",
		AppID:     "1:1234567890:android:abcdef",
		APIKey:    "AIzaSyExample",
		SenderID:  "1234567890",
	}
	srv, store, tenantID := newDirectPairingServer(t, want)
	defer srv.Close()

	got := redeemForFCM(t, srv, store, tenantID)
	if got.FCM == nil {
		t.Fatal("a fully configured bridge must hand the phone its FCM client config")
	}
	if *got.FCM != want {
		t.Fatalf("FCM config = %+v, want %+v", *got.FCM, want)
	}
}

// Without push configured the response must be byte-identical to what it was
// before this field existed: a phone on an older build decodes it unchanged,
// and a current one must read "no push here" rather than a partial config it
// would only fail to initialise Firebase with.
//
// Asserted on the raw body, not a decoded struct: decoding cannot tell an
// absent key from "fcm":null, and absent is what the claim actually is.
func TestDirectDevicePairOmitsFCMWhenUnconfigured(t *testing.T) {
	srv, store, tenantID := newDirectPairingServer(t)
	defer srv.Close()

	body := redeemRawForFCM(t, srv, store, tenantID)
	if strings.Contains(body, "fcm") {
		t.Fatalf("unconfigured bridge must not mention fcm at all, got %s", body)
	}
}

// redeemRawForFCM is redeemForFCM's undecoded twin, for the assertions that
// are about the bytes rather than the values.
func redeemRawForFCM(t *testing.T, srv *httptest.Server, store *auth.Store, tenantID string) string {
	t.Helper()
	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"code":"` + code + `","device_pubkey":"device-pubkey-b64","name":"my-phone"}`
	resp, err := http.Post(srv.URL+"/devices/pair", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A half-filled config is the dangerous case: Firebase rejects partial
// options, so sending three of four fields would hand the phone something it
// can only crash on. It must read as "not configured" instead.
func TestDirectDevicePairOmitsPartialFCMConfig(t *testing.T) {
	partial := wire.FCMClientConfig{
		ProjectID: "my-project",
		AppID:     "1:1234567890:android:abcdef",
		SenderID:  "1234567890",
		// APIKey deliberately absent.
	}
	srv, store, tenantID := newDirectPairingServer(t, partial)
	defer srv.Close()

	if got := redeemForFCM(t, srv, store, tenantID); got.FCM != nil {
		t.Fatalf("partial config must be withheld, got %+v", *got.FCM)
	}
}
