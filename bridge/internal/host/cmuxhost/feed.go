package cmuxhost

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sodre90/cmux-bridge/internal/host"
	"github.com/sodre90/cmux-bridge/internal/wire"
)

// PendingFeed forwards cmux's feed.list with pending_only. The result is
// passed through as-is so the app receives the full question structure
// (request_id, questions[].options[], question_multi_select) it needs to
// render choices and reply -- except each item's "cwd", which is rewritten
// to its canonical form (see canonicalizeFeedCWDs) so it matches
// wire.Workspace.CWD byte-for-byte.
func (h *Host) PendingFeed(ctx context.Context) (json.RawMessage, error) {
	raw, err := h.client.Rpc(ctx, "feed.list", map[string]any{"pending_only": true})
	if err != nil {
		return nil, err
	}
	return canonicalizeFeedCWDs(raw), nil
}

// canonicalizeFeedCWDs rewrites each pending item's "cwd" to its
// symlink-resolved form, mirroring parseWorkspaces' canonicalization of
// Workspace.CWD. cmux's feed.list and mobile.workspace.list disagree on
// symlinks (e.g. /tmp/foo vs /private/tmp/foo -- see
// resolvePendingPermission's doc comment in server, which hit this live),
// and the app's own cwd-based item-to-workspace matching (pendingItemTarget
// in SessionsLogic.kt) needs both sides normalized the same way to have any
// chance of matching. Falls back to the raw bytes unchanged on any parse
// failure -- a shape cmux might change shouldn't break the primary read.
func canonicalizeFeedCWDs(raw []byte) []byte {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw
	}
	items, ok := root["items"].([]any)
	if !ok {
		return raw
	}
	for _, it := range items {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if cwd, ok := item["cwd"].(string); ok && cwd != "" {
			item["cwd"] = canonicalPath(cwd)
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		return raw
	}
	return out
}

// FeedReply answers a prompt. Params are forwarded to cmux verbatim with the
// required request_id injected.
//
// Confirmed live against `cmux rpc feed.permission.reply` (via its
// invalid_params decode errors, with a fake request_id so nothing real was
// ever affected): a "permissionRequest" reply's params is `mode`, one of
// `once`, `always`, `all`, `bypass`, `deny` -- `once`/`deny` are a one-shot
// manual reply, the other three are the recurring YOLO auto-modes (see
// android's YoloMode). "question"'s `selections` was already confirmed live.
// "exitPlan" is not yet confirmed against a real exit-plan prompt and may
// need correcting the same way "permission" did.
func (h *Host) FeedReply(ctx context.Context, kind, requestID string, params map[string]any) error {
	method, ok := feedMethod(kind)
	if !ok {
		return fmt.Errorf("feed kind %q: %w", kind, host.ErrUnsupported)
	}
	merged := map[string]any{}
	for k, v := range params {
		merged[k] = v
	}
	merged["request_id"] = requestID
	_, err := h.client.Rpc(ctx, method, merged)
	return err
}

func feedMethod(kind string) (string, bool) {
	switch kind {
	case wire.FeedKindPermissionRequest:
		return "feed.permission.reply", true
	case wire.FeedKindQuestion:
		return "feed.question.reply", true
	case wire.FeedKindExitPlan:
		return "feed.exit_plan.reply", true
	}
	return "", false
}
