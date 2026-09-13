package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/sodre90/cmux-bridge/internal/httpjson"
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

// handleLayout answers GET /sessions/{id}/layout from cmux's pane.list.
func (s *Server) handleLayout(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	panes, err := s.listPanes(r.Context(), id)
	if err != nil {
		slog.Warn("server: pane.list failed", "workspace", id, "err", err)
		httpjson.Error(w, http.StatusBadGateway, "cmux pane.list failed")
		return
	}
	httpjson.Write(w, http.StatusOK, normaliseLayout(panes))
}

func (s *Server) listPanes(ctx context.Context, workspaceID string) ([]paneEntry, error) {
	raw, err := s.cmux.Rpc(ctx, "pane.list", map[string]any{"workspace_id": workspaceID})
	if err != nil {
		return nil, err
	}
	var list paneList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("pane.list: %w", err)
	}
	return list.Panes, nil
}

// normaliseLayout maps cmux's pixel frames onto the unit square. The frames
// are taken relative to the panes' own bounding box: cmux reports them in
// window coordinates, so the first pane starts at the sidebar's width, not
// at zero. A workspace never shown on the Mac reports every frame as zero;
// those are laid out as equal columns and flagged as estimated.
func normaliseLayout(panes []paneEntry) wire.Layout {
	out := wire.Layout{Panes: make([]wire.LayoutPane, 0, len(panes))}
	if len(panes) == 0 {
		return out
	}
	minX, minY := panes[0].PixelFrame.X, panes[0].PixelFrame.Y
	maxX, maxY := minX, minY
	for _, p := range panes {
		f := p.PixelFrame
		minX, minY = min(minX, f.X), min(minY, f.Y)
		maxX, maxY = max(maxX, f.X+f.Width), max(maxY, f.Y+f.Height)
	}
	width, height := maxX-minX, maxY-minY
	out.Estimated = width <= 0 || height <= 0
	for i, p := range panes {
		lp := wire.LayoutPane{
			ID:                p.ID,
			Focused:           p.Focused,
			SurfaceIDs:        nonNil(p.SurfaceIDs),
			SelectedSurfaceID: p.SelectedSurfaceID,
		}
		if out.Estimated {
			lp.X, lp.Y = float64(i)/float64(len(panes)), 0
			lp.W, lp.H = 1/float64(len(panes)), 1
		} else {
			f := p.PixelFrame
			lp.X, lp.Y = (f.X-minX)/width, (f.Y-minY)/height
			lp.W, lp.H = f.Width/width, f.Height/height
		}
		out.Panes = append(out.Panes, lp)
	}
	return out
}

func nonNil(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}
