package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sodre90/term-bridge/internal/config"
	"github.com/sodre90/term-bridge/internal/status"
)

func TestPrintStatusRelayUpDirectDisabled(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, config.HostCmux, status.Snapshot{
		WrittenAt:         time.Now(),
		RelayTunnelUp:     true,
		DirectModeEnabled: false,
		LastCmuxReachedAt: time.Now(),
		LastEventAt:       time.Time{},
	})
	out := buf.String()
	if !strings.Contains(out, "relay tunnel:    up") {
		t.Fatalf("output missing relay tunnel up: %s", out)
	}
	if !strings.Contains(out, "direct listener: disabled") {
		t.Fatalf("output missing disabled direct listener: %s", out)
	}
	if !strings.Contains(out, "last event:      never") {
		t.Fatalf("output missing never for last event: %s", out)
	}
}

func TestPrintStatusDirectEnabledDown(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, config.HostCmux, status.Snapshot{
		WrittenAt:         time.Now(),
		RelayTunnelUp:     false,
		DirectModeEnabled: true,
		DirectListenerUp:  false,
	})
	out := buf.String()
	if !strings.Contains(out, "relay tunnel:    down") {
		t.Fatalf("output missing relay tunnel down: %s", out)
	}
	if !strings.Contains(out, "direct listener: down") {
		t.Fatalf("output missing direct listener down: %s", out)
	}
}

// cmux-app-0no. Each of these three states used to print the same word,
// "up", including the two where the direct half of dual-pairing was in fact
// dead.
func TestPrintStatusDistinguishesBoundFromWorking(t *testing.T) {
	cases := []struct {
		name string
		snap status.Snapshot
		want string
	}{
		{
			name: "nothing ever reaches it",
			snap: status.Snapshot{DirectModeEnabled: true, DirectListenerUp: true},
			want: "nothing has ever connected",
		},
		{
			name: "connections arrive but never complete",
			snap: status.Snapshot{DirectModeEnabled: true, DirectListenerUp: true, DirectConnectionsAccepted: 7},
			want: "none of 7 connections",
		},
		{
			name: "actually serving",
			snap: status.Snapshot{
				DirectModeEnabled:         true,
				DirectListenerUp:          true,
				DirectConnectionsAccepted: 7,
				DirectLastServedAt:        time.Now(),
			},
			want: "up, last served",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.snap.WrittenAt = time.Now()
			printStatus(&buf, config.HostCmux, tc.snap)
			if !strings.Contains(buf.String(), tc.want) {
				t.Fatalf("output missing %q: %s", tc.want, buf.String())
			}
		})
	}
}

// cmux-app-9aa: these totals have no /debug/vars on the agent to be read
// from, so `term-bridge status` is the only place they surface at all.
func TestPrintStatusListsCountersSortedIncludingZeroes(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, config.HostCmux, status.Snapshot{
		WrittenAt: time.Now(),
		Counters: map[string]int64{
			"push_sent_total":                 12,
			"push_failed_total":               0,
			"e2e_decrypt_failures_total/body": 3,
			"pairing_codes_redeemed_total":    1,
		},
	})
	out := buf.String()

	for _, want := range []string{"push_sent_total", "12", "push_failed_total", "e2e_decrypt_failures_total/body"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %s", want, out)
		}
	}
	// Sorted, so two readings taken apart diff line by line.
	if strings.Index(out, "pairing_codes_redeemed_total") > strings.Index(out, "push_failed_total") {
		t.Fatalf("counters are not sorted: %s", out)
	}
}

func TestPrintStatusSaysSoWhenThereAreNoCounters(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, config.HostCmux, status.Snapshot{WrittenAt: time.Now()})

	if !strings.Contains(buf.String(), "counters:        none") {
		t.Fatalf("output missing the no-counters line: %s", buf.String())
	}
}

func TestRunStatusReadsWrittenSnapshot(t *testing.T) {
	dir := t.TempDir()
	statusPath := filepath.Join(dir, "status.json")
	cfgPath := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(cfgPath, []byte(`status_file = "`+statusPath+`"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := status.Write(statusPath, status.Snapshot{
		WrittenAt:     time.Now(),
		RelayTunnelUp: true,
	}); err != nil {
		t.Fatal(err)
	}

	if got := runStatus([]string{"-config", cfgPath}); got != 0 {
		t.Fatalf("runStatus exit code = %d, want 0", got)
	}
}

func TestRunStatusMissingFileFails(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(cfgPath, []byte(`status_file = "`+filepath.Join(dir, "never-written.json")+`"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := runStatus([]string{"-config", cfgPath}); got != 1 {
		t.Fatalf("runStatus exit code = %d, want 1 for a status file that was never written", got)
	}
}

// -- slot reachability output (cmux-app-t5x)

// The line that would have shown a 14-day standby outage instead of a health
// gauge that reset on every restart.
func TestPrintStatusCallsOutAStandbyThatStoppedAnswering(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, config.HostCmux, status.Snapshot{
		WrittenAt: time.Now(),
		SlotLastReachedAt: map[string]time.Time{
			"relay":  time.Now().Add(-time.Minute),
			"direct": time.Now().Add(-14 * 24 * time.Hour),
		},
	})
	out := buf.String()

	if !strings.Contains(out, "UNREACHABLE since then") {
		t.Fatalf("a 14-day-old slot must be called out: %s", out)
	}
	// The healthy one must not be, or the warning means nothing.
	relayLine := lineContaining(t, out, "relay")
	if strings.Contains(relayLine, "UNREACHABLE") {
		t.Fatalf("a slot reached a minute ago must not be flagged: %q", relayLine)
	}
}

// "never answered" and "not configured" are different answers and only one is
// alarming, so they must not print the same.
func TestPrintStatusDistinguishesNeverReachedFromAbsent(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, config.HostCmux, status.Snapshot{
		WrittenAt:         time.Now(),
		SlotLastReachedAt: map[string]time.Time{"direct": {}},
	})
	out := buf.String()

	if !strings.Contains(out, "NEVER") {
		t.Fatalf("want the never-reached wording: %s", out)
	}
	// Scoped to the slots block: "relay tunnel:" is a different line entirely.
	if strings.Contains(slotsBlock(out), "relay") {
		t.Fatalf("an unconfigured slot must not appear at all: %s", out)
	}
}

// slotsBlock returns the indented rows under "slots reached:".
func slotsBlock(out string) string {
	var block []string
	inBlock := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "slots reached:"):
			inBlock = true
		case inBlock && strings.HasPrefix(line, "  "):
			block = append(block, line)
		case inBlock:
			return strings.Join(block, "\n")
		}
	}
	return strings.Join(block, "\n")
}

func TestPrintStatusSaysSoBeforeTheFirstRound(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, config.HostCmux, status.Snapshot{WrittenAt: time.Now()})

	if !strings.Contains(buf.String(), "no completed device round yet") {
		t.Fatalf("output missing the pre-first-round line: %s", buf.String())
	}
}

// A slot inside the grace window reads as ordinary: one missed hourly round is
// a blip, not an outage, and crying about it would train the reader to ignore
// the line that matters.
func TestPrintStatusToleratesASingleMissedRound(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, config.HostCmux, status.Snapshot{
		WrittenAt:         time.Now(),
		SlotLastReachedAt: map[string]time.Time{"direct": time.Now().Add(-90 * time.Minute)},
	})

	if strings.Contains(buf.String(), "UNREACHABLE") {
		t.Fatalf("90 minutes is one missed round, not an outage: %s", buf.String())
	}
}

func lineContaining(t *testing.T, out, want string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("no line containing %q in:\n%s", want, out)
	return ""
}

func TestPrintStatusLabelsTheBackendByHostKind(t *testing.T) {
	for _, kind := range []string{config.HostCmux, config.HostTmux} {
		var buf bytes.Buffer
		printStatus(&buf, kind, status.Snapshot{WrittenAt: time.Now(), LastCmuxReachedAt: time.Now()})
		if !strings.Contains(buf.String(), kind+" reached:    ") {
			t.Fatalf("%s host: output missing %q line, aligned like its neighbours: %s", kind, kind+" reached", buf.String())
		}
	}
}
