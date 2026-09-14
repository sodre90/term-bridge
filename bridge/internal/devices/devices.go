// Package devices implements the agent-facing device-admin HTTP routes
// served, byte-identically, by both the relay (internal/relay) and direct
// mode (internal/server). It exists so an operator can enumerate and revoke
// paired devices using an identifier the system actually exposes -- the
// token hash -- rather than the raw bearer token only the device itself
// holds (cmux-app-vkq).
//
// Deliberately separate from internal/pairing: these routes act on devices
// that finished pairing long ago, and folding them in would make that
// package's name a lie. The TenantResolver seam is shared with it, since
// "how does this request resolve to a tenant" is the same question in both.
package devices

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/httpjson"
	"github.com/sodre90/term-bridge/internal/pairing"
	"github.com/sodre90/term-bridge/internal/wire"
)

type handlers struct {
	store  *auth.Store
	tenant pairing.TenantResolver
}

// Mount registers the device-admin routes onto mux, backed by store, with
// tenant deciding which tenant the caller acts for.
//
// Both routes are agent-facing: the relay passes its verified-mTLS-CN
// resolver, so a tenant reaches only its own devices. Direct mode passes
// ConstantTenant and therefore applies no per-request identity check at all
// -- the same posture MountDirectPairing documents, where that listener's
// access boundary is Tailscale's network ACLs rather than anything here.
// That means any tailnet peer can list and revoke direct-paired devices.
// Accepted: the listing carries no secrets, revocation is the safe direction
// in which to fail, and the same caller can already mint pairing codes.
func Mount(mux *http.ServeMux, store *auth.Store, tenant pairing.TenantResolver) {
	h := &handlers{store: store, tenant: tenant}
	mux.Handle("GET /agent/devices", http.HandlerFunc(h.listDevices))
	mux.Handle("POST /agent/devices/{tokenHash}/revoke", http.HandlerFunc(h.revokeDevice))
}

// MountSelfRevoke registers the one device-facing route here: a device
// retiring its own credential, which is what the phone's Forget calls
// (cmux-app-f5y).
//
// wrap, rather than Mount's TenantResolver, because the two servers
// authenticate a device differently -- the relay adds its notAgent guard
// around auth.Require, direct mode uses auth.Require alone -- while both
// leave the verified device in the request context, which is all the handler
// reads.
//
// Direct mode must pass a wrap WITHOUT encryptionMiddleware, unlike its
// other device routes. This request carries no cmux content, and a handler
// behind that middleware cannot delete anything the response is encrypted
// with; see the design doc's "the obvious design is wrong".
func MountSelfRevoke(mux *http.ServeMux, store *auth.Store, wrap func(http.Handler) http.Handler) {
	h := &handlers{store: store}
	mux.Handle("POST /devices/self-revoke", wrap(http.HandlerFunc(h.selfRevoke)))
}

// selfRevoke deletes the calling device's own token and nothing else. The
// device comes from the bearer token auth.Require already verified, never
// from the request, so there is no identifier here for a caller to point at
// somebody else's credential.
//
// A token that is already gone is a success: the caller's goal is that the
// credential stops working, and Forget must survive being retried.
func (h *handlers) selfRevoke(w http.ResponseWriter, req *http.Request) {
	dev, ok := auth.DeviceFromContext(req.Context())
	if !ok {
		httpjson.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	err := h.store.RevokeByHash(dev.TenantID, dev.TokenHash)
	if err != nil && !errors.Is(err, auth.ErrNotFound) {
		slog.Error("devices: self-revoke", "tenant_id", dev.TenantID, "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal_error")
		return
	}
	httpjson.Write(w, http.StatusOK, struct{}{})
}

func (h *handlers) listDevices(w http.ResponseWriter, req *http.Request) {
	tenantID, ok := h.tenant(req)
	if !ok {
		httpjson.Error(w, http.StatusForbidden, "forbidden")
		return
	}
	found := h.store.ListByTenant(tenantID)
	out := make([]wire.AgentDevice, 0, len(found))
	for _, dev := range found {
		out = append(out, wire.AgentDevice{
			Name:      dev.Name,
			TokenHash: dev.TokenHash,
			CreatedAt: dev.Created.UTC().Format(time.RFC3339),
			HasFCM:    dev.FCM != "",
		})
	}
	httpjson.Write(w, http.StatusOK, wire.AgentDeviceListResp{Devices: out})
}

func (h *handlers) revokeDevice(w http.ResponseWriter, req *http.Request) {
	tenantID, ok := h.tenant(req)
	if !ok {
		httpjson.Error(w, http.StatusForbidden, "forbidden")
		return
	}
	err := h.store.RevokeByHash(tenantID, req.PathValue("tokenHash"))
	switch {
	case errors.Is(err, auth.ErrNotFound):
		httpjson.Error(w, http.StatusNotFound, "unknown_device")
	case err != nil:
		slog.Error("devices: revoke", "tenant_id", tenantID, "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal_error")
	default:
		httpjson.Write(w, http.StatusOK, struct{}{})
	}
}
