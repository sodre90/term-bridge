package main

import (
	"log/slog"
	"time"
)

// How often a still-unreachable relay is worth mentioning again.
const relayOutageSummaryPeriod = 10 * time.Minute

// relayDialLog decides what each attempt of the reconnect loop is worth
// saying.
//
// The loop dials every 30s once backoff caps, and used to write two lines per
// attempt for as long as an outage lasted: 1058 attempts in 4.7 hours on
// 2026-08-21, 84% of the whole log file, burying the terminal, auth and
// reaper lines someone was actually reading it for (cmux-app-5v1). What
// matters about a known, ongoing outage is that it started, that it is still
// going, and that it ended -- not each individual retry.
//
// A changed error is always reported, because it is not the same outage any
// more: 502 becoming 401 is a different problem wearing the same silence.
type relayDialLog struct {
	relayURL string
	now      func() time.Time

	down           bool
	since          time.Time
	attempts       int
	lastErr        string
	lastSummarized time.Time
}

func newRelayDialLog(relayURL string) *relayDialLog {
	return &relayDialLog{relayURL: relayURL, now: time.Now}
}

// dialing announces an attempt, but only while the relay is not already known
// to be down -- during an outage the attempts are the noise.
func (l *relayDialLog) dialing() {
	if l.down {
		return
	}
	slog.Info("agent: dialing relay", "relay_url", l.relayURL)
}

// up reports that the tunnel is established, and how long it was gone if it
// had been.
func (l *relayDialLog) up() {
	if l.down {
		slog.Info("agent: relay reachable again",
			"relay_url", l.relayURL,
			"down_for", l.now().Sub(l.since).Round(time.Second),
			"attempts", l.attempts)
	}
	l.down = false
	l.attempts = 0
	l.lastErr = ""
}

// failed records an attempt that did not produce a working tunnel, and logs
// the first one, any change of cause, and a periodic reminder.
func (l *relayDialLog) failed(err error) {
	msg := err.Error()
	l.attempts++
	switch {
	case !l.down:
		l.down = true
		l.since = l.now()
		l.lastSummarized = l.now()
		l.lastErr = msg
		slog.Warn("agent: relay unreachable, retrying quietly until it recovers",
			"relay_url", l.relayURL, "err", err)
	case msg != l.lastErr:
		l.lastErr = msg
		l.lastSummarized = l.now()
		slog.Warn("agent: relay still unreachable, and for a different reason now",
			"relay_url", l.relayURL, "err", err,
			"down_for", l.now().Sub(l.since).Round(time.Second),
			"attempts", l.attempts)
	case l.now().Sub(l.lastSummarized) >= relayOutageSummaryPeriod:
		l.lastSummarized = l.now()
		slog.Warn("agent: relay still unreachable",
			"relay_url", l.relayURL, "err", err,
			"down_for", l.now().Sub(l.since).Round(time.Second),
			"attempts", l.attempts)
	}
}
