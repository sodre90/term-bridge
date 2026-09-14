package relay

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/sodre90/term-bridge/internal/auth"
)

// How long a socket belonging to a revoked device may keep streaming. The
// store is authoritative the instant a row is deleted; this only bounds how
// long an already-open connection outlives it.
const connSweepPeriod = 30 * time.Second

// ConnTracker holds the agent-bound streams the proxy dialed, keyed by the
// token hash of the device that asked for them. It exists so revocation can
// reach connections that authenticated before the row went away: both device
// CLIs delete rows from a different process than the one serving the socket,
// so nothing in a revocation path can close these directly (see
// docs/superpowers/specs/2026-08-13-revocation-teardown-design.md).
type ConnTracker struct {
	mu    sync.Mutex
	conns map[string]map[*trackedConn]struct{}
}

func NewConnTracker() *ConnTracker {
	return &ConnTracker{conns: map[string]map[*trackedConn]struct{}{}}
}

// Track registers conn under tokenHash and returns a wrapper that
// deregisters itself when closed, so a connection ended by ordinary traffic
// leaves nothing behind and the map cannot grow with request count.
func (t *ConnTracker) Track(tokenHash, tenantID string, conn net.Conn) net.Conn {
	tracked := &trackedConn{Conn: conn, tracker: t, tokenHash: tokenHash, tenantID: tenantID}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conns[tokenHash] == nil {
		t.conns[tokenHash] = map[*trackedConn]struct{}{}
	}
	t.conns[tokenHash][tracked] = struct{}{}
	return tracked
}

func (t *ConnTracker) forget(c *trackedConn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	peers := t.conns[c.tokenHash]
	delete(peers, c)
	if len(peers) == 0 {
		delete(t.conns, c.tokenHash)
	}
}

// CloseRevoked closes every tracked connection whose device is no longer one
// live represents, and returns how many it closed. Devices absent from live
// are gone from the store; a device whose tenant has been revoked is closed
// even though its own row survives, because that is what the tenant
// revocation means.
func (t *ConnTracker) CloseRevoked(live map[string]string, tenantActive func(string) bool) int {
	t.mu.Lock()
	var doomed []*trackedConn
	for tokenHash, peers := range t.conns {
		tenantID, stillListed := live[tokenHash]
		if stillListed && tenantActive(tenantID) {
			continue
		}
		for c := range peers {
			doomed = append(doomed, c)
		}
	}
	t.mu.Unlock()

	// Closing outside the lock: Close deregisters through forget, which takes
	// the same mutex.
	for _, c := range doomed {
		slog.Info("relay: closing revoked device's connection", "tenant_id", c.tenantID)
		_ = c.Close()
	}
	return len(doomed)
}

// SweepRevoked re-runs the proxy's connect-time check against every live
// connection on a timer, which is what lets an out-of-process revocation --
// `term-bridge-relay devices revoke`, or anything else that edits the store -- take
// effect on connections that are already open.
func (t *ConnTracker) SweepRevoked(ctx context.Context, store *auth.Store, period time.Duration) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			live := map[string]string{}
			for _, dev := range store.List() {
				live[dev.TokenHash] = dev.TenantID
			}
			t.CloseRevoked(live, store.TenantActive)
		}
	}
}

type trackedConn struct {
	net.Conn
	tracker   *ConnTracker
	tokenHash string
	tenantID  string
	once      sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { c.tracker.forget(c) })
	return c.Conn.Close()
}
