package tmuxhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sodre90/cmux-bridge/internal/host"
	"github.com/sodre90/cmux-bridge/internal/testutil"
	"github.com/sodre90/cmux-bridge/internal/tmux"
	"github.com/sodre90/cmux-bridge/internal/wire"
)

const epoch = "1789367814"

// fakeTmux logs every invocation as one line and answers from canned files
// the test writes under $TMUX_FAKE_DIR, chosen by the command line's shape.
const fakeTmux = `#!/bin/sh
printf '%s\n' "$*" >> "$TMUX_FAKE_LOG"
D="$TMUX_FAKE_DIR"
case "$*" in
  "list-panes -a -F "*) cat "$D/panes-all";;
  "list-panes -t @3 -F "*) cat "$D/panes-w3";;
  "list-sessions -F "*) cat "$D/sessions";;
  "display-message -p -t "*"#{start_time}") echo ` + epoch + `;;
  "display-message -p -t "*"#{pane_current_path}") echo /home/sodre90/prj;;
  "display-message -p -t "*"#{window_id}"*"#{window_height}") cat "$D/window-size";;
  "display-message -p -t %9 -F "*"; capture-pane"*) cat "$D/replay";;
  "display-message -p -t %404"*) echo "can't find pane: %404" >&2; exit 1;;
  "new-window "*|"new-session "*) cat "$D/created";;
  "split-window "*) cat "$D/created";;
  "kill-window -t @404") echo "can't find window: @404" >&2; exit 1;;
  "load-buffer "*) cat > "$D/pasted";;
esac
`

type fixture struct {
	h   *Host
	dir string
	log func() string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	t.Setenv("TMUX_FAKE_LOG", logPath)
	t.Setenv("TMUX_FAKE_DIR", dir)
	h := New(&tmux.Client{Bin: testutil.WriteFakeTmux(t, fakeTmux)})
	return fixture{h: h, dir: dir, log: func() string {
		b, _ := os.ReadFile(logPath)
		return string(b)
	}}
}

func (f fixture) write(t *testing.T, name string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func row(fields ...string) string { return strings.Join(fields, fieldSep) }

func TestListWorkspacesGroupsPanesIntoWindows(t *testing.T) {
	f := newFixture(t)
	f.write(t, "panes-all",
		row(epoch, "main", "@3", "claude", "1", "%9", "1", "claude", "/home/sodre90/prj", "0", "0", "50", "30"),
		row(epoch, "main", "@3", "claude", "1", "%10", "0", "bash", "/tmp", "51", "0", "49", "30"),
		row(epoch, "other", "@5", "bash", "1", "%12", "1", "bash", "/home/sodre90", "0", "0", "100", "30"),
	)
	ws, err := f.h.ListWorkspaces(context.Background())
	if err != nil || len(ws) != 2 {
		t.Fatalf("ListWorkspaces = %+v, %v", ws, err)
	}
	if ws[0].ID != "tmux-"+epoch+"-w3" || ws[0].Title != "claude" || ws[0].CWD != "/home/sodre90/prj" || len(ws[0].Terminals) != 2 {
		t.Fatalf("workspace 0 = %+v", ws[0])
	}
	if p := ws[0].Terminals[0]; p.ID != "tmux-"+epoch+"-p9" || p.Kind != "agent" || !p.Focused || p.Title != "claude" {
		t.Fatalf("pane 0 = %+v", p)
	}
	if p := ws[0].Terminals[1]; p.Kind != "terminal" || p.Focused || p.CWD != "/tmp" {
		t.Fatalf("pane 1 = %+v", p)
	}
	if ws[1].ID != "tmux-"+epoch+"-w5" || ws[1].CWD != "/home/sodre90" {
		t.Fatalf("workspace 1 = %+v", ws[1])
	}
}

func TestListWorkspacesWithoutAServerIsEmptyNotAnError(t *testing.T) {
	h := New(&tmux.Client{Bin: testutil.WriteFakeTmux(t, "#!/bin/sh\necho 'no server running on /tmp/tmux-1000/default' >&2\nexit 1\n")})
	ws, err := h.ListWorkspaces(context.Background())
	if err != nil || len(ws) != 0 {
		t.Fatalf("ListWorkspaces = %+v, %v", ws, err)
	}
}

func TestListPanesCarriesCellFrames(t *testing.T) {
	f := newFixture(t)
	f.write(t, "panes-w3",
		row(epoch, "main", "@3", "claude", "1", "%9", "1", "claude", "/p", "0", "0", "50", "30"),
		row(epoch, "main", "@3", "claude", "1", "%10", "0", "bash", "/p", "51", "0", "49", "30"),
	)
	panes, err := f.h.ListPanes(context.Background(), "tmux-"+epoch+"-w3")
	if err != nil || len(panes) != 2 {
		t.Fatalf("ListPanes = %+v, %v", panes, err)
	}
	if panes[1].Frame != (host.Frame{X: 51, Y: 0, Width: 49, Height: 30}) || panes[1].SurfaceIDs[0] != panes[1].ID || panes[1].SelectedSurfaceID != panes[1].ID {
		t.Fatalf("pane 1 = %+v", panes[1])
	}
	if !strings.Contains(f.log(), "list-panes -t @3 -F ") {
		t.Fatalf("calls:\n%s", f.log())
	}
}

func TestStaleAndMalformedIDsAreNotFound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, id := range []string{"tmux-1-w3", "tmux-" + epoch + "-p3", "@3", "nonsense"} {
		_, err := f.h.ListPanes(ctx, id)
		if !host.IsNotFound(err) {
			t.Errorf("ListPanes(%q) = %v, want not-found", id, err)
		}
	}
	if err := f.h.CloseWorkspace(ctx, "tmux-"+epoch+"-w404"); !host.IsNotFound(err) {
		t.Errorf("tmux's own can't-find = %v, want not-found", err)
	}
	if strings.Contains(f.log(), "kill-window -t @3") {
		t.Fatal("a stale id must never reach a mutating command")
	}
}

func TestMutationsNameTheirTargetAndStayDetached(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.write(t, "sessions", row("1789368000", "old"), row("1789368100", "main"))
	f.write(t, "created", row(epoch, "@7", "%20"))
	created, err := f.h.CreateWorkspace(ctx, "/home/sodre90/prj", "work")
	if err != nil || created.WorkspaceID != "tmux-"+epoch+"-w7" || created.SurfaceID != "tmux-"+epoch+"-p20" {
		t.Fatalf("CreateWorkspace = %+v, %v", created, err)
	}
	split, err := f.h.SplitPane(ctx, "tmux-"+epoch+"-p9", wire.PlacementLeft)
	if err != nil || split.SurfaceID != "tmux-"+epoch+"-p20" || split.PaneID != split.SurfaceID {
		t.Fatalf("SplitPane = %+v, %v", split, err)
	}
	if _, err := f.h.CreateTab(ctx, "tmux-"+epoch+"-w3", "tmux-"+epoch+"-p9"); !errors.Is(err, host.ErrUnsupported) {
		t.Fatalf("CreateTab = %v, want ErrUnsupported", err)
	}
	for _, op := range []func() error{
		func() error { return f.h.SelectWorkspace(ctx, "tmux-"+epoch+"-w3") },
		func() error { return f.h.FocusSurface(ctx, "tmux-"+epoch+"-p9") },
		func() error { return f.h.RenameWorkspace(ctx, "tmux-"+epoch+"-w3", "New name") },
		func() error { return f.h.CloseWorkspace(ctx, "tmux-"+epoch+"-w3") },
		func() error { return f.h.CloseSurface(ctx, "tmux-"+epoch+"-p9") },
	} {
		if err := op(); err != nil {
			t.Fatal(err)
		}
	}
	log := f.log()
	for _, want := range []string{
		"new-window -d -t main: -c /home/sodre90/prj -P -F " + createdFormat + " -n work",
		"split-window -d -t %9 -P -F " + createdFormat + " -h -b -c /home/sodre90/prj",
		"select-window -t @3",
		"select-pane -t %9",
		"rename-window -t @3 New name",
		"kill-window -t @3",
		"kill-pane -t %9",
	} {
		if !strings.Contains(log, want+"\n") {
			t.Errorf("missing call %q in:\n%s", want, log)
		}
	}
}

func TestCreateWorkspaceStartsAServerWhenThereIsNone(t *testing.T) {
	f := newFixture(t)
	f.write(t, "sessions")
	f.write(t, "created", row(epoch, "@0", "%0"))
	if _, err := f.h.CreateWorkspace(context.Background(), "/home/sodre90", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.log(), "new-session -d -c /home/sodre90 -P -F "+createdFormat+"\n") {
		t.Fatalf("calls:\n%s", f.log())
	}
}

func TestInputPasteAndResize(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.write(t, "window-size", row("@3", "100", "30"))
	if err := f.h.Input(ctx, "tmux-"+epoch+"-p9", "ls\r\x1b[A"); err != nil {
		t.Fatal(err)
	}
	if err := f.h.Paste(ctx, "tmux-"+epoch+"-p9", "echo hi\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.h.Resize(ctx, "tmux-"+epoch+"-p9", 80, 24); err != nil {
		t.Fatal(err)
	}
	if err := f.h.Resize(ctx, "tmux-"+epoch+"-p9", 100, 30); err != nil {
		t.Fatal(err)
	}
	log := f.log()
	for _, want := range []string{
		"send-keys -t %9 -H 6c 73 0d 1b 5b 41\n",
		"load-buffer -b cmux-bridge-9 -\n",
		"paste-buffer -p -d -b cmux-bridge-9 -t %9\n",
		"resize-window -t @3 -x 80 -y 24\n",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("missing call %q in:\n%s", want, log)
		}
	}
	if strings.Count(log, "resize-window") != 1 {
		t.Fatalf("a resize to the current size must be skipped:\n%s", log)
	}
	if pasted, _ := os.ReadFile(filepath.Join(f.dir, "pasted")); string(pasted) != "echo hi\n" {
		t.Fatalf("pasted = %q", pasted)
	}
}

func TestReplayBuildsAGridFromOneInvocation(t *testing.T) {
	f := newFixture(t)
	f.write(t, "replay",
		row(epoch, "@3", "10", "3", "2", "1", "2", "1", "0", "0", "0", "0", "0", "0", "1"),
		"old line 1",
		"old line 2",
		"\x1b[1mbold\x1b[0m text",
		"",
		"$ ",
	)
	replay, err := f.h.Replay(context.Background(), "tmux-"+epoch+"-p9")
	if err != nil {
		t.Fatal(err)
	}
	if replay.Columns != 10 || replay.Rows != 3 {
		t.Fatalf("replay = %+v", replay)
	}
	grid := string(replay.Grid)
	for _, want := range []string{
		`"format":"cmux.render-grid.v1"`,
		`"scrollback_rows":2`,
		`{"row":0,"column":0,"cell_width":10,"style_id":0,"text":"old line 1"}`,
		`{"row":0,"column":0,"cell_width":4,"style_id":1,"text":"bold"}`,
		`{"row":0,"column":4,"cell_width":5,"style_id":0,"text":" text"}`,
		`{"row":2,"column":0,"cell_width":1,"style_id":0,"text":"$"}`,
		`"cursor":{"row":2,"column":1,"style":"block","visible":true,"blinking":false}`,
		`{"ansi":false,"code":2004,"on":true}`,
	} {
		if !strings.Contains(grid, want) {
			t.Errorf("grid lacks %s:\n%s", want, grid)
		}
	}
	if !strings.Contains(f.log(), `capture-pane -e -p -N -t %9 -S -240 -E -1 ; capture-pane -e -p -N -t %9`) {
		t.Fatalf("calls:\n%s", f.log())
	}
}

func TestReplayDropsTmuxsEchoedFirstRowForAnEmptyHistory(t *testing.T) {
	f := newFixture(t)
	f.write(t, "replay",
		row(epoch, "@3", "10", "3", "0", "0", "0", "1", "0", "0", "0", "0", "0", "0", "0"),
		"$ ls",
		"$ ls",
		"",
		"$ ",
	)
	replay, err := f.h.Replay(context.Background(), "tmux-"+epoch+"-p9")
	if err != nil {
		t.Fatal(err)
	}
	grid := string(replay.Grid)
	for _, want := range []string{
		`"scrollback_rows":0`,
		`"scrollback_spans":[]`,
		`{"row":0,"column":0,"cell_width":4,"style_id":0,"text":"$ ls"}`,
		`{"row":2,"column":0,"cell_width":1,"style_id":0,"text":"$"}`,
	} {
		if !strings.Contains(grid, want) {
			t.Errorf("grid lacks %s:\n%s", want, grid)
		}
	}
}

func TestReplayRefusesAStaleEpochAndAShortCapture(t *testing.T) {
	f := newFixture(t)
	f.write(t, "replay", row("1", "@3", "10", "3", "0", "0", "0", "1", "0", "0", "0", "0", "0", "0", "0"), "a", "b", "c")
	if _, err := f.h.Replay(context.Background(), "tmux-"+epoch+"-p9"); !host.IsNotFound(err) {
		t.Fatalf("stale epoch = %v, want not-found", err)
	}
	f.write(t, "replay", row(epoch, "@3", "10", "3", "0", "0", "0", "1", "0", "0", "0", "0", "0", "0", "0"), "a", "b")
	if _, err := f.h.Replay(context.Background(), "tmux-"+epoch+"-p9"); !errors.Is(err, host.ErrMalformed) {
		t.Fatalf("short capture = %v, want ErrMalformed", err)
	}
	if _, err := f.h.Replay(context.Background(), "tmux-"+epoch+"-p404"); !host.IsNotFound(err) {
		t.Fatalf("missing pane = %v, want not-found", err)
	}
}

func TestWindowSizesReleaseAfterTheLastViewerLeaves(t *testing.T) {
	now := time.Unix(1000, 0)
	var released []string
	w := newWindowSizes(func(id string) { released = append(released, id) }, func() time.Time { return now })
	w.viewed("@3")
	w.touched("@9") // never sized: not tracked
	now = now.Add(3 * time.Second)
	w.touched("@3")
	now = now.Add(3 * time.Second)
	if n := w.sweep(); n != 0 || len(released) != 0 {
		t.Fatalf("a window replayed 3s ago must stay sized: %d %v", n, released)
	}
	now = now.Add(3 * time.Second)
	if n := w.sweep(); n != 1 || len(released) != 1 || released[0] != "@3" {
		t.Fatalf("sweep = %d, released %v", n, released)
	}
	if n := w.sweep(); n != 0 {
		t.Fatal("a released window is forgotten")
	}
}

func TestRunEventsCoalescesStructuralNotificationsIntoLayoutFrames(t *testing.T) {
	// The fake answers list-sessions, then plays a burst as the control
	// client and holds until stdin closes.
	script := `#!/bin/sh
case "$1" in
  list-sessions) printf '1\037main\n';;
  -C) printf '%%begin 1 1 0\n%%end 1 1 0\n%%session-changed $0 main\n%%window-add @6\n%%window-renamed @6 x\n%%layout-change @6 l l\n%%output %%1 no\n'; cat >/dev/null;;
esac
`
	h := New(&tmux.Client{Bin: testutil.WriteFakeTmux(t, script)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := make(chan wire.EventFrame, 10)
	go h.RunEvents(ctx, func(f wire.EventFrame) { frames <- f })
	select {
	case f := <-frames:
		if f.Type != EventTypeLayout {
			t.Fatalf("frame = %+v", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no layout frame")
	}
	select {
	case f := <-frames:
		t.Fatalf("burst must coalesce into one frame, got a second: %+v", f)
	case <-time.After(300 * time.Millisecond):
	}
}
