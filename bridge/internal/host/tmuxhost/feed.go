package tmuxhost

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sodre90/term-bridge/internal/host"
	"github.com/sodre90/term-bridge/internal/host/agentfeed"
	"github.com/sodre90/term-bridge/internal/wire"
)

// EnableFeed turns on the Claude Code hook feed: hooks forwarded to ln
// become pending prompts, and Capabilities advertises Feed. Call before
// RunEvents, which serves the listener.
func (h *Host) EnableFeed(ln net.Listener) {
	h.feed = agentfeed.New(paneOps{h})
	h.hooks = ln
}

// PendingFeed is the hook feed's items, or none without one: tmux itself
// has no notion of an agent prompt.
func (h *Host) PendingFeed(ctx context.Context) (json.RawMessage, error) {
	if h.feed == nil {
		return json.RawMessage(`{"items":[]}`), nil
	}
	return h.feed.PendingFeed(ctx)
}

// FeedReply types the reply into the prompt's pane (see agentfeed).
func (h *Host) FeedReply(ctx context.Context, kind, requestID string, params map[string]any) error {
	if h.feed == nil {
		return host.ErrUnsupported
	}
	return h.feed.FeedReply(ctx, kind, requestID, params)
}

// paneOps is the tmux side of agentfeed.Panes: the pane ids there are
// tmux's own (%N, from the hook's $TMUX_PANE), not wire ids.
type paneOps struct{ h *Host }

var tmuxPaneID = regexp.MustCompile(`^%[0-9]+$`)

// Locate resolves a hook's pane on this host's server. %N is reused by
// every tmux server, so the socket the hook saw must be the one this host
// talks to; a hook from another server is refused rather than typed into.
func (p paneOps) Locate(ctx context.Context, socket, pane string) (agentfeed.Location, error) {
	if !tmuxPaneID.MatchString(pane) {
		return agentfeed.Location{}, fmt.Errorf("%w: pane %q", host.ErrMalformed, pane)
	}
	out, err := p.h.tmux.Run(ctx, "display-message", "-p", "-t", pane, "-F",
		"#{socket_path}"+fieldSep+"#{start_time}"+fieldSep+"#{window_id}"+fieldSep+"#{pane_current_path}")
	if err != nil {
		return agentfeed.Location{}, err
	}
	f := splitFields(string(out), 4)
	if filepath.Clean(f[0]) != filepath.Clean(socket) {
		return agentfeed.Location{}, fmt.Errorf("pane %s belongs to the tmux server at %s, not %s", pane, socket, f[0])
	}
	epoch, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return agentfeed.Location{}, fmt.Errorf("%w: start_time %q", host.ErrMalformed, f[1])
	}
	return agentfeed.Location{
		WorkspaceID: encodeID(epoch, f[2]),
		SurfaceID:   encodeID(epoch, pane),
		CWD:         f[3],
	}, nil
}

// Capture is the pane's visible screen as plain text, wrapped lines joined
// so a long option label at a phone's width still reads as one line.
func (p paneOps) Capture(ctx context.Context, pane string) (string, error) {
	if !tmuxPaneID.MatchString(pane) {
		return "", fmt.Errorf("%w: pane %q", host.ErrMalformed, pane)
	}
	out, err := p.h.tmux.Run(ctx, "capture-pane", "-p", "-J", "-t", pane)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (p paneOps) SendKeys(ctx context.Context, pane, key string) error {
	if !tmuxPaneID.MatchString(pane) {
		return fmt.Errorf("%w: pane %q", host.ErrMalformed, pane)
	}
	_, err := p.h.tmux.Run(ctx, "send-keys", "-t", pane, key)
	return err
}

// applyFeedStatus marks the workspaces whose panes the feed knows to be
// holding a prompt or waiting for input, the way cmux's synthesized
// preview does for the Mac. A pane no longer running an agent gets no
// mark whatever the feed remembers: the process is the ground truth.
func (h *Host) applyFeedStatus(workspaces []wire.Workspace) {
	if h.feed == nil {
		return
	}
	statuses := h.feed.Statuses()
	if len(statuses) == 0 {
		return
	}
	for i := range workspaces {
		for _, term := range workspaces[i].Terminals {
			if term.Kind != "agent" {
				continue
			}
			if st, ok := statuses[term.ID]; ok && rank(st.Attention) > rank(workspaces[i].Attention) {
				workspaces[i].Attention = st.Attention
				workspaces[i].Preview = st.Preview
			}
		}
	}
}

// rank orders attention states so a window with both a prompt and an idle
// agent shows the prompt.
func rank(attention string) int {
	switch strings.ToLower(attention) {
	case "permission":
		return 2
	case "input":
		return 1
	}
	return 0
}
