package server

import (
	"encoding/json"
	"net/http"

	"github.com/sodre90/term-bridge/internal/httpjson"
)

type renameWorkspaceRequest struct {
	Title string `json:"title"`
}

// handleRenameWorkspace sets a workspace's persistent display title in cmux
// via its documented workspace.rename RPC (the same one behind cmux's own
// `cmux rename-workspace` CLI command and Cmd+Shift+R shortcut). It was the
// bridge's only workspace mutation until create/select/close arrived in
// workspaces.go and panes.go; see README's "What the app does".
func (s *Server) handleRenameWorkspace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req renameWorkspaceRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Title == "" {
		httpjson.Error(w, http.StatusBadRequest, "missing title")
		return
	}
	if err := s.host.RenameWorkspace(r.Context(), id, req.Title); err != nil {
		httpjson.Error(w, http.StatusBadGateway, "cmux rename failed")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]bool{"ok": true})
}
