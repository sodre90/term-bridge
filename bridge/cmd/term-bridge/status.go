package main

import (
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/sodre90/term-bridge/internal/config"
	"github.com/sodre90/term-bridge/internal/status"
)

// runStatus implements `term-bridge status`: read the snapshot the running
// `term-bridge agent` process last wrote (internal/status) and print a
// human-readable summary. It never talks to the agent process directly --
// see internal/status's package doc for why a status file was chosen over a
// live query.
func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultAgentConfigPath(), "path to agent.toml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.LoadAgent(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load agent config:", err)
		return 1
	}
	snap, err := status.Read(cfg.StatusFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no status available at %s (%v) -- is `term-bridge agent` running?\n", cfg.StatusFile, err)
		return 1
	}
	printStatus(os.Stdout, cfg.Host, snap)
	return 0
}

// printStatus labels the backend line by the configured host kind (cmux or
// tmux), which is what the snapshot's LastCmuxReachedAt actually tracks.
func printStatus(w io.Writer, hostKind string, snap status.Snapshot) {
	_, _ = fmt.Fprintf(w, "as of:           %s\n", formatAgo(snap.WrittenAt))
	_, _ = fmt.Fprintf(w, "relay tunnel:    %s\n", upDown(snap.RelayTunnelUp))
	if snap.DirectModeEnabled {
		_, _ = fmt.Fprintf(w, "direct listener: %s\n", describeDirectListener(snap))
	} else {
		_, _ = fmt.Fprintln(w, "direct listener: disabled")
	}
	_, _ = fmt.Fprintf(w, "%-16s %s\n", hostKind+" reached:", formatTimeOrNever(snap.LastCmuxReachedAt))
	_, _ = fmt.Fprintf(w, "last event:      %s\n", formatTimeOrNever(snap.LastEventAt))
	printSlotReachability(w, snap.SlotLastReachedAt)
	printCounters(w, snap.Counters)
}

// staleSlotAfter is how long a slot may go unreached before the line says so
// outright. Two missed hourly rounds: one is a blip, three hours of silence is
// a transport that is not there.
const staleSlotAfter = 3 * time.Hour

// printSlotReachability reports when each transport last answered the hourly
// device listing -- the only end-to-end probe the agent runs against its own
// slots, and the one that would have shown the direct standby was unreachable
// for fourteen days instead of leaving it to an hourly WARN (cmux-app-t5x).
//
// These timestamps survive restarts, so an age here is a real outage length
// rather than time since the agent last started.
func printSlotReachability(w io.Writer, reached map[string]time.Time) {
	if len(reached) == 0 {
		_, _ = fmt.Fprintln(w, "slots reached:   no completed device round yet (the first is 2 min after start, then hourly)")
		return
	}
	_, _ = fmt.Fprintln(w, "slots reached:")
	for _, slot := range slices.Sorted(maps.Keys(reached)) {
		_, _ = fmt.Fprintf(w, "  %-32s %s\n", slot, describeSlotReach(reached[slot]))
	}
}

func describeSlotReach(at time.Time) string {
	switch {
	case at.IsZero():
		return "NEVER -- this transport has not answered once"
	case time.Since(at) >= staleSlotAfter:
		return fmt.Sprintf("%s -- UNREACHABLE since then", formatAgo(at))
	default:
		return formatAgo(at)
	}
}

// printCounters lists the agent's expvar totals, sorted so two readings taken
// apart can be diffed line by line -- which is the only way to read them, since
// they count from process start and reset on restart. Zeroes are printed too:
// "this has never happened" is a different answer from "this counter is gone".
func printCounters(w io.Writer, counters map[string]int64) {
	if len(counters) == 0 {
		_, _ = fmt.Fprintln(w, "counters:        none (agent predates this field, or has not written one yet)")
		return
	}
	_, _ = fmt.Fprintln(w, "counters:")
	for _, name := range slices.Sorted(maps.Keys(counters)) {
		_, _ = fmt.Fprintf(w, "  %-32s %d\n", name, counters[name])
	}
}

// describeDirectListener says what the listener is doing, not merely that it
// is bound. Both ways a bound listener can be useless -- nothing reaching it
// (a firewall, an ACL) and connections reaching it but never completing
// (TLS) -- used to print as a plain "up", which is how the direct half of
// dual-pairing stayed dead for days with the only operator-visible signal
// calling it healthy (cmux-app-0no).
func describeDirectListener(snap status.Snapshot) string {
	switch {
	case !snap.DirectListenerUp:
		return "down"
	case !snap.DirectLastServedAt.IsZero():
		return fmt.Sprintf("up, last served %s", formatAgo(snap.DirectLastServedAt))
	case snap.DirectConnectionsAccepted == 0:
		return "bound, but nothing has ever connected -- check the macOS firewall and Tailscale ACLs"
	default:
		return fmt.Sprintf("bound, but none of %d connections got as far as a request -- check TLS",
			snap.DirectConnectionsAccepted)
	}
}

func upDown(up bool) string {
	if up {
		return "up"
	}
	return "down"
}

func formatTimeOrNever(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return formatAgo(t)
}

func formatAgo(t time.Time) string {
	return fmt.Sprintf("%s (%s ago)", t.Local().Format(time.RFC3339), time.Since(t).Round(time.Second))
}
