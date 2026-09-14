package server

import (
	"log/slog"
	"net/http"

	"github.com/sodre90/cmux-bridge/internal/host"
	"github.com/sodre90/cmux-bridge/internal/httpjson"
	"github.com/sodre90/cmux-bridge/internal/wire"
)

// handleLayout answers GET /sessions/{id}/layout from the host's pane list.
func (s *Server) handleLayout(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r, "id")
	if !ok {
		return
	}
	panes, err := s.host.ListPanes(r.Context(), id)
	if err != nil {
		slog.Warn("server: pane.list failed", "workspace", id, "err", err)
		httpjson.Error(w, http.StatusBadGateway, "cmux pane.list failed")
		return
	}
	httpjson.Write(w, http.StatusOK, normaliseLayout(panes))
}

// normaliseLayout maps the host's pane frames onto the unit square. The
// frames are taken relative to the panes' own bounding box: cmux reports
// them in window coordinates, so the first pane starts at the sidebar's
// width, not at zero. A workspace never shown on the Mac reports every frame
// as zero; those are laid out as equal columns and flagged as estimated.
func normaliseLayout(panes []host.Pane) wire.Layout {
	out := wire.Layout{Panes: make([]wire.LayoutPane, 0, len(panes))}
	if len(panes) == 0 {
		return out
	}
	minX, minY := panes[0].Frame.X, panes[0].Frame.Y
	maxX, maxY := minX, minY
	for _, p := range panes {
		f := p.Frame
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
			f := p.Frame
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
