// Package status defines the small on-disk health snapshot the running
// `term-bridge agent` process writes periodically, and `term-bridge status`
// reads, so an operator can ask "is the tunnel up, when did we last reach
// cmux, when was the last event" without reading logs.
//
// A status *file* was chosen over a live query (e.g. a local unix socket)
// deliberately: it needs no new listening surface on a codebase that is
// already careful about what it exposes (see serveDirect's Tailscale-only
// binding, the relay's loopback-by-default listener), at the cost of the
// report being up to one write interval stale -- an acceptable tradeoff for
// an operator-facing health check, not a monitoring system.
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Snapshot is the agent's health snapshot. Zero-value timestamps mean "never
// observed", not "at the Unix epoch" -- callers should check IsZero.
type Snapshot struct {
	WrittenAt time.Time `json:"written_at"`

	// RelayTunnelUp reports whether the outbound yamux tunnel to the relay
	// is currently connected (dialed, and the handler is being served over
	// it) at WrittenAt.
	RelayTunnelUp bool `json:"relay_tunnel_up"`

	// DirectModeEnabled reports whether this agent has direct_listen
	// configured at all. The three fields below are only meaningful when
	// this is true.
	DirectModeEnabled bool `json:"direct_mode_enabled"`

	// DirectListenerUp reports only that the listener is bound. That is a
	// weaker claim than it reads as: a bound socket nothing can reach still
	// reports up, which is how a macOS firewall block stayed invisible for
	// days behind a plain "direct listener: up" (cmux-app-0no). The two
	// fields below separate the ways it can be bound and still useless.
	DirectListenerUp bool `json:"direct_listener_up"`

	// DirectConnectionsAccepted counts connections the direct listener has
	// accepted since the agent started. Zero, while bound, means nothing is
	// arriving at all -- a firewall or a Tailscale ACL, not a TLS fault.
	DirectConnectionsAccepted int64 `json:"direct_connections_accepted"`

	// DirectLastServedAt is when the listener last handled a request from
	// something other than this agent's own admin client, which is the
	// earliest moment a TLS handshake is known to have completed. Never,
	// with connections accepted, means clients are arriving and failing
	// before they deliver anything.
	//
	// The agent's own hourly device reaper is excluded on purpose: it calls
	// every configured server, this listener included, so counting it made
	// the field advance once an hour whether or not a phone had ever reached
	// the standby transport (cmux-app-8d3).
	DirectLastServedAt time.Time `json:"direct_last_served_at"`

	// LastCmuxReachedAt is when a `cmux rpc` call (fast-path socket or
	// subprocess) last completed successfully.
	LastCmuxReachedAt time.Time `json:"last_cmux_reached_at"`
	// LastEventAt is when the agent last processed a frame from its
	// `cmux events --reconnect` stream.
	LastEventAt time.Time `json:"last_event_at"`

	// SlotLastReachedAt records, per transport slot ("relay", "direct"), when
	// the hourly device reaper last listed devices on it successfully. That
	// listing is the only thing the agent does that probes each transport end
	// to end, over the same name and port a phone uses.
	//
	// Unlike every other field here it is CARRIED FORWARD across restarts
	// (see [NewSlotReachability]), which is the whole point: a gauge that
	// resets to "never" whenever the agent restarts cannot answer "has the
	// standby been down for two weeks", and that is exactly the question
	// nothing could answer while it had been (cmux-app-t5x).
	//
	// A slot absent from the map has never been reached since the status file
	// was created. An empty map means no round has completed yet.
	SlotLastReachedAt map[string]time.Time `json:"slot_last_reached_at,omitempty"`

	// Counters is metrics.Snapshot() at WrittenAt: running totals since this
	// agent process started, so they reset on restart and only differences
	// between two readings mean anything.
	//
	// They live here because the agent serves no /debug/vars and is not
	// getting a listener for one -- see this package's doc, and cmux-app-9aa
	// for how long they went unreadable.
	Counters map[string]int64 `json:"counters,omitempty"`
}

// SlotReachability accumulates the reaper's per-slot results across rounds and
// across restarts. Safe for concurrent use: the reaper goroutine records, the
// status writer reads.
//
// Seed it with [Snapshot.SlotLastReachedAt] from the status file the previous
// process left behind, or the first restart erases the outage being looked for.
type SlotReachability struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func NewSlotReachability(seed map[string]time.Time) *SlotReachability {
	seen := make(map[string]time.Time, len(seed))
	maps.Copy(seen, seed)
	return &SlotReachability{seen: seen}
}

// Record stamps every slot that answered. A slot that did NOT answer keeps
// whatever timestamp it already had -- the gap between that and now is the
// outage, so overwriting or clearing it would destroy the measurement.
func (r *SlotReachability) Record(reached map[string]bool, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for kind, ok := range reached {
		if ok {
			r.seen[kind] = at
		} else if _, known := r.seen[kind]; !known {
			// Remember that the slot exists and has never answered; the
			// zero time is what distinguishes it from one never configured.
			r.seen[kind] = time.Time{}
		}
	}
}

func (r *SlotReachability) Snapshot() map[string]time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.seen)
}

// Write atomically persists snap to path as JSON: written to a temp file in
// the same directory, then renamed, so a concurrent Read never observes a
// half-written file.
func Write(path string, snap Snapshot) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("status: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("status: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("status: write temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("status: rename: %w", err)
	}
	return nil
}

// Read loads the snapshot most recently written to path.
func Read(path string) (Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("status: parse %s: %w", path, err)
	}
	return snap, nil
}

// RunWriter persists snapshot() to path immediately, then again every
// interval, until ctx is done. A failed write is logged and retried on the
// next tick rather than treated as fatal -- a status-file hiccup must never
// take down the agent it's reporting on.
func RunWriter(ctx context.Context, path string, interval time.Duration, snapshot func() Snapshot) {
	write := func() {
		if err := Write(path, snapshot()); err != nil {
			slog.Warn("status: write failed", "err", err)
		}
	}
	write()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			write()
		}
	}
}
