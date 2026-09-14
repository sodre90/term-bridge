// Package tmuxhost implements host.Host on top of tmux's documented CLI
// and control mode (via internal/tmux). A tmux window is the app's
// workspace, a tmux pane is both its pane and its one surface; there are
// no tabs. Every format string, command spelling and id encoding the
// bridge knows about tmux lives here.
package tmuxhost

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sodre90/term-bridge/internal/host"
	"github.com/sodre90/term-bridge/internal/tmux"
)

// Host talks to one tmux server.
type Host struct {
	tmux *tmux.Client
	// sizes remembers the windows this host has sized to a phone, so the
	// size can be handed back once no phone is looking (see terminal.go).
	sizes *windowSizes
}

// New wraps a tmux client. Call Run to start the resize janitor.
func New(c *tmux.Client) *Host {
	h := &Host{tmux: c}
	h.sizes = newWindowSizes(h.releaseWindowSize, time.Now)
	return h
}

var _ host.Host = (*Host)(nil)

func (h *Host) Kind() string { return host.KindTmux }

// Capabilities: no tabs (a pane is its own surface) and, until the hook
// feed lands, no structured prompts.
func (h *Host) Capabilities() host.Capabilities {
	return host.Capabilities{Tabs: false, Feed: false}
}

// ValidID reports whether id is one of this host's window or pane ids.
func (h *Host) ValidID(s string) bool { return validID(s) }

// staleError is a well-formed id from a tmux server that is no longer the
// one running: its object is gone for good, like a not-found.
type staleError struct{ id string }

func (e *staleError) Error() string {
	return fmt.Sprintf("tmux: %s belongs to an earlier tmux server", e.id)
}
func (e *staleError) NotFound() bool { return true }

// resolve decodes a wire id of the wanted kind and confirms, in one tmux
// round trip, that its object exists on the server that minted it. The
// returned target is the tmux spelling for -t.
func (h *Host) resolve(ctx context.Context, wireID string, want idKind) (string, error) {
	id, ok := decodeID(wireID)
	if !ok || id.kind != want {
		return "", &tmux.NotFoundError{Target: wireID, Stderr: "can't find " + wireID}
	}
	out, err := h.tmux.Run(ctx, "display-message", "-p", "-t", id.target(), "-F", "#{start_time}")
	if err != nil {
		return "", notFoundIfNoServer(err)
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return "", fmt.Errorf("%w: start_time %q", host.ErrMalformed, strings.TrimSpace(string(out)))
	}
	if epoch != id.epoch {
		return "", &staleError{id: wireID}
	}
	return id.target(), nil
}

// splitFields splits one formatted line into exactly n fields, padding
// with empties so a short line never indexes out of range.
func splitFields(line string, n int) []string {
	f := strings.Split(strings.TrimRight(line, "\n"), fieldSep)
	for len(f) < n {
		f = append(f, "")
	}
	return f[:n]
}

// notFoundIfNoServer reads "no tmux server" as "that object is gone": with
// no server there are no panes, and the phone should stop asking for one.
func notFoundIfNoServer(err error) error {
	if errors.Is(err, tmux.ErrNoServer) {
		return &tmux.NotFoundError{Stderr: err.Error()}
	}
	return err
}
