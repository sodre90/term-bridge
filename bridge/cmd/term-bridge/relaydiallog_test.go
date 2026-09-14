package main

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) advance(d time.Time) { c.t = d }

func testDialLog(t *testing.T) (*relayDialLog, *fakeClock, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	clock := &fakeClock{t: time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC)}
	l := newRelayDialLog("wss://relay.example/agent/tunnel")
	l.now = clock.now
	return l, clock, &buf
}

func countLines(buf *bytes.Buffer) int {
	trimmed := strings.TrimSpace(buf.String())
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, "\n") + 1
}

// The measured shape of the problem: 1058 attempts in 4.7 hours wrote 84% of
// the log file. The same outage must now cost a handful of lines.
func TestASustainedOutageStopsWritingTwoLinesAnAttempt(t *testing.T) {
	l, clock, buf := testDialLog(t)
	outage := errors.New("websocket: bad handshake (relay answered HTTP 502 Bad Gateway)")

	start := clock.t
	for i := range 560 { // 560 attempts at 30s == 4.7 hours
		clock.advance(start.Add(time.Duration(i) * 30 * time.Second))
		l.dialing()
		l.failed(outage)
	}

	// One opening WARN plus one summary per 10 minutes of a 4.7-hour outage.
	if lines := countLines(buf); lines > 32 {
		t.Fatalf("a 4.7-hour outage wrote %d lines:\n%s", lines, buf)
	}
	if !strings.Contains(buf.String(), "relay unreachable, retrying quietly") {
		t.Errorf("the outage must still be reported when it starts:\n%s", buf)
	}
	if !strings.Contains(buf.String(), "attempts=") {
		t.Errorf("the summary should say how many attempts were suppressed:\n%s", buf)
	}
}

func TestTheFirstFailureIsAlwaysReportedWithItsCause(t *testing.T) {
	l, _, buf := testDialLog(t)

	l.dialing()
	l.failed(errors.New("connection refused"))

	if !strings.Contains(buf.String(), "connection refused") {
		t.Fatalf("the first failure must carry its cause:\n%s", buf)
	}
}

// 502 becoming 401 is a different problem wearing the same silence.
func TestAChangedCauseIsReportedEvenMidOutage(t *testing.T) {
	l, _, buf := testDialLog(t)
	l.failed(errors.New("relay answered HTTP 502 Bad Gateway"))
	buf.Reset()

	l.failed(errors.New("relay answered HTTP 502 Bad Gateway"))
	if countLines(buf) != 0 {
		t.Fatalf("a repeat of the same cause should stay quiet:\n%s", buf)
	}

	l.failed(errors.New("relay answered HTTP 401 Unauthorized"))
	if !strings.Contains(buf.String(), "401") {
		t.Fatalf("a changed cause must break the silence:\n%s", buf)
	}
}

func TestRecoveryIsReportedWithHowLongItWasDown(t *testing.T) {
	l, clock, buf := testDialLog(t)
	start := clock.t
	l.failed(errors.New("boom"))
	clock.advance(start.Add(90 * time.Second))
	l.failed(errors.New("boom"))
	buf.Reset()

	clock.advance(start.Add(2 * time.Minute))
	l.up()

	logged := buf.String()
	if !strings.Contains(logged, "relay reachable again") {
		t.Fatalf("recovery must be reported:\n%s", logged)
	}
	if !strings.Contains(logged, "down_for=2m0s") {
		t.Errorf("recovery should say how long it was gone:\n%s", logged)
	}
	if !strings.Contains(logged, "attempts=2") {
		t.Errorf("recovery should say how many attempts it took:\n%s", logged)
	}
}

// A tunnel that comes up, runs, and drops is an outage starting, not a
// continuation of the previous one -- so the next failure is loud again.
func TestAFreshOutageAfterARecoveryIsLoudAgain(t *testing.T) {
	l, _, buf := testDialLog(t)
	l.failed(errors.New("boom"))
	l.up()
	buf.Reset()

	l.dialing()
	l.failed(errors.New("boom"))

	if !strings.Contains(buf.String(), "relay unreachable, retrying quietly") {
		t.Fatalf("a new outage must be reported, not folded into the old one:\n%s", buf)
	}
	if !strings.Contains(buf.String(), "dialing relay") {
		t.Errorf("the dial after a recovery is not noise, it should be logged:\n%s", buf)
	}
}

// Nothing suppresses the healthy path: an ordinary connect still says so.
func TestTheHealthyPathStillLogsTheDial(t *testing.T) {
	l, _, buf := testDialLog(t)

	l.dialing()
	l.up()

	if !strings.Contains(buf.String(), "dialing relay") {
		t.Fatalf("a dial outside an outage must be logged:\n%s", buf)
	}
	if strings.Contains(buf.String(), "reachable again") {
		t.Error("a tunnel that was never down did not 'recover'")
	}
}
