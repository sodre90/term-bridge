package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"

	"github.com/sodre90/cmux-bridge/internal/httpjson"
	"github.com/sodre90/cmux-bridge/internal/wire"
	"github.com/sodre90/cmux-bridge/internal/yolo"
)

type setYoloModeRequest struct {
	Mode string `json:"mode"`
}

// handleSetYoloMode persists a workspace's opt-in auto-reply mode for
// permission prompts. When turning a mode on, it immediately resolves any
// permission already pending for that workspace, so enabling e.g. Bypass
// unsticks an already-blocked workspace without waiting for the next event.
func (s *Server) handleSetYoloMode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req setYoloModeRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !yolo.Valid(req.Mode) {
		httpjson.Error(w, http.StatusBadRequest, "invalid mode")
		return
	}
	if s.yolo == nil {
		httpjson.Error(w, http.StatusServiceUnavailable, "yolo store unavailable")
		return
	}
	if err := s.yolo.SetMode(id, req.Mode); err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "persist failed")
		return
	}
	if req.Mode != "" {
		s.resolvePendingPermission(r.Context(), id, req.Mode)
	}
	httpjson.Write(w, http.StatusOK, map[string]bool{"ok": true})
}

// yoloMode reads a workspace's configured YOLO mode ("" when off or when YOLO
// isn't configured at all). A cheap local read with no RPC cost, so callers on
// the event path can check it before deciding to spend any.
func (s *Server) yoloMode(workspaceID string) string {
	if s.yolo == nil || workspaceID == "" {
		return ""
	}
	return s.yolo.Mode(workspaceID)
}

// resolvePendingPermission finds any pending "permissionRequest" feed item
// whose cwd matches workspaceID's live working directory and replies to it
// with mode, unblocking the agent without a phone tap. Returns whether at
// least one item was found and successfully replied to. Used by the
// yolo-mode endpoint, which has no lookups of its own to share; the event path
// calls replyPendingPermissions directly with the state it already fetched.
func (s *Server) resolvePendingPermission(ctx context.Context, workspaceID, mode string) bool {
	ws, ok := s.findWorkspace(ctx, workspaceID)
	if !ok || ws.CWD == "" {
		return false
	}
	return s.replyPendingPermissions(ctx, s.listPendingItems(ctx), canonicalPath(ws.CWD), mode)
}

// replyPendingPermissions answers every pending permission request running in
// wantCWD with mode.
//
// Pending items are keyed by workstream_id -- the agent's own session ID
// (e.g. "claude-<uuid>"), confirmed live to be a different ID space than
// cmux's workspace ID -- so correlation is done on cwd instead, the one field
// both a pending item and a workspace carry in common. Callers must pass a
// canonicalized wantCWD: mobile.workspace.list's current_directory and
// feed.list's cwd disagree on symlinks (confirmed live, e.g. "/tmp/foo" vs
// "/private/tmp/foo"), so comparing raw strings would silently drop every
// match for a workspace under /tmp, /var, /etc, or any other symlinked path.
func (s *Server) replyPendingPermissions(ctx context.Context, items []pendingFeedItem, wantCWD, mode string) bool {
	if wantCWD == "" {
		return false
	}
	resolvedAny := false
	for _, item := range items {
		if item.Kind != "permissionRequest" || item.Status != "pending" || canonicalPath(item.CWD) != wantCWD {
			continue
		}
		if err := s.host.FeedReply(ctx, wire.FeedKindPermissionRequest, item.RequestID,
			map[string]any{"mode": mode}); err == nil {
			resolvedAny = true
		}
	}
	return resolvedAny
}

// canonicalPath resolves symlinks so two working directories can be compared
// for equality regardless of how they were reported; a path that no longer
// exists (or any other resolution failure) is returned unchanged rather than
// dropped. The host already canonicalises what it hands us; this is the
// belt to that brace.
func canonicalPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}
