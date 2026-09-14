package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sodre90/cmux-bridge/internal/httpjson"
	"github.com/sodre90/cmux-bridge/internal/wire"
)

// Every mutating route names its target by a well-formed host id and
// refuses anything else before a backend call is made: cmux's create
// methods fall back to the window or pane focused on the Mac when a target
// is missing, which is never what the phone meant.
func (s *Server) pathID(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	id := r.PathValue(name)
	if !s.host.ValidID(id) {
		httpjson.Error(w, http.StatusBadRequest, "invalid "+name)
		return "", false
	}
	return id, true
}

const maxWorkspaceTitleLen = 120

var (
	errCwdNotAbsolute = errors.New("cwd must be absolute")
	errCwdNotFound    = errors.New("cwd not found")
	errCwdNotDir      = errors.New("cwd is not a directory")
	errCwdOutsideHome = errors.New("cwd is outside the home directory")
)

// validateWorkspaceCwd resolves a requested directory on the Mac and
// accepts it only when it exists, is a directory and lies inside the home
// directory once symlinks are followed on both sides. The resolved path is
// what cmux gets.
func validateWorkspaceCwd(cwd, home string) (string, error) {
	if !filepath.IsAbs(cwd) {
		return "", errCwdNotAbsolute
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", errCwdNotFound
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", errCwdNotFound
	}
	if !info.IsDir() {
		return "", errCwdNotDir
	}
	homeResolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", errCwdOutsideHome
	}
	rel, err := filepath.Rel(homeResolved, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errCwdOutsideHome
	}
	return resolved, nil
}

func cwdRefusal(err error) string {
	switch {
	case errors.Is(err, errCwdNotAbsolute):
		return "cwd_not_absolute"
	case errors.Is(err, errCwdNotFound):
		return "cwd_not_found"
	case errors.Is(err, errCwdNotDir):
		return "cwd_not_dir"
	default:
		return "cwd_outside_home"
	}
}

// handleCreateWorkspace is POST /sessions: a new cmux workspace in a
// directory under the user's home, left unfocused on the Mac.
func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var req wire.CreateWorkspaceRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid json")
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "home directory unknown")
		return
	}
	cwd, err := validateWorkspaceCwd(req.CWD, home)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, cwdRefusal(err))
		return
	}
	title := strings.TrimSpace(req.Title)
	if len(title) > maxWorkspaceTitleLen {
		httpjson.Error(w, http.StatusBadRequest, "title too long")
		return
	}
	created, err := s.host.CreateWorkspace(r.Context(), cwd, title)
	if err != nil {
		slog.Warn("server: workspace.create failed", "err", err)
		httpjson.Error(w, http.StatusBadGateway, "cmux workspace.create failed")
		return
	}
	slog.Info("server: workspace created", "workspace", created.WorkspaceID, "surface", created.SurfaceID)
	httpjson.Write(w, http.StatusOK, created)
}

// handleSelectWorkspace is POST /sessions/{id}/select: make the Mac show
// this workspace and, when the body names one, focus a surface in it.
func (s *Server) handleSelectWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r, "id")
	if !ok {
		return
	}
	var req wire.SelectRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpjson.Error(w, http.StatusBadRequest, "invalid json")
			return
		}
	}
	if req.SurfaceID != "" && !s.host.ValidID(req.SurfaceID) {
		httpjson.Error(w, http.StatusBadRequest, "invalid surface_id")
		return
	}
	if err := s.host.SelectWorkspace(r.Context(), id); err != nil {
		slog.Warn("server: workspace.select failed", "workspace", id, "err", err)
		httpjson.Error(w, http.StatusBadGateway, "cmux workspace.select failed")
		return
	}
	if req.SurfaceID != "" {
		if err := s.host.FocusSurface(r.Context(), req.SurfaceID); err != nil {
			slog.Warn("server: surface.focus failed", "surface", req.SurfaceID, "err", err)
			httpjson.Error(w, http.StatusBadGateway, "cmux surface.focus failed")
			return
		}
	}
	slog.Info("server: shown on mac", "workspace", id, "surface", req.SurfaceID)
	httpjson.Write(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleCloseWorkspace is DELETE /sessions/{id}. The phone confirms before
// calling; the bridge closes what it is told.
func (s *Server) handleCloseWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r, "id")
	if !ok {
		return
	}
	if err := s.host.CloseWorkspace(r.Context(), id); err != nil {
		slog.Warn("server: workspace.close failed", "workspace", id, "err", err)
		httpjson.Error(w, http.StatusBadGateway, "cmux workspace.close failed")
		return
	}
	slog.Info("server: workspace closed", "workspace", id)
	httpjson.Write(w, http.StatusOK, map[string]bool{"ok": true})
}
