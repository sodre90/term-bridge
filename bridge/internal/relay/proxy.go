package relay

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/httpjson"
	"github.com/sodre90/term-bridge/internal/metrics"
)

// ErrAgentOffline is returned by the proxy transport when the target
// tenant's agent has no active session.
var ErrAgentOffline = errors.New("agent offline")

// newProxy builds the reverse proxy that forwards an app request over a fresh
// yamux stream to the agent belonging to the authenticated device's tenant,
// injecting the relay token. A device can never reach another tenant's
// session: the dial target is resolved solely from auth.DeviceFromContext,
// never from "whichever session happens to be registered."
func newProxy(reg *Registry, relayToken string, conns *ConnTracker) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		// httputil.ReverseProxy only accepts a *log.Logger; slog.NewLogLogger
		// routes its Printf calls through the shared slog handler instead of
		// a second, independent log destination.
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "agent" // ignored by the stream dialer below
			req.Header.Set("X-Relay-Token", relayToken)
			if dev, ok := auth.DeviceFromContext(req.Context()); ok {
				req.Header.Set("X-Device-ID", dev.TokenHash)
				metrics.ProxyRequestsTotal.Add(dev.TenantID, 1)
			}
		},
		Transport: &http.Transport{
			// Security-load-bearing, not a performance knob: DialContext below
			// resolves the target tenant from the request context on every
			// call. http.Transport pools/reuses idle connections keyed only by
			// the (constant, identical-for-every-tenant) placeholder host set
			// in Director above, so without DisableKeepAlives a connection
			// dialed for one tenant could be handed to a different tenant's
			// request — a cross-tenant leak this repo's adversarial test
			// (internal/relay/multitenant_test.go) exists to catch. Do not
			// remove this to "improve" performance.
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				dev, ok := auth.DeviceFromContext(ctx)
				if !ok {
					return nil, ErrAgentOffline
				}
				sess := reg.Get(dev.TenantID)
				if sess == nil {
					return nil, ErrAgentOffline
				}
				conn, err := sess.Open()
				if err != nil {
					return nil, err
				}
				// A WebSocket holds this stream for its whole life, which is
				// what makes it reachable by revocation later; a plain
				// request's stream deregisters itself moments later on Close.
				return conns.Track(dev.TokenHash, dev.TenantID, conn), nil
			},
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			var tenantID string
			if dev, ok := auth.DeviceFromContext(req.Context()); ok {
				tenantID = dev.TenantID
			}
			if errors.Is(err, ErrAgentOffline) {
				slog.Warn("relay: agent offline", "method", req.Method, "route", req.URL.Path, "tenant_id", tenantID)
				if tenantID != "" {
					metrics.ProxyAgentOfflineTotal.Add(tenantID, 1)
				}
				httpjson.Error(w, http.StatusServiceUnavailable, "agent_offline")
				return
			}
			slog.Error("relay: agent proxy error", "method", req.Method, "route", req.URL.Path, "tenant_id", tenantID, "err", err)
			httpjson.Error(w, http.StatusBadGateway, "agent_error")
		},
	}
}
