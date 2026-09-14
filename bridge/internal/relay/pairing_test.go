package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/wire"
)

func TestNewPairingCodeRequiresAgentCN(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/agent/pairing-code", nil)
	req.Header.Set("X-Client-Cert-Cn", "CN=phone")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 for a non-agent CN, got %d", resp.StatusCode)
	}
}

func TestDevicePairRedeemsCode(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

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
	if got.Token == "" {
		t.Fatal("expected a non-empty device token")
	}
	if got.TenantID != tenantID {
		t.Fatalf("TenantID = %q, want %q", got.TenantID, tenantID)
	}
	dev, err := store.Verify(got.Token)
	if err != nil {
		t.Fatalf("returned token should verify: %v", err)
	}
	if dev.Name != "my-phone" || dev.DevicePubkey != "device-pubkey-b64" || dev.TenantID != tenantID {
		t.Fatalf("unexpected device: %+v", dev)
	}
}

// TestDevicePairRateLimitedPerIP proves the accept criterion for
// docs/improvement-guide.md §6.4: a legitimate pairing attempt succeeds, and
// a second attempt from the same source within devicePairMinInterval is
// throttled with the same 429/rate_limited shape as /tenants/register's
// existing per-IP limiter -- even though the second code is itself
// perfectly valid, proving the limiter (not code validation) is what blocks
// it.
func TestDevicePairRateLimitedPerIP(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	pair := func(code, devicePubkey string) (status int, body string) {
		reqBody := `{"code":"` + code + `","device_pubkey":"` + devicePubkey + `","name":"my-phone"}`
		resp, err := http.Post(srv.URL+"/devices/pair", "application/json", strings.NewReader(reqBody))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(b)
	}

	code1, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := pair(code1, "device-pubkey-b64"); status != http.StatusOK {
		t.Fatalf("first pairing attempt: want 200, got %d", status)
	}

	code2, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	status, body := pair(code2, "device-pubkey-b64-2")
	if status != http.StatusTooManyRequests {
		t.Fatalf("second pairing attempt from the same IP within the interval (a fresh, otherwise-valid code): want 429, got %d", status)
	}
	if !strings.Contains(body, "rate_limited") {
		t.Fatalf("body = %q, want it to contain rate_limited", body)
	}
}

func TestDevicePairDefaultsName(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	body := `{"code":"` + code + `","device_pubkey":"device-pubkey-b64"}`
	resp, err := http.Post(srv.URL+"/devices/pair", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got wire.DevicePairResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	dev, err := store.Verify(got.Token)
	if err != nil || dev.Name != "phone" {
		t.Fatalf("expected default name %q, got device: %+v err=%v", "phone", dev, err)
	}
}

func TestDevicePairRejectsUnknownCode(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	body := `{"code":"bogus","device_pubkey":"x"}`
	resp, err := http.Post(srv.URL+"/devices/pair", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("want 410, got %d", resp.StatusCode)
	}
}

func TestDevicePairRejectsNoDevicePubkey(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	body := `{"code":"` + code + `"}`
	resp, err := http.Post(srv.URL+"/devices/pair", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for missing device_pubkey, got %d", resp.StatusCode)
	}
}

func TestDevicePairWorksWithNoClientCert(t *testing.T) {
	// /devices/pair must be reachable by a phone presenting no client cert at
	// all (X-Client-Cert-Cn absent) -- this is the whole point of the
	// self-service flow. notAgent/auth.Require must never gate this route.
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	body := `{"code":"` + code + `","device_pubkey":"device-pubkey-b64"}`
	req, _ := http.NewRequest("POST", srv.URL+"/devices/pair", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately no X-Client-Cert-Cn header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 with no client cert, got %d", resp.StatusCode)
	}
}

func TestNewPairingCodeRejectsUnverifiedCert(t *testing.T) {
	// The regression test for the cross-tenant impersonation gap: once
	// ssl_verify_client is optional, nginx forwards X-Client-Cert-CN for any
	// presented certificate, verified or not. A correct CN with no (or a
	// failed) X-Client-Cert-Verify must still be rejected.
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	for _, verify := range []string{"", "FAILED:self-signed certificate", "NONE"} {
		req, _ := http.NewRequest("POST", srv.URL+"/agent/pairing-code", nil)
		req.Header.Set("X-Client-Cert-Cn", "CN=agent:"+tenantID)
		if verify != "" {
			req.Header.Set("X-Client-Cert-Verify", verify)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("X-Client-Cert-Verify=%q: want 403 for an unverified cert with a correct CN, got %d", verify, resp.StatusCode)
		}
	}
}

func TestHandleTunnelRejectsUnverifiedAgentCert(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/agent/tunnel", nil)
	req.Header.Set("X-Client-Cert-Cn", "CN=agent:"+tenantID)
	// Deliberately no X-Client-Cert-Verify: SUCCESS -- a spoofed self-signed
	// cert with the right CN must not be enough to open a tunnel.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 for an unverified agent cert, got %d", resp.StatusCode)
	}
}

func TestNewPairingCodeIssuesCode(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/agent/pairing-code", strings.NewReader(`{"agent_pubkey":"agent-pubkey-b64"}`))
	req.Header.Set("X-Client-Cert-Cn", "CN=agent:"+tenantID)
	req.Header.Set("X-Client-Cert-Verify", "SUCCESS")
	resp, err := http.DefaultClient.Do(req)
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

func TestNewPairingCodeRejectsMissingAgentPubkey(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/agent/pairing-code", nil)
	req.Header.Set("X-Client-Cert-Cn", "CN=agent:"+tenantID)
	req.Header.Set("X-Client-Cert-Verify", "SUCCESS")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for missing agent_pubkey, got %d", resp.StatusCode)
	}
}

func TestNewPairingCodeRejectsRevokedTenant(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	store.RevokeTenant(tenantID)
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/agent/pairing-code", nil)
	req.Header.Set("X-Client-Cert-Cn", "CN=agent:"+tenantID)
	req.Header.Set("X-Client-Cert-Verify", "SUCCESS")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 for a revoked tenant, got %d", resp.StatusCode)
	}
}

func TestPairingCodeStatusPendingThenRedeemed(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	poll := func() wire.PairingCodeStatusResp {
		req, _ := http.NewRequest("GET", srv.URL+"/agent/pairing-code/"+code, nil)
		req.Header.Set("X-Client-Cert-Cn", "CN=agent:"+tenantID)
		req.Header.Set("X-Client-Cert-Verify", "SUCCESS")
		resp, err := http.DefaultClient.Do(req)
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

func TestPairingCodeStatusUnknownCodeIs404(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/agent/pairing-code/bogus", nil)
	req.Header.Set("X-Client-Cert-Cn", "CN=agent:"+tenantID)
	req.Header.Set("X-Client-Cert-Verify", "SUCCESS")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestPairingCodeStatusScopedToOwnTenant(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantA, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	tenantB, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.NewPairingCode(tenantA, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/agent/pairing-code/"+code, nil)
	req.Header.Set("X-Client-Cert-Cn", "CN=agent:"+tenantB)
	req.Header.Set("X-Client-Cert-Verify", "SUCCESS")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("tenant B must not see tenant A's pairing code, got %d", resp.StatusCode)
	}
}

func TestPairingCodeStatusRequiresAgentCN(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/agent/pairing-code/"+code, nil)
	req.Header.Set("X-Client-Cert-Cn", "CN=phone")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 for a non-agent CN, got %d", resp.StatusCode)
	}
}

func TestPairingCodeInfoReturnsAgentPubkey(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	// Deliberately no X-Client-Cert-Cn -- a phone pairing manually has no
	// cert yet, same as /devices/pair itself.
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
	if body.AgentPubkey != "agent-pubkey-b64" {
		t.Fatalf("AgentPubkey = %q, want %q", body.AgentPubkey, "agent-pubkey-b64")
	}
	if body.TenantID != tenantID {
		t.Fatalf("TenantID = %q, want %q", body.TenantID, tenantID)
	}
	if body.ExpiresAt == "" {
		t.Fatal("expected a non-empty ExpiresAt")
	}
}

func TestPairingCodeInfoRejectsUnknownCode(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
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

func TestPairingCodeInfoRejectsExpiredCode(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", -time.Second) // already expired
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/devices/pair-info/" + code)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("want 410 for an expired code, got %d", resp.StatusCode)
	}
}

func TestPairingCodeInfoRejectsRedeemedCode(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := store.RedeemPairingCode(code, "phone", "device-pubkey-b64"); !ok {
		t.Fatal("redeem should succeed")
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/devices/pair-info/" + code)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("want 410 for an already-redeemed code, got %d", resp.StatusCode)
	}
}

// cmux-app-af1. The relay hands the phone a working bearer token the moment
// it redeems, which is before the operator has seen a fingerprint to accept
// or refuse -- so the refusal path needs a way to take that token back, and
// it must be as tenant-scoped as every other agent-facing route.
func TestAbortPairingRevokesTheRedeemedToken(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, ok := store.RedeemPairingCode(code, "phone", "device-pubkey-b64")
	if !ok {
		t.Fatal("redeem should succeed")
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/agent/pairing-code/"+code, nil)
	req.Header.Set("X-Client-Cert-Cn", "CN=agent:"+tenantID)
	req.Header.Set("X-Client-Cert-Verify", "SUCCESS")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var body struct {
		Revoked bool `json:"revoked"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Revoked {
		t.Fatal("the agent has to be told the phone's token is gone, not just that the code is")
	}
	if _, err := store.Verify(tok); err == nil {
		t.Fatal("a refused pairing's token still verifies")
	}
}

func TestAbortPairingScopedToOwnTenant(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantA, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	tenantB, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.NewPairingCode(tenantA, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, ok := store.RedeemPairingCode(code, "phone", "device-pubkey-b64")
	if !ok {
		t.Fatal("redeem should succeed")
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/agent/pairing-code/"+code, nil)
	req.Header.Set("X-Client-Cert-Cn", "CN=agent:"+tenantB)
	req.Header.Set("X-Client-Cert-Verify", "SUCCESS")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("tenant B must not be able to revoke tenant A's device, got %d", resp.StatusCode)
	}
	if _, err := store.Verify(tok); err != nil {
		t.Fatalf("tenant A's token must survive: %v", err)
	}
}

func TestAbortPairingRequiresAgentCN(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rl := New(store, nil, "relay-secret")
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/agent/pairing-code/"+code, nil)
	req.Header.Set("X-Client-Cert-Cn", "CN=phone")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 for a non-agent CN, got %d", resp.StatusCode)
	}
}

// confirmablePairing builds the state the confirm route acts on: a relay
// serving a tenant whose pairing code a phone has already redeemed.
func confirmablePairing(t *testing.T) (store *auth.Store, srv *httptest.Server, tenantID, code string) {
	t.Helper()
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err = store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	code, err = store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := store.RedeemPairingCode(code, "phone", "device-pubkey-b64"); !ok {
		t.Fatal("redeem should succeed")
	}
	rl := New(store, nil, "relay-secret")
	srv = httptest.NewServer(rl.Handler())
	t.Cleanup(srv.Close)
	return store, srv, tenantID, code
}

func agentConfirm(t *testing.T, srv *httptest.Server, tenantCN, code string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/agent/pairing-code/"+code+"/confirm", nil)
	if tenantCN != "" {
		req.Header.Set("X-Client-Cert-Cn", "CN=agent:"+tenantCN)
		req.Header.Set("X-Client-Cert-Verify", "SUCCESS")
	} else {
		req.Header.Set("X-Client-Cert-Cn", "CN=phone")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func devicePairStatus(t *testing.T, srv *httptest.Server, code string) (int, wire.PairStatusResp) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/devices/pair-status/" + code)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body wire.PairStatusResp
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, body
}

// cmux-app-gmo. The phone holds its pairing in memory until it reads
// confirmed, so this route is what ends the wait.
func TestConfirmPairingMovesTheStatusThePhoneIsWatching(t *testing.T) {
	_, srv, tenantID, code := confirmablePairing(t)

	if status, body := devicePairStatus(t, srv, code); status != http.StatusOK || body.State != auth.PairingPending {
		t.Fatalf("before the operator answers = (%d, %q), want (200, pending)", status, body.State)
	}
	if got := agentConfirm(t, srv, tenantID, code); got != http.StatusOK {
		t.Fatalf("confirm = %d, want 200", got)
	}
	if status, body := devicePairStatus(t, srv, code); status != http.StatusOK || body.State != auth.PairingConfirmed {
		t.Fatalf("after the operator confirms = (%d, %q), want (200, confirmed)", status, body.State)
	}
}

func TestConfirmPairingRequiresAgentCN(t *testing.T) {
	store, srv, _, code := confirmablePairing(t)

	if got := agentConfirm(t, srv, "", code); got != http.StatusForbidden {
		t.Fatalf("a caller with no agent CN = %d, want 403", got)
	}
	if state, _ := store.PairingConfirmationState(code, time.Minute); state != auth.PairingPending {
		t.Fatalf("the rejected call still confirmed the pairing: state=%q", state)
	}
}

// The same 403/404 split abortPairing uses: a foreign tenant that presents a
// valid agent CN is a real agent, just not this pairing's -- so 404, not 403.
func TestConfirmPairingScopedToOwnTenant(t *testing.T) {
	store, srv, _, code := confirmablePairing(t)
	stranger, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}

	if got := agentConfirm(t, srv, stranger, code); got != http.StatusNotFound {
		t.Fatalf("tenant B confirming tenant A's pairing = %d, want 404", got)
	}
	if state, _ := store.PairingConfirmationState(code, time.Minute); state != auth.PairingPending {
		t.Fatalf("the stranger confirmed another tenant's pairing: state=%q", state)
	}
}

func TestConfirmPairingAfterARefusalIs409(t *testing.T) {
	store, srv, tenantID, code := confirmablePairing(t)
	if _, err := store.AbortPairing(tenantID, code); err != nil {
		t.Fatal(err)
	}

	if got := agentConfirm(t, srv, tenantID, code); got != http.StatusConflict {
		t.Fatalf("confirming a refused pairing = %d, want 409", got)
	}
	if _, body := devicePairStatus(t, srv, code); body.State != auth.PairingRefused {
		t.Fatalf("state after the rejected confirm = %q, want refused", body.State)
	}
}

// The status route is the one thing in the pairing flow a phone must reach
// with no credential at all: refusal destroys the very token an
// authenticated route would demand.
func TestPairStatusNeedsNoCredentialAndLeaksNothingElse(t *testing.T) {
	_, srv, _, code := confirmablePairing(t)

	resp, err := http.Get(srv.URL + "/devices/pair-status/" + code)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an unauthenticated poll = %d, want 200", resp.StatusCode)
	}
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 || raw["state"] != auth.PairingPending {
		t.Fatalf("the status response must carry state and nothing else, got %v", raw)
	}
}

func TestPairStatusUnknownCodeIs404(t *testing.T) {
	_, srv, _, _ := confirmablePairing(t)

	if status, _ := devicePairStatus(t, srv, "NOSUCHCD"); status != http.StatusNotFound {
		t.Fatalf("an unknown code = %d, want 404", status)
	}
}

// The relay is the primary pairing path, and it terminates POST /devices/pair
// itself rather than proxying it to the agent -- so the config a phone gets
// there is the RELAY's, and a wiring mistake on this side would be invisible
// to every direct-mode test.
func TestRelayDevicePairCarriesFCMClientConfig(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want := wire.FCMClientConfig{ProjectID: "p", AppID: "a", APIKey: "k", SenderID: "s"}
	rl := New(store, nil, "relay-secret")
	// Before Handler(): pairing.Mount copies the value at mount time, so the
	// reverse order silently sends nothing. This ordering is the contract.
	rl.SetFCMClientConfig(want)
	srv := httptest.NewServer(rl.Handler())
	defer srv.Close()

	body := `{"code":"` + code + `","device_pubkey":"device-pubkey-b64","name":"my-phone"}`
	resp, err := http.Post(srv.URL+"/devices/pair", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got wire.DevicePairResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.FCM == nil {
		t.Fatal("relay pairing must carry the relay's own FCM client config")
	}
	if *got.FCM != want {
		t.Fatalf("FCM = %+v, want %+v", *got.FCM, want)
	}
}

// A relay with no push configured must answer exactly as it did before the
// field existed.
func TestRelayDevicePairOmitsFCMWhenUnconfigured(t *testing.T) {
	store, err := auth.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.NewPairingCode(tenantID, "agent-pubkey-b64", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(store, nil, "relay-secret").Handler())
	defer srv.Close()

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
	if strings.Contains(string(raw), "fcm") {
		t.Fatalf("unconfigured relay must not mention fcm, got %s", raw)
	}
}
