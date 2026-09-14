package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sodre90/term-bridge/internal/e2e"
)

// How long a socket belonging to an unpaired device may keep streaming. The
// store is authoritative the instant the secret is removed; this only bounds
// how long an already-open socket outlives it. Matches the relay's
// connSweepPeriod -- the two sweeps answer the same question about the same
// device, so they should not disagree about how stale an answer may be.
const socketSweepPeriod = 30 * time.Second

// socketTracker holds a teardown func per live device socket, keyed by device
// id, so revocation can reach sockets that authenticated before the device
// was unpaired. Needed for the same reason as the relay's ConnTracker: both
// device CLIs remove state from a different process than the one serving the
// socket (see
// docs/superpowers/specs/2026-08-13-revocation-teardown-design.md).
//
// A teardown func rather than a context.CancelFunc because the two handlers
// stop differently: /terminal's loops exit on context cancellation, while
// /events blocks on its frame channel and has to be unblocked by closing the
// socket itself.
type socketTracker struct {
	mu      sync.Mutex
	sockets map[string]map[*trackedSocket]struct{}
}

func newSocketTracker() *socketTracker {
	return &socketTracker{sockets: map[string]map[*trackedSocket]struct{}{}}
}

// track registers teardown under deviceID and returns the func to call when
// the socket ends on its own, so an ordinary disconnect leaves nothing
// behind.
func (t *socketTracker) track(deviceID string, teardown func()) (release func()) {
	socket := &trackedSocket{deviceID: deviceID, teardown: teardown}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sockets[deviceID] == nil {
		t.sockets[deviceID] = map[*trackedSocket]struct{}{}
	}
	t.sockets[deviceID][socket] = struct{}{}
	return func() { t.forget(socket) }
}

func (t *socketTracker) forget(socket *trackedSocket) {
	t.mu.Lock()
	defer t.mu.Unlock()
	peers := t.sockets[socket.deviceID]
	delete(peers, socket)
	if len(peers) == 0 {
		delete(t.sockets, socket.deviceID)
	}
}

// closeUnpaired tears down every tracked socket whose device stillPaired
// rejects, and returns how many it tore down.
func (t *socketTracker) closeUnpaired(stillPaired func(string) bool) int {
	t.mu.Lock()
	var doomed []*trackedSocket
	for deviceID, peers := range t.sockets {
		if stillPaired(deviceID) {
			continue
		}
		for socket := range peers {
			doomed = append(doomed, socket)
		}
	}
	t.mu.Unlock()

	for _, socket := range doomed {
		slog.Info("server: closing unpaired device's socket", "device", deviceLogID(socket.deviceID))
		socket.close()
	}
	return len(doomed)
}

type trackedSocket struct {
	deviceID string
	teardown func()
	once     sync.Once
}

func (s *trackedSocket) close() { s.once.Do(s.teardown) }

// SweepUnpairedSockets closes the sockets of devices unpaired since they
// connected, until ctx is done. Blocks; run it in a goroutine. No-op without
// an e2e store: that is the plaintext test configuration, where there is no
// device identity to key on and every socket is untracked anyway.
func (s *Server) SweepUnpairedSockets(ctx context.Context) {
	if s.sessions == nil {
		return
	}
	sweepUnpairedSockets(ctx, s.sockets, s.sessions, socketSweepPeriod)
}

func sweepUnpairedSockets(ctx context.Context, tracker *socketTracker, sessions *e2e.Store, period time.Duration) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tracker.closeUnpaired(func(deviceID string) bool {
				_, paired := sessions.SharedSecret(deviceID)
				return paired
			})
		}
	}
}
