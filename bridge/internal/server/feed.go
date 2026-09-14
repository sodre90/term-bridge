package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/sodre90/term-bridge/internal/host"
	"github.com/sodre90/term-bridge/internal/httpjson"
	"github.com/sodre90/term-bridge/internal/wire"
)

// FeedReply answers an agent prompt. Params are forwarded to the host
// verbatim together with the required request_id; the host knows each
// kind's own param names (see cmuxhost.FeedReply for the live-confirmed
// cmux shapes).
type FeedReply struct {
	Kind      string         `json:"kind"`       // wire.FeedKind*
	RequestID string         `json:"request_id"` // required
	Params    map[string]any `json:"params,omitempty"`
}

// handleFeedPending returns the agent's pending blocking prompts as the host
// produces them: the full question structure (request_id,
// questions[].options[], question_multi_select) the app needs to render
// choices and reply, with each item's cwd already matching /sessions'
// Workspace.CWD byte-for-byte.
func (s *Server) handleFeedPending(w http.ResponseWriter, r *http.Request) {
	body, err := s.host.PendingFeed(r.Context())
	if err != nil {
		httpjson.Error(w, http.StatusBadGateway, "feed unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// pendingFeedItem is the subset of a `feed.list --pending_only` item this
// package needs. See android's Dtos.kt PendingFeedItem for the full shape.
type pendingFeedItem struct {
	RequestID string `json:"request_id"`
	Kind      string `json:"kind"`
	CWD       string `json:"cwd"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	// QuestionPrompt carries a "question" item's real question text. The event
	// stream redacts prompt text; feed.list does not, so this is the only
	// place the agent's actual words are available to us.
	QuestionPrompt string `json:"question_prompt"`
	ToolName       string `json:"tool_name"`
	// ToolInput is a "permissionRequest" item's gated tool arguments, e.g.
	// Bash's {"command":"...","description":"..."}. Held raw because cmux
	// double-encodes it as a JSON *string* containing that JSON rather than as
	// a nested object: decoding it into a Go string would make every field
	// here, including the ones resolvePendingPermission needs to unblock an
	// agent, fail to decode the day cmux stops doing that.
	ToolInput json.RawMessage `json:"tool_input"`
}

// listPendingItems returns cmux's currently pending blocking prompts, or nil
// on any RPC/parse failure -- every caller treats "no pending item" and "could
// not find out" the same way, by falling back rather than failing.
func (s *Server) listPendingItems(ctx context.Context) []pendingFeedItem {
	raw, err := s.host.PendingFeed(ctx)
	if err != nil {
		return nil
	}
	var resp struct {
		Items []pendingFeedItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil
	}
	return resp.Items
}

// newestPendingForCWD returns the most recently created pending item running
// in wantCWD. Pending items are keyed by workstream_id -- the agent's own
// session ID, a different ID space than cmux's workspace ID -- so cwd is the
// one field a pending item and a workspace share; callers must canonicalize
// both sides first (see resolvePendingPermission's doc comment). created_at is
// cmux's RFC3339 UTC timestamp, so comparing it as a string orders correctly.
func newestPendingForCWD(items []pendingFeedItem, wantCWD string) (pendingFeedItem, bool) {
	var newest pendingFeedItem
	found := false
	if wantCWD == "" {
		return newest, false
	}
	for _, item := range items {
		if item.Status != "pending" || canonicalPath(item.CWD) != wantCWD {
			continue
		}
		if !found || item.CreatedAt > newest.CreatedAt {
			newest, found = item, true
		}
	}
	return newest, found
}

// promptBody renders a pending prompt as push-notification body text: what the
// agent is actually asking, rather than whatever unrelated line cmux's
// workspace preview happens to hold. Returns "" for a kind with no text worth
// showing, leaving the caller on its existing fallbacks.
func promptBody(item pendingFeedItem) string {
	switch item.Kind {
	case "question":
		return truncateForNotification(item.QuestionPrompt)
	case "permissionRequest":
		return permissionBody(item)
	}
	return ""
}

func permissionBody(item pendingFeedItem) string {
	if item.ToolName == "" {
		return ""
	}
	if detail := toolInputSummary(item.ToolInput); detail != "" {
		return truncateForNotification("Wants to run " + item.ToolName + ": " + detail)
	}
	return "Wants to run " + item.ToolName
}

// toolInputSummaryKeys are the tool_input fields that say what a gated tool is
// about to do, best-first: the shell command, then the path/pattern/URL the
// file and search tools act on, then the tool's own description. An unknown
// tool matching none of them still gets its name shown, just without detail.
var toolInputSummaryKeys = []string{"command", "file_path", "path", "pattern", "url", "description"}

func toolInputSummary(toolInput json.RawMessage) string {
	var nested string
	if err := json.Unmarshal(toolInput, &nested); err == nil {
		toolInput = json.RawMessage(nested) // unwrap cmux's double encoding
	}
	var args map[string]any
	if err := json.Unmarshal(toolInput, &args); err != nil {
		return ""
	}
	for _, k := range toolInputSummaryKeys {
		if val, ok := args[k].(string); ok && val != "" {
			return val
		}
	}
	return ""
}

// maxNotificationBody bounds a push body in runes. A phone collapses a
// notification to about two lines anyway, and this text is e2e-encrypted once
// per paired device into a single FCM data message, so an unbounded prompt
// (a multi-paragraph question, a whole file's contents in an Edit's args)
// would blow the payload budget for every device at once.
const maxNotificationBody = 160

// truncateForNotification collapses whitespace -- prompt text and shell
// commands are routinely multi-line, which a notification renders as a single
// run-on line -- and caps the result at maxNotificationBody runes.
func truncateForNotification(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= maxNotificationBody {
		return s
	}
	return strings.TrimRight(string(r[:maxNotificationBody]), " ") + "…"
}

func (s *Server) handleFeedReply(w http.ResponseWriter, r *http.Request) {
	var fr FeedReply
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&fr); err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !wire.KnownFeedKind(fr.Kind) {
		httpjson.Error(w, http.StatusBadRequest, "unknown kind")
		return
	}
	if fr.RequestID == "" {
		httpjson.Error(w, http.StatusBadRequest, "missing request_id")
		return
	}
	err := s.host.FeedReply(r.Context(), fr.Kind, fr.RequestID, fr.Params)
	switch {
	case errors.Is(err, host.ErrUnsupported):
		httpjson.Error(w, http.StatusBadRequest, "unsupported on this host")
		return
	case errors.Is(err, host.ErrPromptGone):
		httpjson.Error(w, http.StatusConflict, "prompt_gone")
		return
	case err != nil:
		httpjson.Error(w, http.StatusBadGateway, "reply failed")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]bool{"ok": true})
}
