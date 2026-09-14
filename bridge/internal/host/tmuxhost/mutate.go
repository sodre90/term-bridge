package tmuxhost

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/sodre90/term-bridge/internal/host"
	"github.com/sodre90/term-bridge/internal/tmux"
	"github.com/sodre90/term-bridge/internal/wire"
)

// createdFormat is what new-window / split-window print with -P: the
// server epoch and the new ids, so the wire ids can be minted without a
// second round trip.
const createdFormat = "#{start_time}" + fieldSep + "#{window_id}" + fieldSep + "#{pane_id}"

func parseCreated(out []byte) (epoch int64, windowID, paneID string, err error) {
	f := splitFields(strings.TrimSpace(string(out)), 3)
	epoch, err = strconv.ParseInt(f[0], 10, 64)
	if err != nil || f[1] == "" || f[2] == "" {
		return 0, "", "", fmt.Errorf("%w: created ids %q", host.ErrMalformed, strings.TrimSpace(string(out)))
	}
	return epoch, f[1], f[2], nil
}

// CreateWorkspace opens a new window in the most recently active session
// (or a new session when none exists), detached: nothing made from the
// phone steals an attached user's focus. Title is optional; tmux names
// the window after its command otherwise.
func (h *Host) CreateWorkspace(ctx context.Context, cwd, title string) (wire.CreateWorkspaceResponse, error) {
	session, err := h.mostRecentSession(ctx)
	if err != nil {
		return wire.CreateWorkspaceResponse{}, err
	}
	var args []string
	if session == "" {
		args = []string{"new-session", "-d", "-c", cwd, "-P", "-F", createdFormat}
	} else {
		args = []string{"new-window", "-d", "-t", session + ":", "-c", cwd, "-P", "-F", createdFormat}
	}
	if title != "" {
		args = append(args, "-n", title)
	}
	out, err := h.tmux.Run(ctx, args...)
	if err != nil {
		return wire.CreateWorkspaceResponse{}, err
	}
	epoch, windowID, paneID, err := parseCreated(out)
	if err != nil {
		return wire.CreateWorkspaceResponse{}, err
	}
	return wire.CreateWorkspaceResponse{
		WorkspaceID: encodeID(epoch, windowID),
		SurfaceID:   encodeID(epoch, paneID),
	}, nil
}

// mostRecentSession is the session with the latest activity, "" when the
// server has none (or is not running).
func (h *Host) mostRecentSession(ctx context.Context) (string, error) {
	out, err := h.tmux.Run(ctx, "list-sessions", "-F", "#{session_activity}"+fieldSep+"#{session_name}")
	if errors.Is(err, tmux.ErrNoServer) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	type entry struct {
		activity int64
		name     string
	}
	var sessions []entry
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := splitFields(line, 2)
		activity, _ := strconv.ParseInt(f[0], 10, 64)
		if f[1] != "" {
			sessions = append(sessions, entry{activity, f[1]})
		}
	}
	if len(sessions) == 0 {
		return "", nil
	}
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].activity > sessions[j].activity })
	return sessions[0].name, nil
}

// SplitPane splits the pane beside itself, detached, inheriting its cwd.
// tmux's -h splits left/right and -v up/down; -b puts the new pane before
// (left of / above) the target.
func (h *Host) SplitPane(ctx context.Context, surfaceID, direction string) (wire.CreatePaneResponse, error) {
	target, err := h.resolve(ctx, surfaceID, paneID)
	if err != nil {
		return wire.CreatePaneResponse{}, err
	}
	cwd, err := h.tmux.Run(ctx, "display-message", "-p", "-t", target, "-F", "#{pane_current_path}")
	if err != nil {
		return wire.CreatePaneResponse{}, err
	}
	args := []string{"split-window", "-d", "-t", target, "-P", "-F", createdFormat}
	switch direction {
	case wire.PlacementLeft:
		args = append(args, "-h", "-b")
	case wire.PlacementRight:
		args = append(args, "-h")
	case wire.PlacementUp:
		args = append(args, "-v", "-b")
	case wire.PlacementDown:
		args = append(args, "-v")
	default:
		return wire.CreatePaneResponse{}, fmt.Errorf("%w: split direction %q", host.ErrUnsupported, direction)
	}
	if path := strings.TrimSpace(string(cwd)); path != "" {
		args = append(args, "-c", path)
	}
	out, err := h.tmux.Run(ctx, args...)
	if err != nil {
		return wire.CreatePaneResponse{}, err
	}
	epoch, _, newPane, err := parseCreated(out)
	if err != nil {
		return wire.CreatePaneResponse{}, err
	}
	id := encodeID(epoch, newPane)
	return wire.CreatePaneResponse{SurfaceID: id, PaneID: id}, nil
}

// CreateTab has no tmux analog: a pane holds exactly one surface.
func (h *Host) CreateTab(ctx context.Context, workspaceID, paneID string) (wire.CreatePaneResponse, error) {
	return wire.CreatePaneResponse{}, host.ErrUnsupported
}

// SelectWorkspace makes the window current in its session, which is what
// an attached client shows.
func (h *Host) SelectWorkspace(ctx context.Context, workspaceID string) error {
	target, err := h.resolve(ctx, workspaceID, windowID)
	if err != nil {
		return err
	}
	_, err = h.tmux.Run(ctx, "select-window", "-t", target)
	return err
}

// FocusSurface makes the pane active in its window.
func (h *Host) FocusSurface(ctx context.Context, surfaceID string) error {
	target, err := h.resolve(ctx, surfaceID, paneID)
	if err != nil {
		return err
	}
	_, err = h.tmux.Run(ctx, "select-pane", "-t", target)
	return err
}

// RenameWorkspace sets the window name, which also stops tmux renaming it
// after the running command.
func (h *Host) RenameWorkspace(ctx context.Context, workspaceID, title string) error {
	target, err := h.resolve(ctx, workspaceID, windowID)
	if err != nil {
		return err
	}
	_, err = h.tmux.Run(ctx, "rename-window", "-t", target, title)
	return err
}

// CloseWorkspace kills the window and every process in it.
func (h *Host) CloseWorkspace(ctx context.Context, workspaceID string) error {
	target, err := h.resolve(ctx, workspaceID, windowID)
	if err != nil {
		return err
	}
	_, err = h.tmux.Run(ctx, "kill-window", "-t", target)
	return err
}

// CloseSurface kills the pane; tmux closes the window with its last pane.
func (h *Host) CloseSurface(ctx context.Context, surfaceID string) error {
	target, err := h.resolve(ctx, surfaceID, paneID)
	if err != nil {
		return err
	}
	_, err = h.tmux.Run(ctx, "kill-pane", "-t", target)
	return err
}
