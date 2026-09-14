package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sodre90/cmux-bridge/internal/wire"
)

const fakeFeedScript = `#!/bin/sh
printf '%s\n' "$*" >> "$CMUX_FAKE_LOG"
echo '{"ok":true}'
`

func postFeed(t *testing.T, srvURL, tok, path, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", srvURL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestFeedPermissionReplyRouted(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeFeedScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp := postFeed(t, srv.URL, tok, "/feed/F1/reply",
		`{"kind":"permissionRequest","request_id":"REQ1","params":{"decision":"approve"}}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	data, _ := os.ReadFile(logPath)
	log := string(data)
	for _, want := range []string{"feed.permission.reply", "REQ1", "request_id", "approve"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log missing %q; got:\n%s", want, log)
		}
	}
}

func TestFeedQuestionAndExitPlanMethods(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeFeedScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for kind, method := range map[string]string{
		"question": "feed.question.reply",
		"exitPlan": "feed.exit_plan.reply",
	} {
		resp := postFeed(t, srv.URL, tok, "/feed/F1/reply",
			`{"kind":"`+kind+`","request_id":"R2"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("kind %s: want 200, got %d", kind, resp.StatusCode)
		}
		data, _ := os.ReadFile(logPath)
		if !strings.Contains(string(data), method) {
			t.Fatalf("expected %s in log for kind %s", method, kind)
		}
	}
}

func TestFeedMissingRequestID400(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeFeedScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp := postFeed(t, srv.URL, tok, "/feed/F1/reply", `{"kind":"permissionRequest"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for missing request_id, got %d", resp.StatusCode)
	}
	if data, _ := os.ReadFile(logPath); len(data) != 0 {
		t.Fatalf("cmux must not be called on validation failure; log:\n%s", data)
	}
}

func TestFeedPendingListsItems(t *testing.T) {
	// The agent must surface the full question structure (request_id, options)
	// so the app can reply; the endpoint passes cmux feed.list through verbatim.
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$CMUX_FAKE_LOG"
echo '{"items":[{"id":"I1","request_id":"REQ1","kind":"question","status":"pending","title":"AskUserQuestion","question_multi_select":false,"questions":[{"id":"q0","prompt":"Pick","options":[{"id":"opt0","label":"A"},{"id":"opt1","label":"B"}]}]}]}'
`
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, script)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/feed/pending", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"REQ1", "question", "opt0", "AskUserQuestion"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("response missing %q; got:\n%s", want, body)
		}
	}
	logData, _ := os.ReadFile(logPath)
	if !strings.Contains(string(logData), "feed.list") || !strings.Contains(string(logData), "pending_only") {
		t.Fatalf("cmux not called with feed.list pending_only; got:\n%s", logData)
	}
}

// TestFeedPendingCanonicalizesCWD reproduces the same live-observed symlink
// mismatch as yolo_test.go's TestResolvePendingPermissionMatchesSymlinkedCWD:
// cmux's feed.list reports an item's cwd as a symlink alias while
// mobile.workspace.list (surfaced through /sessions) reports the resolved
// form for the same location. /feed/pending must rewrite "cwd" to its
// canonical form so the app's cwd-based item-to-workspace matching
// (pendingItemTarget in SessionsLogic.kt) has a chance of succeeding.
func TestFeedPendingCanonicalizesCWD(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	script := `#!/bin/sh
printf '%s\n' "$*" >> "$CMUX_FAKE_LOG"
echo '{"items":[{"id":"I1","request_id":"REQ1","kind":"question","status":"pending","cwd":"` + alias + `"}]}'
`
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, script)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/feed/pending", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body struct {
		Items []struct {
			CWD string `json:"cwd"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0].CWD != real {
		t.Fatalf("want cwd rewritten to canonical %q, got %+v", real, body.Items)
	}
}

func TestFeedUnknownKind400(t *testing.T) {
	s, tok := newTestServer(t, fakeFeedScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp := postFeed(t, srv.URL, tok, "/feed/F1/reply", `{"kind":"bogus","request_id":"R"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for unknown kind, got %d", resp.StatusCode)
	}
}

func TestPromptBodyRendersEachKind(t *testing.T) {
	cases := []struct {
		name string
		item pendingFeedItem
		want string
	}{
		{"question uses its own text",
			pendingFeedItem{Kind: "question", QuestionPrompt: "Read-only or writable?"},
			"Read-only or writable?"},
		{"permission names the tool and what it acts on",
			pendingFeedItem{Kind: "permissionRequest", ToolName: "Edit", ToolInput: json.RawMessage(`{"file_path":"/tmp/x.sh","new_string":"..."}`)},
			"Wants to run Edit: /tmp/x.sh"},
		{"permission survives cmux dropping its double encoding of tool_input",
			pendingFeedItem{Kind: "permissionRequest", ToolName: "Bash", ToolInput: json.RawMessage(`"{\"command\":\"ls -la\"}"`)},
			"Wants to run Bash: ls -la"},
		{"permission with unrecognized args still names the tool",
			pendingFeedItem{Kind: "permissionRequest", ToolName: "Frobnicate", ToolInput: json.RawMessage(`{"whatsit":7}`)},
			"Wants to run Frobnicate"},
		{"permission with unparseable args still names the tool",
			pendingFeedItem{Kind: "permissionRequest", ToolName: "Bash", ToolInput: json.RawMessage("not json")},
			"Wants to run Bash"},
		{"permission with absent args still names the tool",
			pendingFeedItem{Kind: "permissionRequest", ToolName: "Bash"},
			"Wants to run Bash"},
		{"a kind with no text worth showing defers to the caller's fallback",
			pendingFeedItem{Kind: "exitPlan"}, ""},
		{"an empty question defers to the caller's fallback",
			pendingFeedItem{Kind: "question"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := promptBody(tc.item); got != tc.want {
				t.Fatalf("promptBody() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A notification body is rendered on one or two phone lines, so multi-line
// prompt text has to collapse and long text has to stop somewhere.
func TestTruncateForNotificationCollapsesAndCaps(t *testing.T) {
	if got := truncateForNotification("run  this\nthen\tthat\n"); got != "run this then that" {
		t.Fatalf("whitespace not collapsed: %q", got)
	}
	long := strings.Repeat("é", maxNotificationBody+50)
	got := truncateForNotification(long)
	if r := []rune(got); len(r) != maxNotificationBody+1 || r[len(r)-1] != '…' {
		t.Fatalf("want %d runes ending in an ellipsis, got %d runes: %q", maxNotificationBody+1, len(r), got)
	}
	exact := strings.Repeat("a", maxNotificationBody)
	if got := truncateForNotification(exact); got != exact {
		t.Fatalf("text at exactly the cap must not be truncated, got %q", got)
	}
}

func TestAgentStatusLineKeepsOnlyRecognizedStatuses(t *testing.T) {
	// The host marks a preview it recognised as an agent status by setting
	// Attention; only those previews are worth showing as a notification body.
	for _, keep := range []wire.Workspace{
		{Preview: "Claude is waiting for your input", Attention: "input"},
		{Preview: "Codex needs your permission", Attention: "permission"},
	} {
		if got := agentStatusLine(keep); got != keep.Preview {
			t.Fatalf("agentStatusLine(%q) = %q, want it kept", keep.Preview, got)
		}
	}
	// The live-observed banner behind this whole fix, plus ordinary preview text.
	for _, drop := range []string{
		"macOS is reporting sustained critical memory pressure. cmux has shed hidden resources",
		"Build options trading system",
		"",
	} {
		if got := agentStatusLine(wire.Workspace{Preview: drop}); got != "" {
			t.Fatalf("agentStatusLine(%q) = %q, want it dropped", drop, got)
		}
	}
}

func TestNewestPendingForCWDPicksTheLatestMatch(t *testing.T) {
	items := []pendingFeedItem{
		{RequestID: "OLD", Status: "pending", CWD: "/tmp/proj", CreatedAt: "2026-07-25T13:00:00Z"},
		{RequestID: "OTHER", Status: "pending", CWD: "/tmp/elsewhere", CreatedAt: "2026-07-25T15:00:00Z"},
		{RequestID: "NEW", Status: "pending", CWD: "/tmp/proj", CreatedAt: "2026-07-25T14:00:00Z"},
		{RequestID: "RESOLVED", Status: "expired", CWD: "/tmp/proj", CreatedAt: "2026-07-25T16:00:00Z"},
	}
	got, ok := newestPendingForCWD(items, "/tmp/proj")
	if !ok || got.RequestID != "NEW" {
		t.Fatalf("got %+v (ok=%v), want the newest still-pending item in that cwd", got, ok)
	}
	if _, ok := newestPendingForCWD(items, ""); ok {
		t.Fatal("an unknown workspace cwd must not match every pending item")
	}
	if _, ok := newestPendingForCWD(nil, "/tmp/proj"); ok {
		t.Fatal("no pending items must not match")
	}
}

// TestPendingFeedItemDecodesRealCmuxPayload parses bytes captured from a live
// `cmux rpc feed.list` (testdata/feed_list_pending.json). Only cwd and status
// were rewritten, so both items look pending in one workspace, and a home
// directory inside tool_input was anonymized -- the double-encoding and
// escaping that the parser actually has to cope with are untouched.
//
// This package has shipped a plausible-looking schema that cmux never emits
// before, with unit tests that asserted the same fiction and stayed green --
// see resolvePendingPermission's `kind == "permission"` bug. Hand-written
// fixtures cannot catch that class of error; real captured bytes can.
func TestPendingFeedItemDecodesRealCmuxPayload(t *testing.T) {
	raw, err := os.ReadFile("testdata/feed_list_pending.json")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Items []pendingFeedItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("real feed.list payload must decode: %v", err)
	}

	byKind := map[string]pendingFeedItem{}
	for _, item := range resp.Items {
		byKind[item.Kind] = item
	}
	question, ok := byKind["question"]
	if !ok {
		t.Fatal("fixture should carry a question item")
	}
	if question.RequestID == "" || question.CreatedAt == "" {
		t.Fatalf("reply/ordering fields did not decode: %+v", question)
	}
	if got := promptBody(question); got != "What should the push notification body say when an agent needs you?" {
		t.Fatalf("question body = %q", got)
	}

	permission, ok := byKind["permissionRequest"]
	if !ok {
		t.Fatal("fixture should carry a permissionRequest item")
	}
	if got := promptBody(permission); got != "Wants to run Edit: /Users/u/scripts/list-watchers.sh" {
		t.Fatalf("permission body = %q", got)
	}

	// The same decode feeds YOLO's auto-approve, so a drift that breaks
	// unblocking an agent fails here too, not just the notification text.
	if _, ok := newestPendingForCWD(resp.Items, "/tmp/proj"); !ok {
		t.Fatal("real pending items must match their workspace by cwd")
	}
}
