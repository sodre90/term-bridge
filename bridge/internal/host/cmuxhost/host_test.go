package cmuxhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sodre90/cmux-bridge/internal/cmux"
	"github.com/sodre90/cmux-bridge/internal/host"
	"github.com/sodre90/cmux-bridge/internal/testutil"
)

// fakeCmux answers each `cmux rpc <method> [json]` from a case table and
// logs "<method> <json>" per call so a test can assert exactly what cmux was
// told.
const fakeCmux = `#!/bin/sh
printf '%s %s\n' "$2" "$3" >> "$CMUX_FAKE_LOG"
case "$2" in
  mobile.workspace.list) echo '{"workspaces":[{"id":"W1","current_directory":"/tmp","preview":"Claude needs your permission","title":"✳ Fix it","terminals":[{"id":"S1","title":"~/x","is_focused":true}]}]}' ;;
  pane.list) echo '{"panes":[{"id":"P1","focused":true,"surface_ids":["S1"],"selected_surface_id":"S1","pixel_frame":{"x":248,"y":28,"width":100,"height":50}}]}' ;;
  workspace.create) echo '{"workspace_id":"W2","surface_id":"S9"}' ;;
  surface.split) echo '{"surface_id":"S3","pane_id":"P3","workspace_id":"W1"}' ;;
  surface.create) echo '{"surface_id":"S4","pane_id":"P1","workspace_id":"W1"}' ;;
  mobile.terminal.replay) echo '{"columns":80,"rows":24,"seq":7,"render_grid":{"format":"cmux.render-grid.v1"}}' ;;
  feed.list) echo '{"items":[{"request_id":"R1","kind":"question","cwd":"/tmp"}]}' ;;
  broken) echo 'not json' ;;
  *) echo '{"ok":true}' ;;
esac
`

func newFakeHost(t *testing.T) (*Host, func() string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("CMUX_FAKE_LOG", logPath)
	h := New(&cmux.Client{Bin: testutil.WriteFakeCmux(t, fakeCmux)})
	return h, func() string {
		b, _ := os.ReadFile(logPath)
		return string(b)
	}
}

func TestListWorkspacesNormalisesCmuxShape(t *testing.T) {
	h, _ := newFakeHost(t)
	ws, err := h.ListWorkspaces(context.Background())
	if err != nil || len(ws) != 1 {
		t.Fatalf("ListWorkspaces = %+v, %v", ws, err)
	}
	got := ws[0]
	if got.ID != "W1" || got.Title != "Fix it" || got.Attention != "permission" || len(got.Terminals) != 1 || got.Terminals[0].Kind != "terminal" {
		t.Fatalf("workspace not normalised: %+v", got)
	}
}

func TestListPanesCarriesPixelFrames(t *testing.T) {
	h, calls := newFakeHost(t)
	panes, err := h.ListPanes(context.Background(), "W1")
	if err != nil || len(panes) != 1 {
		t.Fatalf("ListPanes = %+v, %v", panes, err)
	}
	if panes[0].Frame != (host.Frame{X: 248, Y: 28, Width: 100, Height: 50}) || panes[0].SelectedSurfaceID != "S1" {
		t.Fatalf("pane = %+v", panes[0])
	}
	if !strings.Contains(calls(), `pane.list {"workspace_id":"W1"}`) {
		t.Fatalf("calls:\n%s", calls())
	}
}

func TestMutationsNameTheirTargetAndNeverFocus(t *testing.T) {
	h, calls := newFakeHost(t)
	ctx := context.Background()
	if created, err := h.CreateWorkspace(ctx, "/tmp", "T"); err != nil || created.WorkspaceID != "W2" {
		t.Fatalf("CreateWorkspace = %+v, %v", created, err)
	}
	if created, err := h.SplitPane(ctx, "S1", "right"); err != nil || created.SurfaceID != "S3" {
		t.Fatalf("SplitPane = %+v, %v", created, err)
	}
	if created, err := h.CreateTab(ctx, "W1", "P1"); err != nil || created.SurfaceID != "S4" {
		t.Fatalf("CreateTab = %+v, %v", created, err)
	}
	for _, op := range []func() error{
		func() error { return h.SelectWorkspace(ctx, "W1") },
		func() error { return h.FocusSurface(ctx, "S1") },
		func() error { return h.RenameWorkspace(ctx, "W1", "New") },
		func() error { return h.CloseWorkspace(ctx, "W1") },
		func() error { return h.CloseSurface(ctx, "S1") },
	} {
		if err := op(); err != nil {
			t.Fatal(err)
		}
	}
	log := calls()
	for _, want := range []string{
		`workspace.create {"cwd":"/tmp","focus":false,"title":"T"}`,
		`surface.split {"direction":"right","focus":false,"surface_id":"S1"}`,
		`surface.create {"focus":false,"pane_id":"P1","type":"terminal","workspace_id":"W1"}`,
		`workspace.select {"workspace_id":"W1"}`,
		`surface.focus {"surface_id":"S1"}`,
		`workspace.rename {"title":"New","workspace_id":"W1"}`,
		`workspace.close {"workspace_id":"W1"}`,
		`surface.close {"surface_id":"S1"}`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("missing call %q in:\n%s", want, log)
		}
	}
}

func TestTerminalCalls(t *testing.T) {
	h, calls := newFakeHost(t)
	ctx := context.Background()
	replay, err := h.Replay(ctx, "S1")
	if err != nil || replay.Columns != 80 || replay.Rows != 24 || replay.Seq != 7 || !strings.Contains(string(replay.Grid), "render-grid") {
		t.Fatalf("Replay = %+v, %v", replay, err)
	}
	_ = h.Input(ctx, "S1", "ls")
	_ = h.Paste(ctx, "S1", "echo hi")
	_ = h.Resize(ctx, "S1", 100, 40)
	log := calls()
	for _, want := range []string{
		`mobile.terminal.input {"surface_id":"S1","text":"ls"}`,
		`mobile.terminal.paste {"surface_id":"S1","text":"echo hi"}`,
		`mobile.terminal.viewport {"columns":100,"rows":40,"surface_id":"S1"}`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("missing call %q in:\n%s", want, log)
		}
	}
}

func TestFeedReplyRoutesByKindAndInjectsRequestID(t *testing.T) {
	h, calls := newFakeHost(t)
	ctx := context.Background()
	if err := h.FeedReply(ctx, "permissionRequest", "R1", map[string]any{"mode": "once"}); err != nil {
		t.Fatal(err)
	}
	if err := h.FeedReply(ctx, "question", "R2", map[string]any{"selections": []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.FeedReply(ctx, "exitPlan", "R3", nil); err != nil {
		t.Fatal(err)
	}
	err := h.FeedReply(ctx, "bogus", "R4", nil)
	if !errors.Is(err, host.ErrUnsupported) {
		t.Fatalf("unknown kind err = %v, want ErrUnsupported", err)
	}
	log := calls()
	for _, want := range []string{
		`feed.permission.reply {"mode":"once","request_id":"R1"}`,
		`feed.question.reply {"request_id":"R2","selections":["a"]}`,
		`feed.exit_plan.reply {"request_id":"R3"}`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("missing call %q in:\n%s", want, log)
		}
	}
	if strings.Contains(log, "bogus") {
		t.Fatal("an unknown kind must not reach cmux")
	}
}

func TestPendingFeedPassesItemsThrough(t *testing.T) {
	h, _ := newFakeHost(t)
	body, err := h.PendingFeed(context.Background())
	if err != nil || !strings.Contains(string(body), `"request_id":"R1"`) {
		t.Fatalf("PendingFeed = %s, %v", body, err)
	}
}

func TestMalformedReplyIsErrMalformed(t *testing.T) {
	h := New(&cmux.Client{Bin: testutil.WriteFakeCmux(t, "#!/bin/sh\necho 'not json'\n")})
	if _, err := h.ListWorkspaces(context.Background()); !errors.Is(err, host.ErrMalformed) {
		t.Fatalf("ListWorkspaces err = %v, want ErrMalformed", err)
	}
	if _, err := h.Replay(context.Background(), "S1"); !errors.Is(err, host.ErrMalformed) {
		t.Fatalf("Replay err = %v, want ErrMalformed", err)
	}
}

func TestValidIDIsAUUID(t *testing.T) {
	h := New(&cmux.Client{})
	if !h.ValidID("0B1D3CA1-7E2A-4E7B-9E2F-0A1B2C3D4E5F") || h.ValidID("W1") || h.ValidID("") {
		t.Fatal("ValidID must accept exactly cmux UUIDs")
	}
}

func TestNotFoundSurvivesTheHostSeam(t *testing.T) {
	err := &cmux.RPCError{Method: "mobile.terminal.replay", Code: cmux.CodeNotFound, Message: "gone"}
	if !host.IsNotFound(err) {
		t.Fatal("a cmux not_found must be recognised through host.IsNotFound")
	}
	if host.IsNotFound(&cmux.RPCError{Code: "internal"}) {
		t.Fatal("other cmux refusals are not not-found")
	}
}
