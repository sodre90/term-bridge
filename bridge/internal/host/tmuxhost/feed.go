package tmuxhost

import (
	"context"
	"encoding/json"

	"github.com/sodre90/term-bridge/internal/host"
)

// PendingFeed is empty until the Claude Code hook feed exists: tmux itself
// has no notion of an agent prompt.
func (h *Host) PendingFeed(ctx context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{"items":[]}`), nil
}

// FeedReply has nothing to answer.
func (h *Host) FeedReply(ctx context.Context, kind, requestID string, params map[string]any) error {
	return host.ErrUnsupported
}
