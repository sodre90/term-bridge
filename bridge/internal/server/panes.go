package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"

	"github.com/sodre90/cmux-bridge/internal/httpjson"
	"github.com/sodre90/cmux-bridge/internal/wire"
)

var errSurfaceNotInWorkspace = errors.New("surface not in workspace")

func splitDirection(placement string) (string, bool) {
	switch placement {
	case wire.PlacementLeft, wire.PlacementRight, wire.PlacementUp, wire.PlacementDown:
		return placement, true
	}
	return "", false
}

// handleCreatePane is POST /sessions/{id}/panes: a new terminal beside the
// surface the phone is viewing (surface.split) or as a tab in its pane
// (surface.create). Either way cmux is told exactly which surface or pane,
// and the new terminal does not take focus on the Mac.
func (s *Server) handleCreatePane(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var req wire.CreatePaneRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !isUUID(req.SurfaceID) {
		httpjson.Error(w, http.StatusBadRequest, "invalid surface_id")
		return
	}
	direction, isSplit := splitDirection(req.Placement)
	if !isSplit && req.Placement != wire.PlacementTab {
		httpjson.Error(w, http.StatusBadRequest, "invalid placement")
		return
	}
	var (
		raw []byte
		err error
	)
	if isSplit {
		raw, err = s.cmux.Rpc(r.Context(), "surface.split", map[string]any{
			"surface_id": req.SurfaceID,
			"direction":  direction,
			"focus":      false,
		})
	} else {
		raw, err = s.createTab(r.Context(), id, req.SurfaceID)
	}
	if errors.Is(err, errSurfaceNotInWorkspace) {
		httpjson.Error(w, http.StatusNotFound, "surface not in workspace")
		return
	}
	if err != nil {
		slog.Warn("server: create pane failed", "workspace", id, "surface", req.SurfaceID, "placement", req.Placement, "err", err)
		httpjson.Error(w, http.StatusBadGateway, "cmux create pane failed")
		return
	}
	var created wire.CreatePaneResponse
	if err := json.Unmarshal(raw, &created); err != nil || created.SurfaceID == "" {
		slog.Warn("server: create pane reply unreadable", "err", err)
		httpjson.Error(w, http.StatusBadGateway, "cmux create pane failed")
		return
	}
	slog.Info("server: pane created", "workspace", id, "from", req.SurfaceID, "placement", req.Placement, "surface", created.SurfaceID, "pane", created.PaneID)
	httpjson.Write(w, http.StatusOK, created)
}

// createTab finds the pane holding surfaceID and adds a terminal tab to it.
func (s *Server) createTab(ctx context.Context, workspaceID, surfaceID string) (json.RawMessage, error) {
	panes, err := s.listPanes(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(panes, func(p paneEntry) bool { return slices.Contains(p.SurfaceIDs, surfaceID) })
	if i < 0 {
		return nil, errSurfaceNotInWorkspace
	}
	return s.cmux.Rpc(ctx, "surface.create", map[string]any{
		"workspace_id": workspaceID,
		"pane_id":      panes[i].ID,
		"type":         "terminal",
		"focus":        false,
	})
}

// handleCloseSurface is DELETE /sessions/{id}/panes/{surfaceId}.
func (s *Server) handleCloseSurface(w http.ResponseWriter, r *http.Request) {
	if _, ok := pathUUID(w, r, "id"); !ok {
		return
	}
	surfaceID, ok := pathUUID(w, r, "surfaceId")
	if !ok {
		return
	}
	if _, err := s.cmux.Rpc(r.Context(), "surface.close", map[string]any{"surface_id": surfaceID}); err != nil {
		slog.Warn("server: surface.close failed", "surface", surfaceID, "err", err)
		httpjson.Error(w, http.StatusBadGateway, "cmux surface.close failed")
		return
	}
	slog.Info("server: surface closed", "surface", surfaceID)
	httpjson.Write(w, http.StatusOK, map[string]bool{"ok": true})
}
