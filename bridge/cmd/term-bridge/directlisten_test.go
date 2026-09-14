package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func testDirectLog(t *testing.T) (*directListenLog, *fakeClock, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	clock := &fakeClock{t: time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC)}
	l := newDirectListenLog(":8443")
	l.now = clock.now
	return l, clock, &buf
}

// cmux-app-t5x. The exact failure: the agent started before Tailscale had
// assigned its address, and one bind error ended the standby for 14 days.
func TestABindFailureIsReportedWithItsCause(t *testing.T) {
	l, _, buf := testDirectLog(t)

	l.failed(errors.New("direct mode: listen 100.105.91.51:8443: bind: can't assign requested address"))

	out := buf.String()
	if !strings.Contains(out, "can't assign requested address") {
		t.Fatalf("the cause must survive into the log: %s", out)
	}
	if !strings.Contains(out, "retrying") {
		t.Fatalf("the first line must say it will keep trying: %s", out)
	}
}

// A retry loop that logs every attempt writes 2880 lines a day at the 30s cap
// and buries the log, which is cmux-app-5v1 all over again.
func TestASustainedOutageDoesNotLogEveryAttempt(t *testing.T) {
	l, clock, buf := testDirectLog(t)
	err := errors.New("direct mode: tailscale status: not running")

	for range 240 {
		clock.advance(clock.t.Add(30 * time.Second))
		l.failed(err)
	}

	// Two hours of 30s retries: the first line plus one summary per 10 min.
	if n := countLines(buf); n > 15 {
		t.Fatalf("wrote %d lines for one continuous outage, want a handful:\n%s", n, buf.String())
	}
	if n := countLines(buf); n < 2 {
		t.Fatalf("a two-hour outage must still be mentioned more than once, got %d", n)
	}
}

// A changed cause is a different problem wearing the same silence -- the bind
// error becoming a cert error is worth a line even mid-outage.
func TestAChangedCauseIsReportedMidOutage(t *testing.T) {
	l, clock, buf := testDirectLog(t)
	l.failed(errors.New("bind: can't assign requested address"))
	before := countLines(buf)

	clock.advance(clock.t.Add(30 * time.Second))
	l.failed(errors.New("direct mode: tailscale cert: acme failed"))

	if countLines(buf) <= before {
		t.Fatalf("a new cause must be reported: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "different reason") {
		t.Fatalf("want the changed-cause wording: %s", buf.String())
	}
}

func TestDirectRecoveryIsReportedWithHowLongItWasDown(t *testing.T) {
	l, clock, buf := testDirectLog(t)
	l.failed(errors.New("bind: can't assign requested address"))
	clock.advance(clock.t.Add(14 * 24 * time.Hour))

	l.wasUp()

	out := buf.String()
	if !strings.Contains(out, "working again") {
		t.Fatalf("want a recovery line: %s", out)
	}
	if !strings.Contains(out, "down_for=336h0m0s") {
		t.Fatalf("want the outage length: %s", out)
	}
}

// A listener that comes back and later fails again is a NEW outage, and must
// be as loud as the first -- otherwise the second one is invisible.
func TestAFreshOutageAfterRecoveryIsLoudAgain(t *testing.T) {
	l, clock, buf := testDirectLog(t)
	l.failed(errors.New("bind: can't assign requested address"))
	l.wasUp()
	buf.Reset()

	clock.advance(clock.t.Add(time.Hour))
	l.failed(errors.New("bind: can't assign requested address"))

	if !strings.Contains(buf.String(), "retrying") {
		t.Fatalf("the first failure of a new outage must be loud: %s", buf.String())
	}
}

// serveDirect returning nil is not success -- the listener still stopped.
func TestAListenerThatStopsWithoutAnErrorIsStillAFailure(t *testing.T) {
	l, _, buf := testDirectLog(t)

	l.failed(nil)

	if !strings.Contains(buf.String(), "listener stopped without an error") {
		t.Fatalf("a nil error must still be described: %s", buf.String())
	}
}

// The counter the retry loop uses to tell "never started" from "was serving
// and then stopped". markUnbound erases the boolean, which is why a separate
// count is needed at all.
func TestBindCountSurvivesMarkUnbound(t *testing.T) {
	h := &directHealth{}
	if h.binds() != 0 {
		t.Fatalf("fresh health reports %d binds, want 0", h.binds())
	}

	h.markBound()
	h.markUnbound()

	if h.binds() != 1 {
		t.Fatalf("binds() = %d after a bind and unbind, want 1", h.binds())
	}
	if bound, _, _ := h.snapshot(); bound {
		t.Fatal("markUnbound must still clear the current state")
	}
}

func TestANilHealthReportsNoBinds(t *testing.T) {
	var h *directHealth
	h.markBound()
	if h.binds() != 0 {
		t.Fatalf("a nil health must record nothing, got %d", h.binds())
	}
}

// -- the retry loop itself

// THE regression. Before this, one bind failure ended the direct listener for
// the life of the process: the goroutine logged and exited, the relay kept
// working, and the standby was silently gone for 14 days.
func TestABindFailureIsRetriedInsteadOfEndingTheListener(t *testing.T) {
	quietLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	attempts := 0
	retryDirectListener(ctx, ":8443", &directHealth{}, time.Millisecond, time.Millisecond, func() error {
		attempts++
		if attempts >= 5 {
			cancel()
		}
		return errors.New("direct mode: listen 100.105.91.51:8443: bind: can't assign requested address")
	})

	if attempts < 5 {
		t.Fatalf("the listener was attempted %d times, want it retried until cancelled", attempts)
	}
}

// The loop must stop when the agent is shutting down, not spin.
func TestTheLoopStopsWhenTheContextIsCancelled(t *testing.T) {
	quietLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		retryDirectListener(ctx, ":8443", &directHealth{}, time.Millisecond, time.Millisecond, func() error {
			attempts++
			return errors.New("nope")
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retryDirectListener did not return on a cancelled context")
	}
	if attempts != 0 {
		t.Fatalf("an already-cancelled context must not start an attempt, got %d", attempts)
	}
}

// A listener that bound and later stopped is a different story from one that
// never started, and the loop has to tell them apart to reset its backoff and
// report recovery.
func TestAListenerThatServedAndStoppedIsTreatedAsRecovered(t *testing.T) {
	buf := quietLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	health := &directHealth{}
	attempts := 0
	retryDirectListener(ctx, ":8443", health, time.Millisecond, time.Millisecond, func() error {
		attempts++
		switch attempts {
		case 1:
			return errors.New("bind: can't assign requested address")
		case 2:
			health.markBound() // this attempt got as far as serving
			return errors.New("http: Server closed")
		default:
			cancel()
			return errors.New("bind: can't assign requested address")
		}
	})

	out := buf.String()
	if !strings.Contains(out, "working again") {
		t.Fatalf("the attempt that bound must be reported as a recovery: %s", out)
	}
}

func quietLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}
