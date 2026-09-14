package cmuxhost

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sodre90/cmux-bridge/internal/host"
	"github.com/sodre90/cmux-bridge/internal/wire"
)

// paneList is the part of cmux's pane.list reply the bridge reads.
type paneList struct {
	Panes []paneEntry `json:"panes"`
}

type paneEntry struct {
	ID                string   `json:"id"`
	Focused           bool     `json:"focused"`
	SurfaceIDs        []string `json:"surface_ids"`
	SelectedSurfaceID string   `json:"selected_surface_id"`
	PixelFrame        struct {
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	} `json:"pixel_frame"`
}

// ListPanes is cmux's pane.list. Frames come back in window pixels: the
// first pane starts at the sidebar's width, not at zero, and a workspace
// never shown on the Mac reports every frame as zero.
func (h *Host) ListPanes(ctx context.Context, workspaceID string) ([]host.Pane, error) {
	raw, err := h.client.Rpc(ctx, "pane.list", map[string]any{"workspace_id": workspaceID})
	if err != nil {
		return nil, err
	}
	return ParsePanes(raw)
}

// ParsePanes decodes a pane.list reply. Exported for tests that feed the
// server live-captured cmux shapes.
func ParsePanes(raw []byte) ([]host.Pane, error) {
	var list paneList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("pane.list: %w: %w", host.ErrMalformed, err)
	}
	panes := make([]host.Pane, 0, len(list.Panes))
	for _, p := range list.Panes {
		panes = append(panes, host.Pane{
			ID:                p.ID,
			Focused:           p.Focused,
			SurfaceIDs:        p.SurfaceIDs,
			SelectedSurfaceID: p.SelectedSurfaceID,
			Frame: host.Frame{
				X: p.PixelFrame.X, Y: p.PixelFrame.Y,
				Width: p.PixelFrame.Width, Height: p.PixelFrame.Height,
			},
		})
	}
	return panes, nil
}

// CreateWorkspace is workspace.create, always unfocused so nothing made from
// the phone steals the Mac's selection.
func (h *Host) CreateWorkspace(ctx context.Context, cwd, title string) (wire.CreateWorkspaceResponse, error) {
	params := map[string]any{"cwd": cwd, "focus": false}
	if title != "" {
		params["title"] = title
	}
	raw, err := h.client.Rpc(ctx, "workspace.create", params)
	if err != nil {
		return wire.CreateWorkspaceResponse{}, err
	}
	var created wire.CreateWorkspaceResponse
	if err := json.Unmarshal(raw, &created); err != nil {
		return wire.CreateWorkspaceResponse{}, fmt.Errorf("workspace.create reply: %w: %w", host.ErrMalformed, err)
	}
	if created.WorkspaceID == "" {
		return wire.CreateWorkspaceResponse{}, fmt.Errorf("workspace.create reply: %w: no workspace_id", host.ErrMalformed)
	}
	return created, nil
}

// SplitPane is surface.split beside the surface the phone is viewing.
func (h *Host) SplitPane(ctx context.Context, surfaceID, direction string) (wire.CreatePaneResponse, error) {
	raw, err := h.client.Rpc(ctx, "surface.split", map[string]any{
		"surface_id": surfaceID,
		"direction":  direction,
		"focus":      false,
	})
	if err != nil {
		return wire.CreatePaneResponse{}, err
	}
	return decodeCreatedPane(raw)
}

// CreateTab is surface.create: a new terminal tab in the named pane.
func (h *Host) CreateTab(ctx context.Context, workspaceID, paneID string) (wire.CreatePaneResponse, error) {
	raw, err := h.client.Rpc(ctx, "surface.create", map[string]any{
		"workspace_id": workspaceID,
		"pane_id":      paneID,
		"type":         "terminal",
		"focus":        false,
	})
	if err != nil {
		return wire.CreatePaneResponse{}, err
	}
	return decodeCreatedPane(raw)
}

func decodeCreatedPane(raw json.RawMessage) (wire.CreatePaneResponse, error) {
	var created wire.CreatePaneResponse
	if err := json.Unmarshal(raw, &created); err != nil {
		return wire.CreatePaneResponse{}, fmt.Errorf("create pane reply: %w: %w", host.ErrMalformed, err)
	}
	if created.SurfaceID == "" {
		return wire.CreatePaneResponse{}, fmt.Errorf("create pane reply: %w: no surface_id", host.ErrMalformed)
	}
	return created, nil
}

// SelectWorkspace is workspace.select: make the Mac show this workspace.
func (h *Host) SelectWorkspace(ctx context.Context, workspaceID string) error {
	_, err := h.client.Rpc(ctx, "workspace.select", map[string]any{"workspace_id": workspaceID})
	return err
}

// FocusSurface is surface.focus.
func (h *Host) FocusSurface(ctx context.Context, surfaceID string) error {
	_, err := h.client.Rpc(ctx, "surface.focus", map[string]any{"surface_id": surfaceID})
	return err
}

// RenameWorkspace sets a workspace's persistent display title via cmux's
// documented workspace.rename RPC (the same one behind cmux's own
// `cmux rename-workspace` CLI command and Cmd+Shift+R shortcut).
func (h *Host) RenameWorkspace(ctx context.Context, workspaceID, title string) error {
	_, err := h.client.Rpc(ctx, "workspace.rename", map[string]any{
		"workspace_id": workspaceID,
		"title":        title,
	})
	return err
}

// CloseWorkspace is workspace.close; it takes every pane with it.
func (h *Host) CloseWorkspace(ctx context.Context, workspaceID string) error {
	_, err := h.client.Rpc(ctx, "workspace.close", map[string]any{"workspace_id": workspaceID})
	return err
}

// CloseSurface is surface.close.
func (h *Host) CloseSurface(ctx context.Context, surfaceID string) error {
	_, err := h.client.Rpc(ctx, "surface.close", map[string]any{"surface_id": surfaceID})
	return err
}
