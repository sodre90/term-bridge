package server

import (
	"net/http"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/devices"
	"github.com/sodre90/term-bridge/internal/pairing"
	"github.com/sodre90/term-bridge/internal/wire"
)

// MountDirectPairing registers the pre-auth pairing routes and the
// agent-facing device-admin routes direct mode needs onto mux, backed by
// store and scoped to the single implicit tenant tenantID (direct mode has
// exactly one, created once at agent startup -- see runAgent). The handlers
// are the same shared implementations the relay mounts (internal/pairing,
// internal/devices), minus that side's agentOnly/mTLS-CN tenant resolution:
// there's no second tenant to disambiguate from here, and the real access
// boundary for this whole listener is Tailscale's own network ACLs, not a
// per-request identity check on these routes. Same reason /devices/pair
// isn't rate-limited here either (pairing.AllowAll): this listener only
// binds this Mac's own Tailscale IPv4 address (serveDirect), so it's never
// internet-reachable the way the relay's copy is.
//
// The device-admin routes inherit that posture, which means any tailnet peer
// can list and revoke direct-paired devices -- see devices.Mount for why
// that is accepted rather than overlooked.
// fcm carries the client-side Firebase config to hand a phone at pairing; its
// zero value means push was not configured and nothing extra is sent.
func MountDirectPairing(mux *http.ServeMux, store *auth.Store, tenantID string, fcm wire.FCMClientConfig) {
	pairing.Mount(mux, store, pairing.ConstantTenant(tenantID), pairing.AllowAll, fcm)
	devices.Mount(mux, store, pairing.ConstantTenant(tenantID))
}
