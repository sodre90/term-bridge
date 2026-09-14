package tmuxhost

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/sodre90/cmux-bridge/internal/host"
	"github.com/sodre90/cmux-bridge/internal/tmux"
	"github.com/sodre90/cmux-bridge/internal/wire"
)

// paneFormat is the one format string every listing uses, so a window's
// panes and the server epoch that scopes their ids come from the same
// server turn. Fields are separated by a unit separator: window names and
// paths may hold anything printable, and tmux never emits control bytes in
// a format.
const (
	fieldSep   = "\x1f"
	paneFormat = "#{start_time}" + fieldSep +
		"#{session_name}" + fieldSep +
		"#{window_id}" + fieldSep +
		"#{window_name}" + fieldSep +
		"#{window_active}" + fieldSep +
		"#{pane_id}" + fieldSep +
		"#{pane_active}" + fieldSep +
		"#{pane_current_command}" + fieldSep +
		"#{pane_current_path}" + fieldSep +
		"#{pane_left}" + fieldSep +
		"#{pane_top}" + fieldSep +
		"#{pane_width}" + fieldSep +
		"#{pane_height}"
	paneFieldCount = 13
)

// paneRow is one line of list-panes in paneFormat.
type paneRow struct {
	epoch                int64
	session              string
	windowID, windowName string
	windowActive         bool
	paneID               string
	paneActive           bool
	command, path        string
	left, top            int
	width, height        int
}

func parsePaneRows(out []byte) ([]paneRow, error) {
	var rows []paneRow
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, fieldSep)
		if len(f) != paneFieldCount {
			return nil, fmt.Errorf("%w: list-panes line has %d fields", host.ErrMalformed, len(f))
		}
		epoch, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: start_time %q", host.ErrMalformed, f[0])
		}
		ints := make([]int, 4)
		for i, s := range f[9:13] {
			if ints[i], err = strconv.Atoi(s); err != nil {
				return nil, fmt.Errorf("%w: pane geometry %q", host.ErrMalformed, s)
			}
		}
		rows = append(rows, paneRow{
			epoch: epoch, session: f[1],
			windowID: f[2], windowName: f[3], windowActive: f[4] == "1",
			paneID: f[5], paneActive: f[6] == "1",
			command: f[7], path: f[8],
			left: ints[0], top: ints[1], width: ints[2], height: ints[3],
		})
	}
	return rows, nil
}

// listPanes runs list-panes with paneFormat, for every window (-a) or one
// window (-t target). No server means no panes, not an outage.
func (h *Host) listPanes(ctx context.Context, scope ...string) ([]paneRow, error) {
	args := append([]string{"list-panes"}, scope...)
	out, err := h.tmux.Run(ctx, append(args, "-F", paneFormat)...)
	if errors.Is(err, tmux.ErrNoServer) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parsePaneRows(out)
}

// ListWorkspaces maps every window of every session to a workspace, in
// tmux's own order (session, then window index). The window's active pane
// supplies its cwd; the window name is its title, which tmux keeps equal
// to the running command until someone renames it.
func (h *Host) ListWorkspaces(ctx context.Context) ([]wire.Workspace, error) {
	rows, err := h.listPanes(ctx, "-a")
	if err != nil {
		return nil, err
	}
	return workspacesOf(rows), nil
}

func workspacesOf(rows []paneRow) []wire.Workspace {
	out := []wire.Workspace{}
	index := map[string]int{}
	for _, r := range rows {
		wid := encodeID(r.epoch, r.windowID)
		i, seen := index[wid]
		if !seen {
			i = len(out)
			index[wid] = i
			out = append(out, wire.Workspace{ID: wid, Title: r.windowName, Terminals: []wire.TerminalPane{}})
		}
		if r.paneActive || out[i].CWD == "" {
			out[i].CWD = r.path
		}
		out[i].Terminals = append(out[i].Terminals, wire.TerminalPane{
			ID:      encodeID(r.epoch, r.paneID),
			CWD:     r.path,
			Title:   r.command,
			Focused: r.paneActive,
			Ready:   true,
			Kind:    classifyKind(r.command),
		})
	}
	return out
}

// agentCommands are the process names of the coding agents the owner runs;
// anything else in a pane is a plain terminal. A process name is a far
// steadier signal than the title heuristic cmux panes need.
var agentCommands = map[string]bool{
	"claude": true, "qwen": true, "gemini": true, "codex": true, "aider": true, "opencode": true,
}

func classifyKind(command string) string {
	if agentCommands[command] {
		return "agent"
	}
	return "terminal"
}

// ListPanes is the layout of one window: each pane's cell rectangle, which
// the server normalises onto the unit square itself. A pane is its own only
// surface.
func (h *Host) ListPanes(ctx context.Context, workspaceID string) ([]host.Pane, error) {
	target, err := h.resolve(ctx, workspaceID, windowID)
	if err != nil {
		return nil, err
	}
	rows, err := h.listPanes(ctx, "-t", target)
	if err != nil {
		return nil, err
	}
	panes := make([]host.Pane, 0, len(rows))
	for _, r := range rows {
		pid := encodeID(r.epoch, r.paneID)
		panes = append(panes, host.Pane{
			ID:                pid,
			Focused:           r.paneActive,
			SurfaceIDs:        []string{pid},
			SelectedSurfaceID: pid,
			Frame: host.Frame{
				X: float64(r.left), Y: float64(r.top),
				Width: float64(r.width), Height: float64(r.height),
			},
		})
	}
	return panes, nil
}
