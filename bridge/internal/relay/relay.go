package relay

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"expvar"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/ca"
	"github.com/sodre90/term-bridge/internal/devices"
	"github.com/sodre90/term-bridge/internal/httpjson"
	"github.com/sodre90/term-bridge/internal/pairing"
	"github.com/sodre90/term-bridge/internal/ratelimit"
	"github.com/sodre90/term-bridge/internal/tunnel"
	"github.com/sodre90/term-bridge/internal/wire"
)

// agentCNPrefix marks a client cert as belonging to a Mac agent, followed by
// its tenant ID (e.g. "agent:9f3a2c..."). Any other CN shape is a device.
const agentCNPrefix = "agent:"

// agentCertValidity is how long a freshly issued agent cert is valid. Cert
// rotation without losing tenant identity is out of scope for this version —
// after this window an agent must re-register, minting a new tenant ID.
const agentCertValidity = 365 * 24 * time.Hour

// defaultMaxTenants is a generous safety valve against unbounded tenant
// growth from /tenants/register abuse, not a real usage limit for a
// self-hosted relay.
const defaultMaxTenants = 1000

// tenantRegisterMinInterval bounds how often a single source IP may hit
// /tenants/register. Defense-in-depth alongside nginx's own limit_req zone
// (deploy/nginx-term-bridge-relay-bootstrap.conf) -- this endpoint is reachable with
// no client cert by design, so both layers matter.
const tenantRegisterMinInterval = 10 * time.Second

// devicePairMinInterval bounds how often a single source IP may hit
// POST /devices/pair. Pairing codes are single-use with a 10-minute TTL
// (wire.PairingCodeTTL) and drawn from a large alphabet, so brute force is
// already unrealistic -- this is cheap defense-in-depth on an
// internet-reachable endpoint, not the primary control. Same order of
// magnitude as tenantRegisterMinInterval.
const devicePairMinInterval = 5 * time.Second

// testPushDeviceCooldown bounds how often a single device may trigger
// handleTestPush (testpush.go) -- see
// docs/improvement-guide.md item 7.7's "rate-limit sensibly" requirement.
// 30s is generous for a manual "let me check push works" tap while still
// making the endpoint useless as a spam vector; matches
// internal/server.testPushDeviceCooldown, direct mode's equivalent.
const testPushDeviceCooldown = 30 * time.Second

// Relay is the home-server rendezvous: it accepts Mac agents' tunnels and
// reverse-proxies authenticated app requests over them.
type Relay struct {
	store             *auth.Store
	reg               *Registry
	ca                *ca.CA
	relayToken        string
	edgeToken         string
	proxy             *httputil.ReverseProxy
	onSession         func(context.Context, string, *yamux.Session)
	registerLimiter   *ipRateLimiter
	devicePairLimiter *ipRateLimiter
	maxTenants        int
	// push is nil unless SetPusher is called (only by term-bridge-relay serve's
	// production wiring, and only when FCM config is present). Nil means
	// POST /devices/test-push (testpush.go) reports 503 push_not_configured;
	// real attention-event fanout (pushmon.go's MonitorAgent) takes its own
	// Pusher directly via SetSessionHook and is unaffected by this field.
	push Pusher
	// fcm is the client-side Firebase config handed to a phone at pairing.
	// Zero value means push was never configured on this relay.
	fcm              wire.FCMClientConfig
	testPushCooldown *ratelimit.Cooldown
	conns            *ConnTracker
}

// New builds a Relay. store may be nil only in tests that never hit auth
// routes. signer may be nil only in tests that never hit /tenants/register.
func New(store *auth.Store, signer *ca.CA, relayToken string) *Relay {
	reg := NewRegistry()
	conns := NewConnTracker()
	return &Relay{
		store:             store,
		reg:               reg,
		ca:                signer,
		relayToken:        relayToken,
		conns:             conns,
		proxy:             newProxy(reg, relayToken, conns),
		registerLimiter:   newIPRateLimiter(tenantRegisterMinInterval),
		devicePairLimiter: newIPRateLimiter(devicePairMinInterval),
		maxTenants:        defaultMaxTenants,
		testPushCooldown:  ratelimit.NewCooldown(testPushDeviceCooldown),
	}
}

// SetPusher enables POST /devices/test-push (testpush.go). Independent of
// SetSessionHook's pushmon wiring, which fans real attention events out to
// every device on a tenant; both share the same Pusher instance in
// production (cmd/term-bridge-relay/serve.go) but are wired separately since a test
// push is deliberately scoped to the one calling device, never a tenant-wide
// fanout.
func (r *Relay) SetPusher(p Pusher) { r.push = p }

// ipRateLimiter allows one call per key per interval, sweeping stale entries
// on access so the map doesn't grow unboundedly under a distributed-source
// flood. Not goroutine-hot: only used on the low-frequency
// /tenants/register path.
type ipRateLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	last     map[string]time.Time
}

func newIPRateLimiter(interval time.Duration) *ipRateLimiter {
	return &ipRateLimiter{interval: interval, last: map[string]time.Time{}}
}

func (l *ipRateLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.last[key]; ok && now.Sub(t) < l.interval {
		return false
	}
	l.last[key] = now
	for k, t := range l.last {
		if now.Sub(t) > 10*l.interval {
			delete(l.last, k)
		}
	}
	return true
}

// clientIP returns the address to key rate limiting by. When an edge token
// is configured, requireEdge gates every route so only nginx can reach this
// handler, and nginx always SETS (never merges) X-Forwarded-For from its own
// $remote_addr -- exactly as trusted as the existing X-Client-Cert-CN model.
// Without an edge token there's no guaranteed intermediary, so fall back to
// the raw TCP peer.
func (r *Relay) clientIP(req *http.Request) string {
	if r.edgeToken != "" {
		if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
			return xff
		}
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

// SetFCMClientConfig sets the client-side Firebase config this relay hands a
// phone at pairing. Called only by term-bridge-relay serve's production wiring, and
// only when all four client fields are configured; left unset, pairing
// responses omit the block entirely.
func (r *Relay) SetFCMClientConfig(c wire.FCMClientConfig) { r.fcm = c }

// SetSessionHook registers a callback invoked (in its own goroutine) for each
// accepted agent session; its context is cancelled when the session ends.
func (r *Relay) SetSessionHook(f func(context.Context, string, *yamux.Session)) { r.onSession = f }

// SetEdgeToken sets a shared secret the trusted edge (nginx) must present in
// X-Edge-Token on every request except /healthz. Empty disables the check.
func (r *Relay) SetEdgeToken(t string) { r.edgeToken = t }

// SweepRevokedConnections closes the connections of devices that have been
// revoked since they connected, until ctx is done. Blocks; run it in a
// goroutine. Requests already fail closed the instant a row is deleted --
// this is only for sockets that authenticated before that happened.
func (r *Relay) SweepRevokedConnections(ctx context.Context) {
	r.conns.SweepRevoked(ctx, r.store, connSweepPeriod)
}

// parseCN extracts the CN attribute from an RFC2253 ("CN=foo,O=bar") or legacy
// slash ("/CN=foo/O=bar") distinguished name.
func parseCN(dn string) string {
	for _, part := range strings.FieldsFunc(dn, func(r rune) bool { return r == ',' || r == '/' }) {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "CN=") {
			return strings.TrimPrefix(part, "CN=")
		}
	}
	return ""
}

func (r *Relay) clientCN(req *http.Request) string {
	return parseCN(req.Header.Get("X-Client-Cert-CN"))
}

// tenantFromAgentCN extracts the tenant ID from an agent CN ("agent:<id>"),
// or reports ok=false for any other CN shape (devices, or no cert at all).
func tenantFromAgentCN(cn string) (tenantID string, ok bool) {
	if !strings.HasPrefix(cn, agentCNPrefix) {
		return "", false
	}
	id := strings.TrimPrefix(cn, agentCNPrefix)
	if id == "" {
		return "", false
	}
	return id, true
}

// agentOnly extracts the calling agent's tenant ID from its mTLS CN,
// rejecting any request that isn't a valid, currently-active, VERIFIED
// agent. Used by the agent-facing pairing-code endpoints, which
// authenticate via mTLS CN rather than auth.Require's device bearer token.
func (r *Relay) agentOnly(req *http.Request) (string, bool) {
	return r.verifiedAgentTenant(req)
}

// verifiedAgentTenant extracts the calling agent's tenant ID from its mTLS
// CN, requiring nginx to report the presented certificate as independently
// verified (X-Client-Cert-Verify: SUCCESS) before trusting the CN at all.
//
// Before this method existed, every agent-CN check (handleTunnel, notAgent,
// agentOnly) trusted X-Client-Cert-CN on its own -- safe only because
// ssl_verify_client was mandatory ("on"), so nginx guaranteed any request
// that reached the relay had already presented a cert chaining to the
// trusted CA, or the TLS handshake would have failed before the request
// ever arrived. Now that ssl_verify_client is optional (see
// deploy/nginx-term-bridge-relay.conf, changed below in this same task, to let
// certless paired devices connect), nginx forwards X-Client-Cert-CN for ANY
// presented certificate -- including a trivial self-signed one with
// CN=agent:<any-tenant-id> -- so a bare CN match is no longer proof of agent
// identity; only a verified cert is.
func (r *Relay) verifiedAgentTenant(req *http.Request) (string, bool) {
	if req.Header.Get("X-Client-Cert-Verify") != "SUCCESS" {
		return "", false
	}
	tenantID, ok := tenantFromAgentCN(r.clientCN(req))
	if !ok || !r.store.TenantActive(tenantID) {
		return "", false
	}
	return tenantID, true
}

func (r *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	// expvar registers its own vars process-globally (internal/metrics'
	// counters and gauges included) regardless of which package incremented
	// them; mounting its handler here is the only wiring needed to serve
	// them, since this mux is custom rather than http.DefaultServeMux (the
	// only target expvar's own init() registers against).
	mux.Handle("GET /debug/vars", expvar.Handler())
	mux.HandleFunc("/agent/tunnel", r.handleTunnel)
	// Self-service pairing routes (internal/pairing). The issue/status
	// routes are agent-CN-gated via agentOnly; /devices/pair and
	// /devices/pair-info are public by design -- a brand-new phone has no
	// cert to present yet (see deploy/nginx-term-bridge-relay.conf's optional
	// ssl_verify_client), mirroring handleRegisterTenant's bootstrap story
	// for agents. /devices/pair additionally gets the same per-IP throttle
	// as /tenants/register, since it's equally reachable with no credential.
	pairing.Mount(mux, r.store, r.agentOnly, func(req *http.Request) bool {
		return r.devicePairLimiter.allow(r.clientIP(req))
	}, r.fcm)
	// Device admin (internal/devices), agent-CN-gated by the same resolver:
	// an agent enumerates and revokes only its own tenant's devices.
	devices.Mount(mux, r.store, r.agentOnly)
	mux.Handle("POST /tenants/register", http.HandlerFunc(r.handleRegisterTenant))
	mux.Handle("POST /devices/register", r.notAgent(auth.Require(r.store, http.HandlerFunc(r.handleRegister))))
	mux.Handle("POST /devices/test-push", r.notAgent(auth.Require(r.store, http.HandlerFunc(r.handleTestPush))))
	// Device-facing, not agent-facing: the phone retiring its own credential
	// on Forget. Terminated here rather than proxied -- the relay owns the
	// token, and the request carries no cmux content for the agent to see.
	devices.MountSelfRevoke(mux, r.store, func(h http.Handler) http.Handler {
		return r.notAgent(auth.Require(r.store, h))
	})
	mux.Handle("/", r.notAgent(auth.Require(r.store, r.logProxy(r.proxy))))
	if r.edgeToken == "" {
		return mux
	}
	return r.requireEdge(mux)
}

// requireEdge gates every route except /healthz on a shared secret the
// trusted edge (nginx) injects. Constant-time compare so the secret can't be
// probed by timing.
func (r *Relay) requireEdge(next http.Handler) http.Handler {
	want := []byte(r.edgeToken)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/healthz" &&
			subtle.ConstantTimeCompare([]byte(req.Header.Get("X-Edge-Token")), want) != 1 {
			httpjson.Error(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, req)
	})
}

// notAgent rejects requests bearing a verified agent CN on non-tunnel
// routes.
func (r *Relay) notAgent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if _, ok := r.verifiedAgentTenant(req); ok {
			httpjson.Error(w, http.StatusForbidden, "forbidden")
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (r *Relay) handleTunnel(w http.ResponseWriter, req *http.Request) {
	tenantID, ok := r.verifiedAgentTenant(req)
	if !ok {
		httpjson.Error(w, http.StatusForbidden, "forbidden")
		return
	}
	sess, err := tunnel.Accept(w, req)
	if err != nil {
		return
	}
	// A completed WS upgrade doesn't prove the agent is actually driving the
	// yamux session -- bound the upgrade-to-live latency with a ping (itself
	// bounded by yamuxCfg's ConnectionWriteTimeout) before registering the
	// tunnel as "up" and replacing any prior session for this tenant.
	if _, err := sess.Ping(); err != nil {
		_ = sess.Close()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.reg.Set(tenantID, sess, cancel)
	slog.Info("relay: agent tunnel up", "tenant_id", tenantID)
	if r.onSession != nil {
		go r.onSession(ctx, tenantID, sess)
	}
	<-sess.CloseChan() // block until the tunnel dies
	slog.Info("relay: agent tunnel down", "tenant_id", tenantID)
	r.reg.Clear(tenantID, sess)
	cancel()
}

type registerReq struct {
	FCMToken string `json:"fcm_token"`
}

func (r *Relay) handleRegister(w http.ResponseWriter, req *http.Request) {
	dev, ok := auth.DeviceFromContext(req.Context())
	if !ok {
		httpjson.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, 4<<10)
	var rq registerReq
	if err := json.NewDecoder(req.Body).Decode(&rq); err != nil || rq.FCMToken == "" {
		httpjson.Error(w, http.StatusBadRequest, "missing fcm_token")
		return
	}
	if err := r.store.SetFCMToken(auth.BearerToken(req), rq.FCMToken); err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			httpjson.Error(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		slog.Error("relay: set fcm token", "tenant_id", dev.TenantID, "device", dev.HashSuffix, "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal_error")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]bool{"ok": true})
}

type registerTenantReq struct {
	CSR string `json:"csr"` // PEM-encoded PKCS#10 certificate signing request
}

type registerTenantResp struct {
	TenantID string `json:"tenant_id"`
	CertPEM  string `json:"cert_pem"`
}

// handleRegisterTenant mints a brand-new tenant identity for a Mac agent that
// has none yet. Reachable without a client cert by design (see
// deploy/nginx-term-bridge-relay-bootstrap.conf) — an unregistered agent has no cert
// to present. Rate limiting / abuse resistance is a known, tracked gap (see
// the design doc's non-goals) — this handler does only basic input hygiene.
func (r *Relay) handleRegisterTenant(w http.ResponseWriter, req *http.Request) {
	if r.ca == nil {
		httpjson.Error(w, http.StatusServiceUnavailable, "registration_unavailable")
		return
	}
	if !r.registerLimiter.allow(r.clientIP(req)) {
		httpjson.Error(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	if n, err := r.store.TenantCount(); err != nil {
		slog.Error("relay: tenant count", "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal_error")
		return
	} else if n >= r.maxTenants {
		httpjson.Error(w, http.StatusServiceUnavailable, "tenant_limit_reached")
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, 8<<10) // CSRs are a few hundred bytes
	var rq registerTenantReq
	if err := json.NewDecoder(req.Body).Decode(&rq); err != nil || rq.CSR == "" {
		httpjson.Error(w, http.StatusBadRequest, "missing csr")
		return
	}
	tenantID, err := r.store.CreateTenant()
	if err != nil {
		slog.Error("relay: create tenant", "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal_error")
		return
	}
	certPEM, serial, err := r.ca.SignCSR([]byte(rq.CSR), agentCNPrefix+tenantID, agentCertValidity)
	if err != nil {
		// The tenant row was already created above; a bad CSR must not leave
		// it active-but-unusable, so revoke it rather than orphan it.
		if !r.store.RevokeTenant(tenantID) {
			slog.Error("relay: failed to revoke orphaned tenant after invalid CSR", "tenant_id", tenantID)
		}
		httpjson.Error(w, http.StatusBadRequest, "invalid_csr")
		return
	}
	if err := r.store.RecordAgentCert(tenantID, serial); err != nil {
		slog.Error("relay: record agent cert", "tenant_id", tenantID, "err", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(registerTenantResp{
		TenantID: tenantID,
		CertPEM:  string(certPEM),
	})
	slog.Info("relay: registered new tenant", "tenant_id", tenantID)
}
