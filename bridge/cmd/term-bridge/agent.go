package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/backoff"
	"github.com/sodre90/term-bridge/internal/cli"
	"github.com/sodre90/term-bridge/internal/cmux"
	"github.com/sodre90/term-bridge/internal/config"
	"github.com/sodre90/term-bridge/internal/e2e"
	"github.com/sodre90/term-bridge/internal/host"
	"github.com/sodre90/term-bridge/internal/host/cmuxhost"
	"github.com/sodre90/term-bridge/internal/host/tmuxhost"
	"github.com/sodre90/term-bridge/internal/logging"
	"github.com/sodre90/term-bridge/internal/metrics"
	"github.com/sodre90/term-bridge/internal/push"
	"github.com/sodre90/term-bridge/internal/server"
	"github.com/sodre90/term-bridge/internal/status"
	"github.com/sodre90/term-bridge/internal/tmux"
	"github.com/sodre90/term-bridge/internal/tunnel"
	"github.com/sodre90/term-bridge/internal/wire"
	"github.com/sodre90/term-bridge/internal/yolo"
)

// statusWriteInterval is how often runAgent persists its status.Snapshot to
// disk for `term-bridge status` to read. Short enough that an operator never
// waits long for a state change to show up, long enough that it's a
// negligible amount of disk I/O.
const statusWriteInterval = 5 * time.Second

// certRefreshInterval is how often serveDirect re-checks the direct-mode
// TLS certificate's remaining validity via `tailscale cert`.
// certMinValidity is the minimum remaining validity requested each time --
// well short of Let's Encrypt's 90-day lifetime, so repeated calls within
// that window are cheap no-ops rather than forcing a fresh issuance.
const (
	certRefreshInterval = 24 * time.Hour
	certMinValidity     = 30 * 24 * time.Hour
)

func defaultAgentConfigPath() string {
	return cli.ConfigPath("term-bridge", "agent.toml")
}

// attachmentSweepInterval is how often landed images past their retention
// are removed; once at start, then daily.
const attachmentSweepInterval = 24 * time.Hour

func sweepAttachments(ctx context.Context, store *server.AttachmentStore) {
	sweep := func() {
		removed, err := store.Sweep()
		switch {
		case err != nil:
			slog.Warn("agent: attachment sweep failed", "err", err)
		case removed > 0:
			slog.Info("agent: attachments swept", "removed", removed)
		}
	}
	sweep()
	ticker := time.NewTicker(attachmentSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

func loadTLS(certPath, keyPath, caPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	// An empty ca_cert means the relay's nginx presents a publicly-trusted
	// server cert (e.g. Let's Encrypt): leave RootCAs nil so Go uses the system
	// roots. A non-empty ca_cert pins a private CA instead.
	if caPath != "" {
		caPEM, err := os.ReadFile(caPath)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("ca_cert: no certificates found")
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// dialAndServe runs one tunnel lifecycle: dial the relay, then serve the handler
// over the yamux session until it dies. Returns when the session ends.
// onConnected, if not nil, is called once the dial succeeds, before serving
// -- runAgent uses it to drive the relay-tunnel-up status for `term-bridge
// status`.
func dialAndServe(ctx context.Context, relayURL string, tlsCfg *tls.Config, handler http.Handler, onConnected func()) error {
	sess, err := tunnel.Dial(ctx, relayURL, tlsCfg, nil)
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()
	if onConnected != nil {
		onConnected()
	}
	return http.Serve(sess, handler)
}

// ensureDirectTenant returns direct mode's single implicit tenant id,
// creating it once on first use. Idempotent across restarts: an existing
// tenant is always reused, so toggling direct_listen off and back on never
// orphans devices paired while it was on.
func ensureDirectTenant(store *auth.Store) (string, error) {
	tenants, err := store.ListTenants()
	if err != nil {
		return "", err
	}
	for _, t := range tenants {
		if !t.Revoked {
			return t.ID, nil
		}
	}
	return store.CreateTenant()
}

// fcmClientConfig assembles the client-side Firebase config handed to a phone
// at pairing. An incomplete set stays incomplete rather than being patched up
// with defaults: wire.FCMClientConfig.Configured then reports false and the
// pairing response omits the block, which is what a bridge without push
// configured should look like on the wire.
func fcmClientConfig(cfg config.AgentConfig) wire.FCMClientConfig {
	return wire.FCMClientConfig{
		ProjectID: cfg.FCMProjectID,
		AppID:     cfg.FCMAppID,
		APIKey:    cfg.FCMAPIKey,
		SenderID:  cfg.FCMSenderID,
	}
}

// warnFCMClientConfig says out loud what a phone can never tell the operator:
// that the Firebase config it would have been handed at pairing is not going
// to be sent. Both cases are silent otherwise -- the pairing response simply
// omits the block, and push stays dead on every phone forever.
func warnFCMClientConfig(c wire.FCMClientConfig, canSend bool) {
	switch {
	case c.PartiallyConfigured():
		slog.Warn("fcm client config incomplete -- withholding it from pairing; phones will not receive push",
			"have_project_id", c.ProjectID != "", "have_app_id", c.AppID != "",
			"have_api_key", c.APIKey != "", "have_sender_id", c.SenderID != "")
	case canSend && !c.Configured():
		slog.Warn("fcm credentials are set but the client config is not -- only phones with a google-services.json compiled in will receive push; set fcm_app_id, fcm_api_key and fcm_sender_id")
	}
}

// directListenPort extracts the port from cfg.DirectListen, which is always
// documented and configured in ":PORT" form (e.g. ":8443" -- see
// bridge/README.md and bridge/deploy/agent.example.toml, and
// internal/config/agent_test.go's TestLoadAgentParsesDirectFields). A bare
// leading-colon address has no host part, so net.SplitHostPort happily
// returns an empty host and just the port.
func directListenPort(listenAddr string) (string, error) {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("direct_listen %q: %w", listenAddr, err)
	}
	return port, nil
}

// selfTailscaleIPv4 returns this node's own Tailscale IPv4 address from a
// tailscale status snapshot. A node can have both an IPv4 and IPv6 tailnet
// address; we bind to the IPv4 one, matching `tailscale ip -4` (the
// convention bridge/README.md's setup steps already use). Returns an error
// if Tailscale isn't up yet or hasn't assigned this node an IPv4 address --
// callers must fail closed on that, not fall back to binding all
// interfaces, since Tailscale's own network ACLs are the actual
// access-control boundary for the direct-mode listener.
func selfTailscaleIPv4(st *tailscaleSelf) (netip.Addr, error) {
	if st == nil {
		return netip.Addr{}, errors.New("no Self status -- is Tailscale up?")
	}
	for _, ip := range st.TailscaleIPs {
		if ip.Is4() {
			return ip, nil
		}
	}
	return netip.Addr{}, errors.New("no Tailscale IPv4 address assigned yet -- is Tailscale up?")
}

// refreshDirectCert re-runs tailscaleCert on a ticker for as long as ctx is
// live, atomically swapping certVal to the reloaded certificate on success.
// A failed refresh (tailscaled down, transient ACME error) logs and keeps
// serving the previous certificate rather than tearing down the listener.
func refreshDirectCert(ctx context.Context, domain, certFile, keyFile string, certVal *atomic.Pointer[tls.Certificate]) {
	ticker := time.NewTicker(certRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := tailscaleCert(ctx, domain, certFile, keyFile, certMinValidity); err != nil {
			slog.Warn("agent: direct mode: cert refresh failed, keeping current cert", "err", err)
			continue
		}
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			slog.Warn("agent: direct mode: cert refresh: reload failed, keeping current cert", "err", err)
			continue
		}
		certVal.Store(&cert)
	}
}

// serveDirect runs the direct (Tailscale) listener until ctx is canceled or
// the listener fails. It never affects the relay dial loop running
// alongside it in runAgent. store/tenantID back both the pairing routes and
// (via handler, already bound to the same store through Server.store)
// authenticated requests -- one auth.Store, opened once, for the whole
// listener. certDir holds the direct-mode TLS certificate/key files
// (refreshed periodically via tailscaleCert); it's created if missing.
//
// It binds ONLY to this Mac's own Tailscale IPv4 address, never to all
// interfaces: the design's whole premise is that Tailscale's own network
// ACLs are the access-control boundary for these routes (some of which,
// like the pairing endpoints, are otherwise unauthenticated), so listening
// on 0.0.0.0/[::] here would let any LAN-adjacent device reach them too.
// health, if not nil, is where the listener reports itself for
// `term-bridge status`.
func serveDirect(ctx context.Context, listenAddr, certDir string, store *auth.Store, tenantID string, handler http.Handler, health *directHealth, fcm wire.FCMClientConfig) error {
	mux := http.NewServeMux()
	server.MountDirectPairing(mux, store, tenantID, fcm)
	mux.Handle("/", handler)

	port, err := directListenPort(listenAddr)
	if err != nil {
		return fmt.Errorf("direct mode: %w", err)
	}
	st, err := tailscaleSelfStatus(ctx)
	if err != nil {
		return fmt.Errorf("direct mode: %w", err)
	}
	ip, err := selfTailscaleIPv4(st)
	if err != nil {
		return fmt.Errorf("direct mode: %w", err)
	}
	if st.DNSName == "" {
		return errors.New("direct mode: this Mac has no Tailscale DNS name yet -- is Tailscale up?")
	}
	domain := strings.TrimSuffix(st.DNSName, ".")
	bindAddr := net.JoinHostPort(ip.String(), port)

	if err := os.MkdirAll(certDir, 0o700); err != nil {
		return fmt.Errorf("direct mode: cert dir: %w", err)
	}
	certFile := filepath.Join(certDir, "direct-cert.pem")
	keyFile := filepath.Join(certDir, "direct-key.pem")
	if err := tailscaleCert(ctx, domain, certFile, keyFile, certMinValidity); err != nil {
		return fmt.Errorf("direct mode: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("direct mode: load cert: %w", err)
	}
	var certVal atomic.Pointer[tls.Certificate]
	certVal.Store(&cert)
	refreshCtx, cancelRefresh := context.WithCancel(ctx)
	defer cancelRefresh()
	go refreshDirectCert(refreshCtx, domain, certFile, keyFile, &certVal)

	tcpLn, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return fmt.Errorf("direct mode: listen %s: %w", bindAddr, err)
	}
	tlsLn := tls.NewListener(countingListener{Listener: tcpLn, health: health}, &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return certVal.Load(), nil
		},
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
	})

	slog.Info("agent: direct listener up (tailscale-only)", "addr", bindAddr)
	return serveDirectListener(ctx, tlsLn, mux, health)
}

// serveDirectListener serves handler on an already-bound ln until ctx is
// canceled or serving fails, reporting to health as it goes. Split from
// serveDirect so the health reporting can be tested against a real TLS
// listener: everything above this point needs a live Tailscale daemon, and
// nothing below it does.
func serveDirectListener(ctx context.Context, ln net.Listener, handler http.Handler, health *directHealth) error {
	health.markBound()
	srv := &http.Server{Handler: markServedByDevice(health, handler)}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		_ = srv.Close()
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// agentAdminPrefix is this agent's own device-admin API (see
// internal/devices.Mount). No phone ever calls it; the hourly reaper in this
// same process does, against every configured server -- and the direct
// listener is one of them.
const agentAdminPrefix = "/agent/"

// markServedByDevice records that a request from something other than this
// agent's own admin client reached the listener.
//
// Reaching a handler proves the TLS handshake completed, which bind success
// alone never did. Excluding the admin API is what stops the reaper's hourly
// round from advancing direct_last_served_at on its own behalf -- that made
// the only signal `term-bridge status` offers about the standby transport
// self-satisfying, reporting it healthy on a day when every phone request to
// it 401'd (cmux-app-8d3). Excluding a caller can only ever under-report,
// which is the safe direction for a health signal to fail in.
func markServedByDevice(health *directHealth, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, agentAdminPrefix) {
			health.markServed()
		}
		next.ServeHTTP(w, r)
	})
}

// directHealth is what the direct listener reports about itself for
// `term-bridge status`. A nil *directHealth is usable and records nothing,
// so tests and any future caller that doesn't want the reporting can pass
// one.
type directHealth struct {
	bound      atomic.Bool
	accepted   atomic.Int64
	lastServed atomic.Value // time.Time
	// boundTimes counts successful binds, not the current state, so a retry
	// loop can tell an attempt that never started from one that was serving
	// and then stopped -- markUnbound erases the difference (cmux-app-t5x).
	boundTimes atomic.Int64
}

func (h *directHealth) markBound() {
	if h != nil {
		h.bound.Store(true)
		h.boundTimes.Add(1)
	}
}

func (h *directHealth) binds() int64 {
	if h == nil {
		return 0
	}
	return h.boundTimes.Load()
}

func (h *directHealth) markUnbound() {
	if h != nil {
		h.bound.Store(false)
	}
}

func (h *directHealth) markAccepted() {
	if h != nil {
		h.accepted.Add(1)
	}
}

func (h *directHealth) markServed() {
	if h != nil {
		h.lastServed.Store(time.Now())
	}
}

func (h *directHealth) snapshot() (bound bool, accepted int64, lastServed time.Time) {
	if h == nil {
		return false, 0, time.Time{}
	}
	t, _ := h.lastServed.Load().(time.Time)
	return h.bound.Load(), h.accepted.Load(), t
}

// countingListener records every accepted connection, which is what tells a
// listener nothing can reach apart from one whose clients are failing later
// (cmux-app-0no).
type countingListener struct {
	net.Listener
	health *directHealth
}

func (l countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.health.markAccepted()
	}
	return conn, err
}

// buildHost picks the terminal backend agent.toml names. The tmux host's
// resize janitor runs for the life of ctx.
func buildHost(ctx context.Context, cfg config.AgentConfig, reached func()) host.Host {
	if cfg.Host == config.HostTmux {
		h := tmuxhost.New(&tmux.Client{Bin: cfg.TmuxBin, Socket: cfg.TmuxSocket, OnReached: reached})
		go h.Run(ctx)
		slog.Info("agent: host is tmux", "tmux_bin", cfg.TmuxBin, "tmux_socket", cfg.TmuxSocket)
		return h
	}
	return cmuxhost.New(&cmux.Client{Bin: cfg.CmuxBin, FastPath: true, OnReached: reached})
}

func runAgent(args []string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultAgentConfigPath(), "path to agent.toml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.LoadAgent(*cfgPath)
	if err != nil {
		slog.Error("agent: load config", "err", err)
		return 1
	}
	// Not fatal: an agent that cannot open its own log file should still run
	// and keep logging to stderr, where launchd will catch it.
	if err := logging.UseRotatingFile(cfg.LogFile); err != nil {
		slog.Error("agent: open log file, staying on stderr", "path", cfg.LogFile, "err", err)
	}
	if err := ensureRegistered(cfg); err != nil {
		slog.Error("agent: register", "err", err)
		return 1
	}
	if cfg.RelayURL == "" {
		slog.Error("agent: relay_url is required")
		return 1
	}
	tlsCfg, err := loadTLS(cfg.ClientCert, cfg.ClientKey, cfg.CACert)
	if err != nil {
		slog.Error("agent: tls", "err", err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var directStore *auth.Store // nil unless direct mode is on; Handler()/authWrap stay unused in production either way
	var directTenantID string
	if cfg.DirectListen != "" {
		var err error
		directStore, err = auth.Open(cfg.DirectAuthStore)
		if err != nil {
			slog.Error("agent: direct mode: open auth store", "err", err)
			return 1
		}
		directTenantID, err = ensureDirectTenant(directStore)
		if err != nil {
			slog.Error("agent: direct mode: ensure tenant", "err", err)
			return 1
		}
	}
	var lastCmuxReached atomic.Value // time.Time
	reached := func() { lastCmuxReached.Store(time.Now()) }
	srv := server.NewWithHost(buildHost(ctx, cfg, reached), directStore)
	sessions, err := e2e.OpenStore(cfg.SessionStore)
	if err != nil {
		slog.Error("agent: open session store", "err", err)
		return 1
	}
	srv.SetSessions(sessions)

	yoloStore, err := yolo.Open(cfg.YoloStore)
	if err != nil {
		slog.Error("agent: open yolo store", "err", err)
		return 1
	}
	srv.SetYoloStore(yoloStore)
	attachments := server.NewAttachmentStore(cfg.AttachmentsDir)
	srv.SetAttachmentStore(attachments)
	go sweepAttachments(ctx, attachments)
	warnFCMClientConfig(fcmClientConfig(cfg), cfg.FCMCredentials != "")
	if cfg.DirectListen != "" && cfg.FCMProjectID != "" && cfg.FCMCredentials != "" {
		if p, err := push.FromServiceAccount(context.Background(), cfg.FCMProjectID, cfg.FCMCredentials); err != nil {
			slog.Warn("agent: direct-mode push disabled", "err", err)
		} else {
			srv.SetPusher(p, directTenantID)
			slog.Info("agent: direct-mode FCM push enabled", "fcm_project_id", cfg.FCMProjectID)
		}
	}
	go srv.RunEvents(ctx)
	// Seeded from the snapshot the previous process left behind: a per-slot
	// "last reached" that resets on restart cannot show a standby that has
	// been unreachable for days, which is the outage it exists to expose
	// (cmux-app-t5x).
	previous, _ := status.Read(cfg.StatusFile)
	slotReach := status.NewSlotReachability(previous.SlotLastReachedAt)
	go runReaper(ctx, cfg, sessions, reaperFirstRound, reaperPeriod, func(reached map[string]bool) {
		slotReach.Record(reached, time.Now())
	})
	// `term-bridge devices revoke` edits the session store from its own
	// process, so an already-streaming socket never learns about it; this is
	// what closes those.
	go srv.SweepUnpairedSockets(ctx)
	handler := srv.TrustedHandler(cfg.RelayToken)

	var relayTunnelUp atomic.Bool
	directHealth := &directHealth{}
	go status.RunWriter(ctx, cfg.StatusFile, statusWriteInterval, func() status.Snapshot {
		var lastReached time.Time
		if t, ok := lastCmuxReached.Load().(time.Time); ok {
			lastReached = t
		}
		directUp, directAccepted, directLastServed := directHealth.snapshot()
		return status.Snapshot{
			WrittenAt:                 time.Now(),
			RelayTunnelUp:             relayTunnelUp.Load(),
			DirectModeEnabled:         cfg.DirectListen != "",
			DirectListenerUp:          directUp,
			DirectConnectionsAccepted: directAccepted,
			DirectLastServedAt:        directLastServed,
			LastCmuxReachedAt:         lastReached,
			LastEventAt:               srv.LastEventAt(),
			SlotLastReachedAt:         slotReach.Snapshot(),
			Counters:                  metrics.Snapshot(),
		}
	})

	if cfg.DirectListen != "" {
		go runDirectListener(ctx, cfg, directStore, directTenantID, srv.DirectHandler(), directHealth)
	}

	retry := backoff.New(time.Second, 30*time.Second)
	dialLog := newRelayDialLog(cfg.RelayURL)
	for ctx.Err() == nil {
		dialLog.dialing()
		err := dialAndServe(ctx, cfg.RelayURL, tlsCfg, handler, func() {
			relayTunnelUp.Store(true)
			dialLog.up()
		})
		relayTunnelUp.Store(false)
		if err != nil {
			dialLog.failed(err)
		}
		if ctx.Err() != nil {
			break
		}
		backoff.Sleep(ctx, retry.Next())
	}
	return 0
}
