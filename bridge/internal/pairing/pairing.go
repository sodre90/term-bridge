// Package pairing implements the self-service pairing-code HTTP routes
// served, byte-identically, by both the relay (internal/relay) and direct
// mode (internal/server). The handlers used to exist near-verbatim in both
// packages; their only real difference -- how a request resolves to a
// tenant -- is the TenantResolver parameter.
package pairing

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/httpjson"
	"github.com/sodre90/term-bridge/internal/metrics"
	"github.com/sodre90/term-bridge/internal/wire"
)

// TenantResolver reports the tenant the agent-facing pairing-code routes
// (issue + status poll) act for, or ok=false to reject the request with a
// 403. The relay passes its mTLS-CN-based agent resolver; direct mode
// passes ConstantTenant.
type TenantResolver func(*http.Request) (tenantID string, ok bool)

// ConstantTenant returns a TenantResolver that resolves every request to
// tenantID -- direct mode's one implicit tenant. It never rejects: direct
// mode deliberately has no per-request identity check on these routes.
func ConstantTenant(tenantID string) TenantResolver {
	return func(*http.Request) (string, bool) { return tenantID, true }
}

// RateLimiter reports whether req may proceed. It gates POST /devices/pair
// only -- pairing codes are already single-use with a short TTL, so this is
// defense-in-depth against brute-force guessing on an endpoint that, for the
// relay, is reachable with no credential by design. Kept as a plain function
// type (mirroring TenantResolver) rather than importing a concrete limiter
// type, so this package stays free of relay-only concerns: the relay passes
// a closure around its own ipRateLimiter, direct mode passes AllowAll.
type RateLimiter func(*http.Request) bool

// AllowAll is a RateLimiter that never throttles. Direct mode's default:
// Tailscale's own network ACLs are that listener's access-control boundary,
// not this package's concern.
func AllowAll(*http.Request) bool { return true }

// Mount registers the pairing routes onto mux, backed by store, with
// tenant deciding which tenant the agent-facing routes act for and
// devicePairLimit gating POST /devices/pair specifically (pass AllowAll to
// disable).
func Mount(mux *http.ServeMux, store *auth.Store, tenant TenantResolver, devicePairLimit RateLimiter, fcm wire.FCMClientConfig) {
	h := &handlers{store: store, tenant: tenant, devicePairLimit: devicePairLimit, fcm: fcm}
	mux.Handle("POST /agent/pairing-code", http.HandlerFunc(h.newPairingCode))
	mux.Handle("GET /agent/pairing-code/{code}", http.HandlerFunc(h.pairingCodeStatus))
	mux.Handle("DELETE /agent/pairing-code/{code}", http.HandlerFunc(h.abortPairing))
	mux.Handle("POST /agent/pairing-code/{code}/confirm", http.HandlerFunc(h.confirmPairing))
	mux.Handle("GET /devices/pair-info/{code}", http.HandlerFunc(h.pairingCodeInfo))
	mux.Handle("GET /devices/pair-status/{code}", http.HandlerFunc(h.pairStatus))
	mux.Handle("POST /devices/pair", http.HandlerFunc(h.devicePair))
}

type handlers struct {
	// fcm is the client-side Firebase config handed to a phone on a
	// successful redemption. The zero value means push was never configured
	// here, and devicePair then answers exactly as it did before this
	// existed.
	fcm             wire.FCMClientConfig
	store           *auth.Store
	tenant          TenantResolver
	devicePairLimit RateLimiter
}

// newPairingCode lets an agent request a fresh single-use pairing code to
// embed in a QR code (see cmd/term-bridge/pair.go). TenantID is echoed
// back so the QR payload can carry it for display, even though
// /devices/pair itself never needs it in the request (see the Global
// Constraint on that endpoint's simplified request/response shapes) — the
// pairing code alone is resolved to a tenant server-side. The agent's e2e
// public key is stored alongside the code (not just embedded in the QR) so
// a phone pairing via manual entry can resolve it too, via pairingCodeInfo.
func (h *handlers) newPairingCode(w http.ResponseWriter, req *http.Request) {
	tenantID, ok := h.tenant(req)
	if !ok {
		httpjson.Error(w, http.StatusForbidden, "forbidden")
		return
	}
	var rq wire.NewPairingCodeReq
	if err := json.NewDecoder(req.Body).Decode(&rq); err != nil || rq.AgentPubkey == "" {
		httpjson.Error(w, http.StatusBadRequest, "missing agent_pubkey")
		return
	}
	code, err := h.store.NewPairingCode(tenantID, rq.AgentPubkey, wire.PairingCodeTTL)
	if err != nil {
		slog.Error("pairing: new pairing code", "tenant_id", tenantID, "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal_error")
		return
	}
	metrics.PairingCodesIssuedTotal.Add(1)
	httpjson.Write(w, http.StatusOK, wire.PairingCodeResp{
		Code:      code,
		ExpiresAt: time.Now().Add(wire.PairingCodeTTL).UTC().Format(time.RFC3339),
		TenantID:  tenantID,
	})
}

// pairingCodeStatus lets the agent that requested a pairing code poll for
// its redemption. Scoped to the resolver's own tenant, so one tenant's
// agent can never observe another tenant's pairing codes.
func (h *handlers) pairingCodeStatus(w http.ResponseWriter, req *http.Request) {
	tenantID, ok := h.tenant(req)
	if !ok {
		httpjson.Error(w, http.StatusForbidden, "forbidden")
		return
	}
	code := req.PathValue("code")
	pubkey, hash, redeemed, ok := h.store.PairingCodeStatus(tenantID, code)
	if !ok {
		httpjson.Error(w, http.StatusNotFound, "not_found")
		return
	}
	httpjson.Write(w, http.StatusOK, wire.PairingCodeStatusResp{
		Redeemed:     redeemed,
		DevicePubkey: pubkey,
		TokenHash:    hash,
	})
}

// abortPairing lets the agent that issued a pairing code destroy whatever
// that code produced, for the case the operator refuses the fingerprint (see
// cmd/term-bridge/pair.go). Redemption mints the device's bearer token
// before the operator is ever asked, so without this an explicit refusal
// left the phone holding a working credential -- one with no e2e session
// behind it, but valid for every bearer-authenticated route
// (cmux-app-af1). Tenant-scoped exactly like pairingCodeStatus.
func (h *handlers) abortPairing(w http.ResponseWriter, req *http.Request) {
	tenantID, ok := h.tenant(req)
	if !ok {
		httpjson.Error(w, http.StatusForbidden, "forbidden")
		return
	}
	revoked, err := h.store.AbortPairing(tenantID, req.PathValue("code"))
	if errors.Is(err, auth.ErrNotFound) {
		httpjson.Error(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		slog.Error("pairing: abort pairing", "tenant_id", tenantID, "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if revoked {
		slog.Warn("pairing: pairing refused by operator, device token revoked", "tenant_id", tenantID)
	}
	httpjson.Write(w, http.StatusOK, wire.AbortPairingResp{Revoked: revoked})
}

// confirmPairing lets the agent that issued a pairing code record that the
// operator answered yes at the fingerprint prompt. It is the counterpart to
// abortPairing and the single commit point of the whole flow: until it lands,
// the phone holds its pairing in memory and persists nothing (cmux-app-gmo).
// Tenant-scoped exactly like abortPairing, with the same 403/404 split.
func (h *handlers) confirmPairing(w http.ResponseWriter, req *http.Request) {
	tenantID, ok := h.tenant(req)
	if !ok {
		httpjson.Error(w, http.StatusForbidden, "forbidden")
		return
	}
	err := h.store.ConfirmPairing(tenantID, req.PathValue("code"), wire.PairingConfirmTTL)
	switch {
	case errors.Is(err, auth.ErrNotFound):
		httpjson.Error(w, http.StatusNotFound, "not_found")
	case errors.Is(err, auth.ErrPairingRefused):
		httpjson.Error(w, http.StatusConflict, "pairing_refused")
	case errors.Is(err, auth.ErrPairingConfirmExpired):
		httpjson.Error(w, http.StatusConflict, "pairing_confirm_expired")
	case err != nil:
		slog.Error("pairing: confirm pairing", "tenant_id", tenantID, "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal_error")
	default:
		httpjson.Write(w, http.StatusOK, struct{}{})
	}
}

// pairStatus lets a phone that has completed /devices/pair learn whether the
// operator accepted it, so it can persist the pairing only once the answer is
// yes. Public, no auth -- matching /devices/pair and /devices/pair-info on
// the same vhost, and necessarily so: refusing a pairing destroys the very
// bearer token an authenticated route would demand, so the refused case would
// answer 401, indistinguishable from a misconfigured credential. An
// unauthenticated route can say "refused" out loud. Not tenant-scoped and it
// must not echo a tenant back: the response is one enum value, nothing more.
func (h *handlers) pairStatus(w http.ResponseWriter, req *http.Request) {
	state, ok := h.store.PairingConfirmationState(req.PathValue("code"), wire.PairingConfirmTTL)
	if !ok {
		httpjson.Error(w, http.StatusNotFound, "not_found")
		return
	}
	httpjson.Write(w, http.StatusOK, wire.PairStatusResp{State: state})
}

// pairingCodeInfo lets a phone that can't scan the QR (no camera, or
// pairing remotely) resolve a manually-entered pairing code to the same
// {agent_pubkey, expires_at, tenant_id} the QR itself carries, so it can
// complete /devices/pair exactly like the QR path does. Public, no auth --
// mirrors /devices/pair's own reachability (a brand-new phone has no
// credential to present yet). Not tenant-scoped, unlike pairingCodeStatus:
// the caller doesn't know its tenant, that's what it's asking for.
// Collapses not-found/expired/already-redeemed into the same 410, matching
// /devices/pair's own error handling.
func (h *handlers) pairingCodeInfo(w http.ResponseWriter, req *http.Request) {
	code := req.PathValue("code")
	agentPubkey, tenantID, expiresAt, ok := h.store.PairingCodeInfo(code)
	if !ok {
		httpjson.Error(w, http.StatusGone, "pairing_code_invalid")
		return
	}
	httpjson.Write(w, http.StatusOK, wire.PairingCodeInfoResp{
		AgentPubkey: agentPubkey,
		ExpiresAt:   expiresAt,
		TenantID:    tenantID,
	})
}

// devicePair is the public, no-auth endpoint a phone hits directly after
// scanning the agent's pairing QR code (or resolving a manually entered
// code via pairingCodeInfo). The response omits the agent's e2e public key
// (the phone already has it from the QR code payload itself,
// cmd/term-bridge/pair.go, or from pair-info -- the relay never needs to
// hold or forward e2e key material) but keeps tenant_id, informationally,
// so the app knows which workspace it just joined.
func (h *handlers) devicePair(w http.ResponseWriter, req *http.Request) {
	if !h.devicePairLimit(req) {
		httpjson.Error(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, 4<<10)
	var rq wire.DevicePairReq
	if err := json.NewDecoder(req.Body).Decode(&rq); err != nil || rq.Code == "" || rq.DevicePubkey == "" {
		httpjson.Error(w, http.StatusBadRequest, "missing code or device_pubkey")
		return
	}
	name := rq.Name
	if name == "" {
		name = "phone"
	}
	tok, tenantID, ok := h.store.RedeemPairingCode(rq.Code, name, rq.DevicePubkey)
	if !ok {
		// RedeemPairingCode's bool return doesn't distinguish not-found,
		// expired, and already-redeemed -- per the spec's error-handling
		// section, all three map to the same response.
		httpjson.Error(w, http.StatusGone, "pairing_code_invalid")
		return
	}
	metrics.PairingCodesRedeemedTotal.Add(1)
	resp := wire.DevicePairResp{Token: tok, TenantID: tenantID}
	if h.fcm.Configured() {
		fcm := h.fcm
		resp.FCM = &fcm
	}
	httpjson.Write(w, http.StatusOK, resp)
	slog.Info("pairing: device paired via QR code", "tenant_id", tenantID)
}
