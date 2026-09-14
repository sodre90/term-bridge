package main

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/backoff"
	"github.com/sodre90/term-bridge/internal/config"
)

const (
	// directListenBackoffMin/Max pace retries of the direct listener. The same
	// 1s..30s the relay dial loop uses: whatever stopped the listener binding
	// -- Tailscale still coming up, an interface being rebuilt -- is usually
	// gone within a minute, and 30s is cheap enough to keep trying forever.
	directListenBackoffMin = time.Second
	directListenBackoffMax = 30 * time.Second

	// directOutageSummaryPeriod is how often a still-broken listener is worth
	// mentioning again. Matches the relay's, and for the same reason
	// (cmux-app-5v1): a retry loop that logs every attempt buries the log it
	// is written into.
	directOutageSummaryPeriod = 10 * time.Minute
)

// runDirectListener keeps the direct (Tailscale) listener up for as long as
// ctx lives, retrying with backoff exactly as the relay dial loop does.
//
// It used to be a single goroutine that ran serveDirect once, logged whatever
// it returned, and exited. That is how the standby disappeared for fourteen
// days (cmux-app-t5x): the agent started at 2026-08-24 18:13 before Tailscale
// had assigned its address, "bind: can't assign requested address" was logged
// once, the goroutine exited, and nothing tried again until a restart on
// 09-07 happened to win the race. Nothing else noticed, because the relay was
// fine the whole time -- which is precisely the situation the standby exists
// for, and precisely when it would not have been there.
//
// The asymmetry was the bug: the primary transport reconnected forever while
// the standby got one attempt.
func runDirectListener(
	ctx context.Context,
	cfg config.AgentConfig,
	store *auth.Store,
	tenantID string,
	handler http.Handler,
	health *directHealth,
) {
	certDir := filepath.Join(filepath.Dir(cfg.DirectAuthStore), "direct-certs")
	retryDirectListener(ctx, cfg.DirectListen, health,
		directListenBackoffMin, directListenBackoffMax,
		func() error {
			return serveDirect(ctx, cfg.DirectListen, certDir, store, tenantID, handler, health, fcmClientConfig(cfg))
		})
}

// retryDirectListener is the loop itself, split from [runDirectListener] for
// the same reason [serveDirectListener] is split from [serveDirect]: serve
// needs a live Tailscale daemon and this does not, so the retry behaviour --
// the whole point of the change -- is testable without one.
// The backoff bounds are parameters rather than the constants directly, the
// same way runReaper takes its own cadence, so a test can drive many attempts
// without waiting out a real one.
func retryDirectListener(
	ctx context.Context,
	listenAddr string,
	health *directHealth,
	backoffMin, backoffMax time.Duration,
	serve func() error,
) {
	retry := backoff.New(backoffMin, backoffMax)
	log := newDirectListenLog(listenAddr)
	for ctx.Err() == nil {
		bindsBefore := health.binds()
		err := serve()
		health.markUnbound()
		if ctx.Err() != nil {
			return
		}
		// Whether the attempt got as far as listening is what separates "could
		// not start" from "was serving and then stopped", and it is what makes
		// the next failure a fresh outage rather than a continuing one.
		if health.binds() > bindsBefore {
			log.wasUp()
			retry = backoff.New(backoffMin, backoffMax)
		}
		log.failed(err)
		if !backoff.Sleep(ctx, retry.Next()) {
			return
		}
	}
}

// directListenLog decides what each failed attempt is worth saying, on the
// same policy as [relayDialLog]: the first failure, any change of cause, and a
// periodic reminder -- never one line per attempt.
type directListenLog struct {
	listenAddr string
	now        func() time.Time

	down           bool
	since          time.Time
	attempts       int
	lastErr        string
	lastSummarized time.Time
}

func newDirectListenLog(listenAddr string) *directListenLog {
	return &directListenLog{listenAddr: listenAddr, now: time.Now}
}

// wasUp records that the listener had been serving before this failure, so the
// failure that follows starts a new outage rather than extending an old one.
func (l *directListenLog) wasUp() {
	if l.down {
		slog.Info("agent: direct listener working again",
			"listen", l.listenAddr,
			"down_for", l.now().Sub(l.since).Round(time.Second),
			"attempts", l.attempts)
	}
	l.down = false
	l.attempts = 0
	l.lastErr = ""
}

func (l *directListenLog) failed(err error) {
	msg := errText(err)
	l.attempts++
	switch {
	case !l.down:
		l.down = true
		l.since = l.now()
		l.lastSummarized = l.now()
		l.lastErr = msg
		slog.Error("agent: direct listener down, retrying quietly until it recovers",
			"listen", l.listenAddr, "err", msg)
	case msg != l.lastErr:
		l.lastErr = msg
		l.lastSummarized = l.now()
		slog.Error("agent: direct listener still down, and for a different reason now",
			"listen", l.listenAddr, "err", msg,
			"down_for", l.now().Sub(l.since).Round(time.Second),
			"attempts", l.attempts)
	case l.now().Sub(l.lastSummarized) >= directOutageSummaryPeriod:
		l.lastSummarized = l.now()
		slog.Error("agent: direct listener still down",
			"listen", l.listenAddr, "err", msg,
			"down_for", l.now().Sub(l.since).Round(time.Second),
			"attempts", l.attempts)
	}
}

func errText(err error) string {
	if err == nil {
		return "listener stopped without an error"
	}
	return err.Error()
}
