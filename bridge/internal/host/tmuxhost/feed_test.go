package tmuxhost

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sodre90/term-bridge/internal/host"
	"github.com/sodre90/term-bridge/internal/host/agentfeed"
	"github.com/sodre90/term-bridge/internal/wire"
)

const bashPrompt = ` Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and always allow access to /tmp from this project
   3. No
`

func (f fixture) enableFeed(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("", "tb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "hooks.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f.h.EnableFeed(ln)
}

func (f fixture) hook(t *testing.T, socket string, event map[string]any) ([]wire.EventFrame, error) {
	t.Helper()
	raw, _ := json.Marshal(event)
	return f.h.feed.Handle(context.Background(), agentfeed.Envelope{Pane: "%9", Socket: socket, Event: raw})
}

func promptPair() (pre, perm map[string]any) {
	input := map[string]any{"command": "ls /tmp"}
	pre = map[string]any{"hook_event_name": "PreToolUse", "session_id": "s1", "cwd": "/home/sodre90/prj",
		"tool_name": "Bash", "tool_input": input, "tool_use_id": "toolu_01"}
	perm = map[string]any{"hook_event_name": "PermissionRequest", "session_id": "s1", "cwd": "/home/sodre90/prj",
		"tool_name": "Bash", "tool_input": input}
	return pre, perm
}

func TestFeedIsOffUntilEnabled(t *testing.T) {
	f := newFixture(t)
	if f.h.Capabilities().Feed {
		t.Fatal("Feed advertised without a hook socket")
	}
	body, err := f.h.PendingFeed(context.Background())
	if err != nil || string(body) != `{"items":[]}` {
		t.Fatalf("PendingFeed = %s, %v", body, err)
	}
	if err := f.h.FeedReply(context.Background(), "permissionRequest", "x", nil); err != host.ErrUnsupported {
		t.Fatalf("FeedReply = %v", err)
	}
	f.enableFeed(t)
	if !f.h.Capabilities().Feed {
		t.Fatal("Feed not advertised after EnableFeed")
	}
}

func TestHookPromptShowsUpAsItemAttentionAndKeystroke(t *testing.T) {
	f := newFixture(t)
	f.enableFeed(t)
	f.write(t, "locate", row("/tmp/tmux-1000/default", epoch, "@3", "/home/sodre90/prj"))
	f.write(t, "screen", bashPrompt)
	f.write(t, "panes-all",
		row(epoch, "main", "@3", "claude", "1", "%9", "1", "claude", "/home/sodre90/prj", "0", "0", "50", "30"),
		row(epoch, "other", "@5", "bash", "1", "%12", "1", "bash", "/home/sodre90", "0", "0", "100", "30"),
	)
	pre, perm := promptPair()
	if _, err := f.hook(t, "/tmp/tmux-1000/default", pre); err != nil {
		t.Fatal(err)
	}
	frames, err := f.hook(t, "/tmp/tmux-1000/default", perm)
	if err != nil || len(frames) != 1 || frames[0].WorkspaceID != "tmux-"+epoch+"-w3" || frames[0].SurfaceID != "tmux-"+epoch+"-p9" {
		t.Fatalf("frames = %+v, %v", frames, err)
	}

	body, err := f.h.PendingFeed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Items []struct {
			ID, CWD, Kind string
		}
	}
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Items) != 1 || resp.Items[0].ID != "toolu_01" || resp.Items[0].CWD != "/home/sodre90/prj" {
		t.Fatalf("pending = %s (%v)", body, err)
	}

	ws, err := f.h.ListWorkspaces(context.Background())
	if err != nil || len(ws) != 2 {
		t.Fatalf("ListWorkspaces = %+v, %v", ws, err)
	}
	if ws[0].Attention != "permission" || ws[0].Preview != "Claude needs your permission" || ws[0].CWD != resp.Items[0].CWD {
		t.Fatalf("workspace 0 = %+v", ws[0])
	}
	if ws[1].Attention != "" || ws[1].Preview != "" {
		t.Fatalf("workspace 1 = %+v", ws[1])
	}
	f.write(t, "panes-all",
		row(epoch, "main", "@3", "bash", "1", "%9", "1", "bash", "/home/sodre90/prj", "0", "0", "50", "30"),
	)
	ws, err = f.h.ListWorkspaces(context.Background())
	if err != nil || ws[0].Attention != "" {
		t.Fatalf("a pane back at the shell must carry no agent status: %+v, %v", ws, err)
	}

	if err := f.h.FeedReply(context.Background(), "permissionRequest", "toolu_01", map[string]any{"mode": "always"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.log(), "send-keys -t %9 2\n") {
		t.Fatalf("calls:\n%s", f.log())
	}
}

func TestHookFromAnotherServerIsRefused(t *testing.T) {
	f := newFixture(t)
	f.enableFeed(t)
	f.write(t, "locate", row("/tmp/tmux-1000/default", epoch, "@3", "/home/sodre90/prj"))
	pre, _ := promptPair()
	if _, err := f.hook(t, "/tmp/tmux-1000/other", pre); err == nil || !strings.Contains(err.Error(), "belongs to the tmux server") {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.h.feed.Handle(context.Background(), agentfeed.Envelope{Pane: "%9; kill-server", Socket: "/tmp/tmux-1000/default", Event: json.RawMessage(`{"hook_event_name":"Stop"}`)}); err == nil {
		t.Fatal("malformed pane id accepted")
	}
	if strings.Contains(f.log(), "kill-server") {
		t.Fatalf("calls:\n%s", f.log())
	}
}
