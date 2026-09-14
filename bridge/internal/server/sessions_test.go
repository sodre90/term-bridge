package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/cmux"
	"github.com/sodre90/term-bridge/internal/testutil"
	"github.com/sodre90/term-bridge/internal/wire"
	"github.com/sodre90/term-bridge/internal/yolo"
)

// realistic-shaped mobile.workspace.list payload: a workspace duplicated across
// groups + top-level, a multi-pane workspace, and an empty-pane workspace.
// Each pane object also has id + current_directory but NO terminals array, so
// only the workspaces must be collected.
const fakeWorkspaceList = `{
  "groups": [
    {"workspaces": [
      {"id":"882CA6F0","current_directory":"/Users/u/prj/trading","preview":"Build options trading system","title":"✳ Build options","has_unread":true,"custom_color":"#6A1B9A",
       "terminals":[{"id":"T1","current_directory":"/Users/u/prj/trading","title":"✳ Build options","is_focused":true,"is_ready":true}]}
    ]}
  ],
  "workspaces": [
    {"id":"882CA6F0","current_directory":"/Users/u/prj/trading","preview":"Build options trading system","title":"✳ Build options","has_unread":true,"custom_color":"#6A1B9A",
     "terminals":[{"id":"T1","current_directory":"/Users/u/prj/trading","title":"✳ Build options","is_focused":true,"is_ready":true}]},
    {"id":"E43BBF04","current_directory":"/Users/u/prj/trading","preview":"shell","title":"~/prj/trading","has_unread":false,
     "terminals":[
       {"id":"T2","current_directory":"/Users/u/prj/trading","title":"~/prj/trading","is_focused":true,"is_ready":true},
       {"id":"T3","current_directory":"/Users/u/prj/trading","title":"✳ Review PR","is_focused":false,"is_ready":false}
     ]},
    {"id":"EMPTY01","current_directory":"/Users/u/prj/x","preview":"empty","title":"~/prj/x","terminals":[]}
  ]
}`

func newTestServer(t *testing.T, script string) (*Server, string) {
	t.Helper()
	bin := testutil.WriteFakeCmux(t, script)
	store, err := auth.Open(t.TempDir() + "/d.db")
	if err != nil {
		t.Fatal(err)
	}
	tenant, _ := store.CreateTenant()
	tok, _ := store.Issue(tenant, "phone", "test-device-pubkey-b64")
	s := New(&cmux.Client{Bin: bin}, store)
	yoloStore, err := yolo.Open(t.TempDir() + "/yolo.json")
	if err != nil {
		t.Fatal(err)
	}
	s.SetYoloStore(yoloStore)
	return s, tok
}

func TestSessionsRequiresToken(t *testing.T) {
	s, _ := newTestServer(t, "#!/bin/sh\necho '{}'\n")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", resp.StatusCode)
	}
}

func TestSessionsDedupAndShape(t *testing.T) {
	script := "#!/bin/sh\ncat <<'JSON'\n" + fakeWorkspaceList + "\nJSON\n"
	s, tok := newTestServer(t, script)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var body struct {
		Workspaces []wire.Workspace `json:"workspaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Workspaces) != 3 {
		t.Fatalf("want 3 deduped workspaces, got %d: %+v", len(body.Workspaces), body.Workspaces)
	}
	byID := map[string]wire.Workspace{}
	for _, w := range body.Workspaces {
		byID[w.ID] = w
	}
	// Panes must NOT leak as top-level workspaces.
	for _, leaked := range []string{"T1", "T2", "T3"} {
		if _, bad := byID[leaked]; bad {
			t.Fatalf("pane %q leaked as a workspace", leaked)
		}
	}
	ws, ok := byID["882CA6F0"]
	if !ok {
		t.Fatal("missing workspace 882CA6F0")
	}
	if ws.CWD != "/Users/u/prj/trading" || ws.Title != "Build options" ||
		ws.Preview != "Build options trading system" || !ws.HasUnread {
		t.Fatalf("workspace 882CA6F0 fields wrong: %+v", ws)
	}
	// cmux's user-picked color must survive the bridge's reshaping (the app
	// renders it as the card dot); a colorless workspace keeps "" and the
	// wire omits the key entirely (omitempty).
	if ws.CustomColor != "#6A1B9A" {
		t.Fatalf("882CA6F0 custom_color = %q, want #6A1B9A", ws.CustomColor)
	}
	if empty := byID["EMPTY01"]; empty.CustomColor != "" {
		t.Fatalf("EMPTY01 custom_color = %q, want empty", empty.CustomColor)
	}
	if len(ws.Terminals) != 1 {
		t.Fatalf("882CA6F0 want 1 pane, got %d", len(ws.Terminals))
	}
	p := ws.Terminals[0]
	if p.ID != "T1" || !p.Focused || !p.Ready || p.Kind != "agent" || p.Title != "Build options" {
		t.Fatalf("882CA6F0 pane wrong: %+v", p)
	}
	multi := byID["E43BBF04"]
	if len(multi.Terminals) != 2 {
		t.Fatalf("E43BBF04 want 2 panes, got %d", len(multi.Terminals))
	}
	if multi.Terminals[0].Kind != "terminal" || multi.Terminals[1].Kind != "agent" {
		t.Fatalf("E43BBF04 pane kinds wrong: %+v", multi.Terminals)
	}
	if multi.Terminals[1].Focused || multi.Terminals[1].Ready {
		t.Fatalf("E43BBF04 second pane should be unfocused+not-ready: %+v", multi.Terminals[1])
	}
	if empty := byID["EMPTY01"]; len(empty.Terminals) != 0 {
		t.Fatalf("EMPTY01 want 0 panes, got %d", len(empty.Terminals))
	}
}

// TestSessionsCanonicalizesWorkspaceCWD mirrors yolo_test.go's
// TestResolvePendingPermissionMatchesSymlinkedCWD: mobile.workspace.list
// reports a workspace's raw current_directory, which can be a symlink alias
// (e.g. macOS's /tmp -> /private/tmp). /sessions must resolve it to the same
// canonical form /feed/pending uses (see canonicalizeFeedCWDs) so the app's
// cwd-based item-to-workspace matching has both sides in agreement.
func TestSessionsCanonicalizesWorkspaceCWD(t *testing.T) {
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
echo '{"workspaces":[{"id":"WS1","current_directory":"` + alias + `","title":"t","terminals":[]}]}'
`
	s, tok := newTestServer(t, script)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body struct {
		Workspaces []wire.Workspace `json:"workspaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Workspaces) != 1 || body.Workspaces[0].CWD != real {
		t.Fatalf("want cwd resolved to canonical %q, got %+v", real, body.Workspaces)
	}
}

func TestSessionsCarriesHostIdentity(t *testing.T) {
	script := "#!/bin/sh\ncat <<'JSON'\n" + fakeWorkspaceList + "\nJSON\n"
	s, tok := newTestServer(t, script)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body wire.SessionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Host.Kind != "cmux" || !body.Host.Capabilities.Tabs || !body.Host.Capabilities.Feed {
		t.Fatalf("host = %+v, want a cmux host with tabs and feed", body.Host)
	}
	if body.Host.Name == "" || strings.Contains(body.Host.Name, ".") {
		t.Fatalf("host name %q should be the short hostname", body.Host.Name)
	}
}

// pendingCountFixtureItems is mirrored by the app's PendingCountFixtureTest:
// the Inbox lists question and permissionRequest items, so a bridge counting
// these four must say 2 and the app's badge must show 2.
const pendingCountFixtureItems = `{"request_id":"q1","kind":"question","cwd":"/tmp/proj"},` +
	`{"request_id":"p1","kind":"permissionRequest","cwd":"/tmp/proj"},` +
	`{"request_id":"e1","kind":"exitPlan","cwd":"/tmp/proj"},` +
	`{"request_id":"t1","kind":"toolUse","cwd":"/tmp/proj"}`

func sessionsWithFeed(t *testing.T, feedCase string) wire.SessionsResponse {
	t.Helper()
	script := `#!/bin/sh
case "$2" in
  mobile.workspace.list) echo '{"workspaces":[]}' ;;
  feed.list) ` + feedCase + ` ;;
  *) echo '{"ok":true}' ;;
esac
`
	s, tok := newTestServer(t, script)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var body wire.SessionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestSessionsCountsThePromptsTheInboxWouldList(t *testing.T) {
	body := sessionsWithFeed(t, `echo '{"items":[`+pendingCountFixtureItems+`]}'`)
	if body.PendingCount == nil || *body.PendingCount != 2 {
		t.Fatalf("pending_count = %v, want 2 (question + permissionRequest)", body.PendingCount)
	}
}

// A feed the bridge cannot read must not fail the workspace list; the count
// is left out and the app keeps whatever badge it had.
func TestSessionsLeavesThePendingCountOutWhenTheFeedFails(t *testing.T) {
	body := sessionsWithFeed(t, `exit 1`)
	if body.PendingCount != nil {
		t.Fatalf("pending_count = %d after a failed feed read, want absent", *body.PendingCount)
	}
}
