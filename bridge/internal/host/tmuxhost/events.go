package tmuxhost

import (
	"context"
	"log/slog"
	"time"

	"github.com/sodre90/term-bridge/internal/backoff"
	"github.com/sodre90/term-bridge/internal/tmux"
	"github.com/sodre90/term-bridge/internal/wire"
)

// EventTypeLayout is the frame RunEvents sends when windows, panes or
// sessions change shape. The app refetches its list on any frame that is
// not a heartbeat, so a new type costs it nothing to learn.
const EventTypeLayout = "layout"

// layoutNotifications are the control-mode notifications that mean the
// workspace list or a layout changed; %unlinked-* cover windows of the
// sessions the control client is not attached to.
var layoutNotifications = map[string]bool{
	"%sessions-changed":        true,
	"%window-add":              true,
	"%window-close":            true,
	"%window-renamed":          true,
	"%unlinked-window-add":     true,
	"%unlinked-window-close":   true,
	"%unlinked-window-renamed": true,
	"%layout-change":           true,
}

// RunEvents keeps one control-mode client attached to some session and
// turns its structural notifications into layout frames until ctx ends.
// A control client needs a session to attach to, so with none it waits
// and retries; when its session dies it reattaches to another. Bursts
// (a split renames, re-lays-out and adds in one go) are coalesced.
func (h *Host) RunEvents(ctx context.Context, sink func(wire.EventFrame)) {
	b := backoff.New(time.Second, 30*time.Second)
	for ctx.Err() == nil {
		session, err := h.mostRecentSession(ctx)
		if err != nil || session == "" {
			if err != nil {
				slog.Warn("tmuxhost: events: list sessions", "err", err)
			}
			backoff.Sleep(ctx, b.Next())
			continue
		}
		slog.Info("tmuxhost: events: attaching control client", "session", session)
		coalesce := newCoalescer(ctx, 100*time.Millisecond, func() {
			sink(wire.EventFrame{Type: EventTypeLayout})
		})
		err = h.tmux.Watch(ctx, session, func(n tmux.Notification) {
			if layoutNotifications[n.Name] {
				coalesce.trigger()
			}
		})
		coalesce.stop()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("tmuxhost: events: control client ended", "err", err)
		} else {
			slog.Info("tmuxhost: events: control client ended, session gone")
			// The session went away: that is itself a change worth a frame.
			sink(wire.EventFrame{Type: EventTypeLayout})
		}
		backoff.Sleep(ctx, b.Next())
	}
}

// coalescer fires once per quiet window after any number of triggers.
type coalescer struct {
	trigger func()
	stop    func()
}

func newCoalescer(ctx context.Context, quiet time.Duration, fire func()) *coalescer {
	ctx, cancel := context.WithCancel(ctx)
	kick := make(chan struct{}, 1)
	go func() {
		var timer *time.Timer
		var due <-chan time.Time
		for {
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case <-kick:
				if timer == nil {
					timer = time.NewTimer(quiet)
					due = timer.C
				}
			case <-due:
				timer, due = nil, nil
				fire()
			}
		}
	}()
	return &coalescer{
		trigger: func() {
			select {
			case kick <- struct{}{}:
			default:
			}
		},
		stop: cancel,
	}
}
