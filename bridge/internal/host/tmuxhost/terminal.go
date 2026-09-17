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
// exactly when the pane has asked for them (mode 2004). The app never
// brackets on its own: only the host owning the PTY knows the mode for
// sure, and cmux cannot even deliver an app-side ESC[200~ intact.
func (h *Host) Paste(ctx context.Context, surfaceID, text string) error {
	target, err := h.resolve(ctx, surfaceID, paneID)
	if err != nil {
		return err
	}
	buffer := "term-bridge-" + target[1:]
	if _, err := h.tmux.RunWithStdin(ctx, []byte(text), "load-buffer", "-b", buffer, "-"); err != nil {
		return err
	}
	_, err = h.tmux.Run(ctx, "paste-buffer", "-p", "-d", "-b", buffer, "-t", target)
	return err
}

// Resize sizes the pane's window to the phone's viewport and, when the
// window is split, zooms the pane so it gets the whole viewport rather than
// a share of it. tmux then holds that size (window-size manual) until
// releaseWindowSize hands the window back, which the janitor does once no
// replay has asked after it for a while -- the server has no "viewer left"
// call on the seam, but a viewer replays several times a second while it
// looks.
func (h *Host) Resize(ctx context.Context, surfaceID string, columns, rows int) error {
	if columns <= 0 || rows <= 0 {
		return nil
	}
	target, err := h.resolve(ctx, surfaceID, paneID)
	if err != nil {
		return err
	}
	out, err := h.tmux.Run(ctx, "display-message", "-p", "-t", target, "-F", windowStateFormat)
	if err != nil {
		return err
	}
	w := parseWindowState(string(out))
	h.sizes.viewed(w.id)
	if w.width != columns || w.height != rows {
		if _, err := h.tmux.Run(ctx, "resize-window", "-t", w.id, "-x", strconv.Itoa(columns), "-y", strconv.Itoa(rows)); err != nil {
			return err
		}
	}
	if w.panes > 1 && !w.zoomedOnTarget() {
		return h.zoomPane(ctx, w, target)
	}
	return nil
}

const windowStateFormat = "#{window_id}" + fieldSep + "#{window_width}" + fieldSep + "#{window_height}" +
	fieldSep + "#{window_panes}" + fieldSep + "#{window_zoomed_flag}" + fieldSep + "#{pane_active}"

// windowState is a window as seen from one of its panes, the target.
type windowState struct {
	id            string
	width, height int
	panes         int
	zoomed        bool
	targetActive  bool
}

// zoomedOnTarget: a zoomed window's zoomed pane is its active pane.
func (w windowState) zoomedOnTarget() bool { return w.zoomed && w.targetActive }

func parseWindowState(line string) windowState {
	f := splitFields(line, 6)
	w := windowState{id: f[0], zoomed: f[4] == "1", targetActive: f[5] == "1"}
	w.width, _ = strconv.Atoi(f[1])
	w.height, _ = strconv.Atoi(f[2])
	w.panes, _ = strconv.Atoi(f[3])
	return w
}

// zoomPane gives target the whole window. resize-pane -Z toggles the
// window's zoom whichever pane it names, so a zoom held by a sibling is
// dropped before target's own is applied (verified on tmux 3.7c).
func (h *Host) zoomPane(ctx context.Context, w windowState, target string) error {
	if w.zoomed {
		if _, err := h.tmux.Run(ctx, "resize-pane", "-Z", "-t", w.id); err != nil {
			return err
		}
	}
	if _, err := h.tmux.Run(ctx, "resize-pane", "-Z", "-t", target); err != nil {
		return err
	}
	h.sizes.zoomed(w.id)
	return nil
}

// releaseWindowSize lets a window follow its clients again and, if a phone
// zoomed a pane in it, restores the layout -- unless someone at the keyboard
// already unzoomed it, which the format condition guards against.
func (h *Host) releaseWindowSize(windowID string, zoomed bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.tmux.Run(ctx, "set-option", "-wu", "-t", windowID, "window-size"); err != nil {
		slog.Debug("tmuxhost: release window size", "window", windowID, "err", err)
	}
	if !zoomed {
		return
	}
	if _, err := h.tmux.Run(ctx, "if-shell", "-F", "-t", windowID, "#{window_zoomed_flag}", "resize-pane -Z -t "+windowID); err != nil {
		slog.Debug("tmuxhost: release zoom", "window", windowID, "err", err)
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

// windowSizes tracks which windows this host has sized to a phone, whether
// it zoomed a pane in each, and when a phone last looked at each.
type windowSizes struct {
	mu      sync.Mutex
	sized   map[string]sizedWindow
	release func(windowID string, zoomed bool)
	now     func() time.Time
}

type sizedWindow struct {
	lastSeen time.Time
	zoomed   bool
}

func newWindowSizes(release func(string, bool), now func() time.Time) *windowSizes {
	return &windowSizes{sized: map[string]sizedWindow{}, release: release, now: now}
}

// viewed records that a phone sized windowID just now.
func (w *windowSizes) viewed(windowID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.sized[windowID]
	s.lastSeen = w.now()
	w.sized[windowID] = s
}

// zoomed records that a phone zoomed a pane of the sized windowID.
func (w *windowSizes) zoomed(windowID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.sized[windowID]
	s.zoomed = true
	w.sized[windowID] = s
}

// touched records a replay of a pane in windowID, keeping a sized window
// sized while anyone still looks. A window never sized is not tracked.
func (w *windowSizes) touched(windowID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s, sized := w.sized[windowID]; sized {
		s.lastSeen = w.now()
		w.sized[windowID] = s
	}
}

// sweep releases every sized window nobody has looked at for
// sizeReleaseAfter and returns how many it released.
func (w *windowSizes) sweep() int {
	w.mu.Lock()
	stale := map[string]bool{}
	cutoff := w.now().Add(-sizeReleaseAfter)
	for id, s := range w.sized {
		if s.lastSeen.Before(cutoff) {
			stale[id] = s.zoomed
			delete(w.sized, id)
		}
	}
	w.mu.Unlock()
	for id, zoomed := range stale {
		w.release(id, zoomed)
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
