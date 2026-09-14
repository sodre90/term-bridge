package cmuxhost

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sodre90/term-bridge/internal/host"
)

// Replay is mobile.terminal.replay: the whole render grid of one surface.
// The caller owns the deadline -- this is categorically the heaviest call
// the bridge makes (see server's replayTimeout).
func (h *Host) Replay(ctx context.Context, surfaceID string) (host.Replay, error) {
	raw, err := h.client.Rpc(ctx, "mobile.terminal.replay",
		map[string]any{"surface_id": surfaceID})
	if err != nil {
		return host.Replay{}, err
	}
	var top struct {
		Columns    int             `json:"columns"`
		Rows       int             `json:"rows"`
		Seq        int             `json:"seq"`
		RenderGrid json.RawMessage `json:"render_grid"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return host.Replay{}, fmt.Errorf("mobile.terminal.replay: %w: %w", host.ErrMalformed, err)
	}
	return host.Replay{
		Grid:    top.RenderGrid,
		Columns: top.Columns,
		Rows:    top.Rows,
		Seq:     int64(top.Seq),
	}, nil
}

// Input is mobile.terminal.input: typed text.
func (h *Host) Input(ctx context.Context, surfaceID, text string) error {
	_, err := h.client.Rpc(ctx, "mobile.terminal.input",
		map[string]any{"surface_id": surfaceID, "text": text})
	return err
}

// Paste is mobile.terminal.paste: text delivered as a paste.
func (h *Host) Paste(ctx context.Context, surfaceID, text string) error {
	_, err := h.client.Rpc(ctx, "mobile.terminal.paste",
		map[string]any{"surface_id": surfaceID, "text": text})
	return err
}

// Resize is mobile.terminal.viewport: the phone's view of the surface.
func (h *Host) Resize(ctx context.Context, surfaceID string, columns, rows int) error {
	_, err := h.client.Rpc(ctx, "mobile.terminal.viewport",
		map[string]any{"surface_id": surfaceID, "columns": columns, "rows": rows})
	return err
}
