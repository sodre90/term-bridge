package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/sodre90/cmux-bridge/internal/host"
	"github.com/sodre90/cmux-bridge/internal/httpjson"
	"github.com/sodre90/cmux-bridge/internal/wire"
)

// findWorkspace fetches the live workspace list and returns the one matching
// id, if any. Used by callers that need a single workspace's live fields
// (CWD, title, preview) without re-deriving the whole list themselves.
func (s *Server) findWorkspace(ctx context.Context, id string) (wire.Workspace, bool) {
	workspaces, err := s.host.ListWorkspaces(ctx)
	if err != nil {
		return wire.Workspace{}, false
	}
	for _, ws := range workspaces {
		if ws.ID == id {
			return ws, true
		}
	}
	return wire.Workspace{}, false
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	workspaces, err := s.host.ListWorkspaces(r.Context())
	if errors.Is(err, host.ErrMalformed) {
		httpjson.Error(w, http.StatusBadGateway, "cmux parse error")
		return
	}
	if err != nil {
		httpjson.Error(w, http.StatusBadGateway, "cmux unavailable")
		return
	}
	if s.yolo != nil {
		for i := range workspaces {
			workspaces[i].YoloMode = s.yolo.Mode(workspaces[i].ID)
		}
	}
	httpjson.Write(w, http.StatusOK, wire.SessionsResponse{Workspaces: workspaces, Host: s.hostInfo})
}
