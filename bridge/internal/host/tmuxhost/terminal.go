package tmuxhost

import (
	"context"
	"encoding/hex"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// Input types text into a pane byte for byte. send-keys -H takes each byte
// as hex, which sidesteps every way -l can misread a leading '-' or an
// escape sequence the phone's key bar sends.
func (h *Host) Input(ctx context.Context, surfaceID, text string) error {
	target, err := h.resolve(ctx, surfaceID, paneID)
	if err != nil {
		return err
	}
	if text == "" {
		return nil
	}
	args := make([]string, 0, 4+len(text))
	args = append(args, "send-keys", "-t", target, "-H")
	for i := 0; i < len(text); i++ {
		args = append(args, hex.EncodeToString([]byte{text[i]}))
	}
	_, err = h.tmux.Run(ctx, args...)
	return err
}

// Paste delivers text through a tmux buffer, so its size is not an
// argument list and paste-buffer -p wraps it in bracketed-paste markers
// exactly when the pane has asked for them (mode 2004), which is the rule
// the app applies on the cmux path too.
func (h *Host) Paste(ctx context.Context, surfaceID, text string) error {
	target, err := h.resolve(ctx, surfaceID, paneID)
	if err != nil {
		return err
	}
	buffer := "cmux-bridge-" + target[1:]
	if _, err := h.tmux.RunWithStdin(ctx, []byte(text), "load-buffer", "-b", buffer, "-"); err != nil {
		return err
	}
	_, err = h.tmux.Run(ctx, "paste-buffer", "-p", "-d", "-b", buffer, "-t", target)
	return err
}

// Resize sizes the pane's window to the phone's viewport. tmux then holds
// that size (window-size manual) until releaseWindowSize hands the window
// back, which the janitor does once no replay has asked after it for a
// while -- the server has no "viewer left" call on the seam, but a viewer
// replays several times a second while it looks.
func (h *Host) Resize(ctx context.Context, surfaceID string, columns, rows int) error {
	if columns <= 0 || rows <= 0 {
		return nil
	}
	target, err := h.resolve(ctx, surfaceID, paneID)
	if err != nil {
		return err
	}
	out, err := h.tmux.Run(ctx, "display-message", "-p", "-t", target, "-F", "#{window_id}"+fieldSep+"#{window_width}"+fieldSep+"#{window_height}")
	if err != nil {
		return err
	}
	windowID, width, height := splitWindowSize(string(out))
	h.sizes.viewed(windowID)
	if width == columns && height == rows {
		return nil
	}
	_, err = h.tmux.Run(ctx, "resize-window", "-t", windowID, "-x", strconv.Itoa(columns), "-y", strconv.Itoa(rows))
	return err
}

func splitWindowSize(line string) (windowID string, width, height int) {
	f := splitFields(line, 3)
	width, _ = strconv.Atoi(f[1])
	height, _ = strconv.Atoi(f[2])
	return f[0], width, height
}

// releaseWindowSize lets a window follow its clients again.
func (h *Host) releaseWindowSize(windowID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.tmux.Run(ctx, "set-option", "-wu", "-t", windowID, "window-size"); err != nil {
		slog.Debug("tmuxhost: release window size", "window", windowID, "err", err)
	}
}

// Run drives the resize janitor until ctx ends. Call it once per host.
func (h *Host) Run(ctx context.Context) { h.sizes.run(ctx, sizeSweepEvery) }

// A window a phone has sized is handed back this long after its last
// replay: long enough to ride out a poll hiccup, short enough that an SSH
// user gets their size back promptly after the phone leaves.
const (
	sizeReleaseAfter = 5 * time.Second
	sizeSweepEvery   = time.Second
)

// windowSizes tracks which windows this host has sized to a phone and when
// a phone last looked at each.
type windowSizes struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
	release  func(windowID string)
	now      func() time.Time
}

func newWindowSizes(release func(string), now func() time.Time) *windowSizes {
	return &windowSizes{lastSeen: map[string]time.Time{}, release: release, now: now}
}

// viewed records that a phone sized windowID just now.
func (w *windowSizes) viewed(windowID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastSeen[windowID] = w.now()
}

// touched records a replay of a pane in windowID, keeping a sized window
// sized while anyone still looks. A window never sized is not tracked.
func (w *windowSizes) touched(windowID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, sized := w.lastSeen[windowID]; sized {
		w.lastSeen[windowID] = w.now()
	}
}

// sweep releases every sized window nobody has looked at for
// sizeReleaseAfter and returns how many it released.
func (w *windowSizes) sweep() int {
	w.mu.Lock()
	var stale []string
	cutoff := w.now().Add(-sizeReleaseAfter)
	for id, seen := range w.lastSeen {
		if seen.Before(cutoff) {
			stale = append(stale, id)
			delete(w.lastSeen, id)
		}
	}
	w.mu.Unlock()
	for _, id := range stale {
		w.release(id)
	}
	return len(stale)
}

func (w *windowSizes) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.sweep()
		}
	}
}
