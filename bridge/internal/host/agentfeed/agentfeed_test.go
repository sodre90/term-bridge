package agentfeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sodre90/term-bridge/internal/host"
	"github.com/sodre90/term-bridge/internal/wire"
)

// Screens as Claude Code 2.1.263 drew them in the 2026-09-14 probe.
const (
	bashPrompt = `● Bash(ls /tmp)

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and always allow access to /tmp from this project
   3. Yes, and switch to auto mode for the rest of this session
   4. No

 Esc to cancel`
	writePrompt = `● Write(/home/sodre90/tb-probe/hello.txt)

 Do you want to create hello.txt?
 ❯ 1. Yes
   2. Yes, and switch to accept edits for the rest of this session (shift+tab)
   3. No`
	questionPrompt = ` Pick a colour
 ❯ 1. Red
      The warm one
   2. Blue
      The cool one
   3. Type something.
   4. Chat about this`
	idleScreen = `● pong

> `
)

const ownSocket = "/tmp/tmux-1000/default"

type fakePanes struct {
	mu      sync.Mutex
	screens map[string]string
	keys    []string
	located int
}

func newFakePanes() *fakePanes { return &fakePanes{screens: map[string]string{}} }

func (p *fakePanes) Locate(_ context.Context, socket, pane string) (Location, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.located++
	if socket != ownSocket {
		return Location{}, errors.New("foreign tmux server")
	}
	n := strings.TrimPrefix(pane, "%")
	return Location{WorkspaceID: "tmux-1-w" + n, SurfaceID: "tmux-1-p" + n, CWD: "/home/sodre90/tb-probe"}, nil
}

func (p *fakePanes) Capture(_ context.Context, pane string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.screens[pane], nil
}

func (p *fakePanes) SendKeys(_ context.Context, pane, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = append(p.keys, pane+":"+key)
	return nil
}

func (p *fakePanes) show(pane, screen string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.screens[pane] = screen
}

func (p *fakePanes) typed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.keys...)
}

type fixture struct {
	f     *Feed
	panes *fakePanes
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	panes := newFakePanes()
	f := New(panes)
	f.replyWait, f.replyPoll = 50*time.Millisecond, 5*time.Millisecond
	return fixture{f: f, panes: panes}
}

func (fx fixture) hook(t *testing.T, pane string, event map[string]any) []wire.EventFrame {
	t.Helper()
	frames, err := fx.hookErr(pane, ownSocket, event)
	if err != nil {
		t.Fatalf("hook %v: %v", event["hook_event_name"], err)
	}
	return frames
}

func (fx fixture) hookErr(pane, socket string, event map[string]any) ([]wire.EventFrame, error) {
	raw, _ := json.Marshal(event)
	return fx.f.Handle(context.Background(), Envelope{Pane: pane, Socket: socket, Event: raw})
}

func bashCall(toolUseID string) (pre, perm map[string]any) {
	input := map[string]any{"command": "ls /tmp", "description": "List tmp"}
	pre = map[string]any{"hook_event_name": "PreToolUse", "session_id": "s1", "cwd": "/home/sodre90/tb-probe",
		"tool_name": "Bash", "tool_input": input, "tool_use_id": toolUseID}
	perm = map[string]any{"hook_event_name": "PermissionRequest", "session_id": "s1", "cwd": "/home/sodre90/tb-probe",
		"tool_name": "Bash", "tool_input": input, "permission_suggestions": []any{}}
	return pre, perm
}

func questionCall(toolUseID string, multi bool) (pre, perm map[string]any) {
	input := map[string]any{"questions": []any{map[string]any{
		"question": "Pick a colour", "header": "Colour", "multiSelect": multi,
		"options": []any{
			map[string]any{"label": "Red", "description": "The warm one"},
			map[string]any{"label": "Blue", "description": "The cool one"},
		},
	}}}
	pre = map[string]any{"hook_event_name": "PreToolUse", "session_id": "s1", "cwd": "/home/sodre90/tb-probe",
		"tool_name": "AskUserQuestion", "tool_input": input, "tool_use_id": toolUseID}
	perm = map[string]any{"hook_event_name": "PermissionRequest", "session_id": "s1", "cwd": "/home/sodre90/tb-probe",
		"tool_name": "AskUserQuestion", "tool_input": input}
	return pre, perm
}

func (fx fixture) pending(t *testing.T) []wireItem {
	t.Helper()
	raw, err := fx.f.PendingFeed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Items []wireItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("pending body %s: %v", raw, err)
	}
	return resp.Items
}

func TestPermissionRequestBecomesItemKeyedByPreToolUse(t *testing.T) {
	fx := newFixture(t)
	fx.panes.show("%16", bashPrompt)
	pre, perm := bashCall("toolu_01")
	if frames := fx.hook(t, "%16", pre); len(frames) != 0 {
		t.Fatalf("PreToolUse produced frames: %+v", frames)
	}
	frames := fx.hook(t, "%16", perm)
	if len(frames) != 1 || !frames[0].NeedsAttention || frames[0].Kind != "PermissionRequest" ||
		frames[0].WorkspaceID != "tmux-1-w16" || frames[0].SurfaceID != "tmux-1-p16" || frames[0].FeedID != "toolu_01" ||
		frames[0].Title != "tb-probe" {
		t.Fatalf("frames = %+v", frames)
	}
	items := fx.pending(t)
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	it := items[0]
	if it.ID != "toolu_01" || it.RequestID != "toolu_01" || it.Kind != "permissionRequest" || it.Status != "pending" ||
		it.ToolName != "Bash" || it.CWD != "/home/sodre90/tb-probe" || it.WorkstreamID != "s1" || it.Title != "tb-probe" {
		t.Fatalf("item = %+v", it)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(it.ToolInput), &args); err != nil || args["command"] != "ls /tmp" {
		t.Fatalf("tool_input should be a JSON string holding the tool's JSON, got %q (%v)", it.ToolInput, err)
	}
	if !strings.Contains(string(mustJSON(t, fx.f)), `"tool_input":"{`) {
		t.Fatalf("tool_input must be string-encoded on the wire: %s", mustJSON(t, fx.f))
	}
}

func mustJSON(t *testing.T, f *Feed) json.RawMessage {
	t.Helper()
	raw, err := f.PendingFeed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPermissionRequestWithoutRecordGetsSyntheticID(t *testing.T) {
	fx := newFixture(t)
	fx.panes.show("%16", bashPrompt)
	_, perm := bashCall("")
	frames := fx.hook(t, "%16", perm)
	if len(frames) != 1 || !strings.HasPrefix(frames[0].FeedID, "prompt-16-") {
		t.Fatalf("frames = %+v", frames)
	}
	items := fx.pending(t)
	if len(items) != 1 || items[0].ID != frames[0].FeedID || items[0].ToolName != "Bash" {
		t.Fatalf("items = %+v", items)
	}
}

func TestPermissionPromptNotificationOnlyFillsAMissedRequest(t *testing.T) {
	fx := newFixture(t)
	fx.panes.show("%16", bashPrompt)
	note := map[string]any{"hook_event_name": "Notification", "session_id": "s1", "cwd": "/home/sodre90/tb-probe",
		"notification_type": "permission_prompt", "message": "Claude needs your permission"}

	frames := fx.hook(t, "%16", note)
	if len(frames) != 1 || !frames[0].NeedsAttention || frames[0].Kind != "Notification" {
		t.Fatalf("missed request should raise attention: %+v", frames)
	}
	if items := fx.pending(t); len(items) != 1 || items[0].Kind != "permissionRequest" || items[0].ToolName != "" {
		t.Fatalf("items = %+v", items)
	}

	fx = newFixture(t)
	fx.panes.show("%16", bashPrompt)
	pre, perm := bashCall("toolu_01")
	fx.hook(t, "%16", pre)
	fx.hook(t, "%16", perm)
	if frames := fx.hook(t, "%16", note); len(frames) != 0 {
		t.Fatalf("the prompt was already announced; no second alert wanted: %+v", frames)
	}
}

func TestQuestionItemCarriesStructuredQuestions(t *testing.T) {
	fx := newFixture(t)
	fx.panes.show("%16", questionPrompt)
	pre, perm := questionCall("toolu_q", false)
	fx.hook(t, "%16", pre)
	frames := fx.hook(t, "%16", perm)
	if len(frames) != 1 || frames[0].Kind != "AskUserQuestion" {
		t.Fatalf("frames = %+v", frames)
	}
	items := fx.pending(t)
	if len(items) != 1 || items[0].Kind != "question" || items[0].QuestionPrompt != "Pick a colour" || items[0].QuestionMultiSelect {
		t.Fatalf("items = %+v", items)
	}
	q := items[0].Questions
	if len(q) != 1 || q[0].Header != "Colour" || q[0].Prompt != "Pick a colour" || len(q[0].Options) != 2 ||
		q[0].Options[1].Label != "Blue" || q[0].Options[1].Description != "The cool one" || q[0].Options[1].ID == "" {
		t.Fatalf("questions = %+v", q)
	}
}

func TestReplyTypesTheDigitOfTheMatchingOption(t *testing.T) {
	cases := []struct {
		name, screen, mode, want string
	}{
		{"once", bashPrompt, "once", "1"},
		{"always bash", bashPrompt, "always", "2"},
		{"all is always", bashPrompt, "all", "2"},
		{"bypass is always", bashPrompt, "bypass", "2"},
		{"deny bash", bashPrompt, "deny", "4"},
		{"always write takes accept edits", writePrompt, "always", "2"},
		{"deny write", writePrompt, "deny", "3"},
		{"deny without a No option presses Esc", " ❯ 1. Yes\n   2. Yes, and always", "deny", "Escape"},
		{"always without an always option approves once", " ❯ 1. Yes\n   2. No", "always", "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.panes.show("%16", tc.screen)
			pre, perm := bashCall("toolu_01")
			fx.hook(t, "%16", pre)
			fx.hook(t, "%16", perm)
			err := fx.f.FeedReply(context.Background(), "permissionRequest", "toolu_01", map[string]any{"mode": tc.mode})
			if err != nil {
				t.Fatal(err)
			}
			if got := fx.panes.typed(); len(got) != 1 || got[0] != "%16:"+tc.want {
				t.Fatalf("typed %v, want %s", got, tc.want)
			}
			if items := fx.pending(t); len(items) != 0 {
				t.Fatalf("answered item still pending: %+v", items)
			}
		})
	}
}

func TestReplyRefusesWhenThePromptIsGone(t *testing.T) {
	fx := newFixture(t)
	fx.panes.show("%16", bashPrompt)
	pre, perm := bashCall("toolu_01")
	fx.hook(t, "%16", pre)
	fx.hook(t, "%16", perm)
	fx.panes.show("%16", idleScreen)

	err := fx.f.FeedReply(context.Background(), "permissionRequest", "toolu_01", map[string]any{"mode": "once"})
	if !errors.Is(err, host.ErrPromptGone) {
		t.Fatalf("err = %v, want ErrPromptGone", err)
	}
	if got := fx.panes.typed(); len(got) != 0 {
		t.Fatalf("typed into a pane with no prompt: %v", got)
	}
	if err := fx.f.FeedReply(context.Background(), "permissionRequest", "never-seen", map[string]any{"mode": "once"}); !errors.Is(err, host.ErrPromptGone) {
		t.Fatalf("unknown request: err = %v", err)
	}
}

func TestReplyWaitsForThePromptToAppear(t *testing.T) {
	fx := newFixture(t)
	pre, perm := bashCall("toolu_01")
	fx.hook(t, "%16", pre)
	fx.hook(t, "%16", perm)
	go func() {
		time.Sleep(15 * time.Millisecond)
		fx.panes.show("%16", bashPrompt)
	}()
	if err := fx.f.FeedReply(context.Background(), "permissionRequest", "toolu_01", map[string]any{"mode": "always"}); err != nil {
		t.Fatal(err)
	}
	if got := fx.panes.typed(); len(got) != 1 || got[0] != "%16:2" {
		t.Fatalf("typed %v", got)
	}
}

func TestReplyRefusesAPromptThatChangedUnderneath(t *testing.T) {
	fx := newFixture(t)
	fx.panes.show("%16", bashPrompt)
	pre, perm := bashCall("toolu_01")
	fx.hook(t, "%16", pre)
	fx.hook(t, "%16", perm)
	fx.panes.show("%16", questionPrompt)
	err := fx.f.FeedReply(context.Background(), "permissionRequest", "toolu_01", map[string]any{"mode": "once"})
	if !errors.Is(err, host.ErrPromptGone) || len(fx.panes.typed()) != 0 {
		t.Fatalf("err = %v, typed %v", err, fx.panes.typed())
	}
}

func TestQuestionReplyTypesTheSelectedLabel(t *testing.T) {
	fx := newFixture(t)
	fx.panes.show("%16", questionPrompt)
	pre, perm := questionCall("toolu_q", false)
	fx.hook(t, "%16", pre)
	fx.hook(t, "%16", perm)
	err := fx.f.FeedReply(context.Background(), "question", "toolu_q", map[string]any{"selections": []any{"Blue"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := fx.panes.typed(); len(got) != 1 || got[0] != "%16:2" {
		t.Fatalf("typed %v", got)
	}
}

func TestQuestionReplyLimits(t *testing.T) {
	fx := newFixture(t)
	fx.panes.show("%16", questionPrompt)
	pre, perm := questionCall("toolu_q", true)
	fx.hook(t, "%16", pre)
	fx.hook(t, "%16", perm)
	err := fx.f.FeedReply(context.Background(), "question", "toolu_q", map[string]any{"selections": []any{"Blue"}})
	if !errors.Is(err, host.ErrUnsupported) {
		t.Fatalf("multi-select: err = %v", err)
	}

	fx = newFixture(t)
	fx.panes.show("%16", questionPrompt)
	pre, perm = questionCall("toolu_q", false)
	fx.hook(t, "%16", pre)
	fx.hook(t, "%16", perm)
	err = fx.f.FeedReply(context.Background(), "question", "toolu_q", map[string]any{"selections": []any{"Red", "Blue"}})
	if !errors.Is(err, host.ErrUnsupported) {
		t.Fatalf("two selections: err = %v", err)
	}
	err = fx.f.FeedReply(context.Background(), "question", "toolu_q", map[string]any{"selections": []any{"Green"}})
	if !errors.Is(err, host.ErrPromptGone) {
		t.Fatalf("label not on screen: err = %v", err)
	}
	if len(fx.panes.typed()) != 0 {
		t.Fatalf("typed %v", fx.panes.typed())
	}
}

func TestItemClearsWhenTheHumanAnswersInTheTerminal(t *testing.T) {
	t.Run("PostToolUse", func(t *testing.T) {
		fx := newFixture(t)
		fx.panes.show("%16", bashPrompt)
		pre, perm := bashCall("toolu_01")
		fx.hook(t, "%16", pre)
		fx.hook(t, "%16", perm)
		fx.hook(t, "%16", map[string]any{"hook_event_name": "PostToolUse", "session_id": "s1", "tool_name": "Bash", "tool_use_id": "toolu_01"})
		if items := fx.pending(t); len(items) != 0 {
			t.Fatalf("items = %+v", items)
		}
	})
	t.Run("Stop", func(t *testing.T) {
		fx := newFixture(t)
		fx.panes.show("%16", bashPrompt)
		pre, perm := bashCall("toolu_01")
		fx.hook(t, "%16", pre)
		fx.hook(t, "%16", perm)
		frames := fx.hook(t, "%16", map[string]any{"hook_event_name": "Stop", "session_id": "s1", "last_assistant_message": "pong"})
		if len(frames) != 1 || frames[0].NeedsAttention || frames[0].Type != "notification" || frames[0].WorkspaceID != "tmux-1-w16" {
			t.Fatalf("Stop should refresh the list without paging: %+v", frames)
		}
		if items := fx.pending(t); len(items) != 0 {
			t.Fatalf("items = %+v", items)
		}
	})
	t.Run("prompt left the screen", func(t *testing.T) {
		fx := newFixture(t)
		fx.panes.show("%16", bashPrompt)
		pre, perm := bashCall("toolu_01")
		fx.hook(t, "%16", pre)
		fx.hook(t, "%16", perm)
		fx.panes.show("%16", idleScreen)
		if items := fx.pending(t); len(items) != 0 {
			t.Fatalf("items = %+v", items)
		}
		if items := fx.pending(t); len(items) != 0 {
			t.Fatalf("pruned item came back: %+v", items)
		}
	})
	t.Run("UserPromptSubmit", func(t *testing.T) {
		fx := newFixture(t)
		fx.panes.show("%16", bashPrompt)
		pre, perm := bashCall("toolu_01")
		fx.hook(t, "%16", pre)
		fx.hook(t, "%16", perm)
		fx.hook(t, "%16", map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "s1"})
		if items := fx.pending(t); len(items) != 0 {
			t.Fatalf("items = %+v", items)
		}
	})
}

func TestStatusesFollowThePane(t *testing.T) {
	fx := newFixture(t)
	fx.panes.show("%16", bashPrompt)
	if got := fx.f.Statuses(); len(got) != 0 {
		t.Fatalf("statuses before any hook: %+v", got)
	}
	pre, perm := bashCall("toolu_01")
	fx.hook(t, "%16", pre)
	fx.hook(t, "%16", perm)
	if got := fx.f.Statuses()["tmux-1-p16"]; got.Attention != "permission" || got.Preview != "Claude needs your permission" {
		t.Fatalf("pending: %+v", got)
	}
	fx.hook(t, "%16", map[string]any{"hook_event_name": "Stop", "session_id": "s1", "last_assistant_message": "  done\n\nnext?  "})
	if got := fx.f.Statuses()["tmux-1-p16"]; got.Attention != "input" || got.Preview != "done next?" {
		t.Fatalf("waiting: %+v", got)
	}
	fx.hook(t, "%16", map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "s1"})
	if got := fx.f.Statuses(); len(got) != 0 {
		t.Fatalf("after a new prompt: %+v", got)
	}
	fx.hook(t, "%16", pre)
	fx.hook(t, "%16", perm)
	fx.hook(t, "%16", map[string]any{"hook_event_name": "SessionEnd", "session_id": "s1"})
	if got := fx.f.Statuses(); len(got) != 0 {
		t.Fatalf("after SessionEnd: %+v", got)
	}
}

func TestIdlePromptNotificationRaisesAttention(t *testing.T) {
	fx := newFixture(t)
	frames := fx.hook(t, "%16", map[string]any{"hook_event_name": "Notification", "session_id": "s1", "cwd": "/home/sodre90/tb-probe",
		"notification_type": "idle_prompt", "message": "Claude is waiting for your input"})
	if len(frames) != 1 || !frames[0].NeedsAttention || frames[0].Kind != "Notification" || frames[0].WorkspaceID != "tmux-1-w16" {
		t.Fatalf("frames = %+v", frames)
	}
	if got := fx.f.Statuses()["tmux-1-p16"]; got.Attention != "input" {
		t.Fatalf("status = %+v", got)
	}
}

func TestHooksFromAnotherTmuxServerAreDropped(t *testing.T) {
	fx := newFixture(t)
	fx.panes.show("%16", bashPrompt)
	pre, perm := bashCall("toolu_01")
	if _, err := fx.hookErr("%16", "/tmp/tmux-1000/other", pre); err == nil {
		t.Fatal("foreign PreToolUse accepted")
	}
	if _, err := fx.hookErr("%16", "/tmp/tmux-1000/other", perm); err == nil {
		t.Fatal("foreign PermissionRequest accepted")
	}
	if items := fx.pending(t); len(items) != 0 {
		t.Fatalf("items = %+v", items)
	}
}

func TestParallelToolCallsMatchByInput(t *testing.T) {
	fx := newFixture(t)
	fx.panes.show("%16", bashPrompt)
	preA, _ := bashCall("toolu_a")
	preB := map[string]any{"hook_event_name": "PreToolUse", "session_id": "s1", "cwd": "/home/sodre90/tb-probe",
		"tool_name": "Bash", "tool_input": map[string]any{"command": "rm -rf build"}, "tool_use_id": "toolu_b"}
	permB := map[string]any{"hook_event_name": "PermissionRequest", "session_id": "s1", "cwd": "/home/sodre90/tb-probe",
		"tool_name": "Bash", "tool_input": map[string]any{"command": "rm -rf build"}}
	fx.hook(t, "%16", preA)
	fx.hook(t, "%16", preB)
	frames := fx.hook(t, "%16", permB)
	if len(frames) != 1 || frames[0].FeedID != "toolu_b" {
		t.Fatalf("frames = %+v", frames)
	}
}

func TestOptionsOnScreen(t *testing.T) {
	cases := []struct {
		name   string
		screen string
		want   []string
	}{
		{"bash", bashPrompt, []string{"Yes", "Yes, and always allow access to /tmp from this project", "Yes, and switch to auto mode for the rest of this session", "No"}},
		{"question with descriptions", questionPrompt, []string{"Red", "Blue", "Type something.", "Chat about this"}},
		{"idle", idleScreen, nil},
		{"prose list above the prompt", "Steps:\n 1. build\n 2. test\n 3. ship\n\n Do you want to proceed?\n ❯ 1. Yes\n   2. No", []string{"Yes", "No"}},
		{"prose list only", "Steps:\n 1. build\n 2. test", []string{"build", "test"}},
		{"gap breaks the run", " 1. Yes\n 3. No", []string{"Yes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var labels []string
			for _, o := range optionsOnScreen(tc.screen) {
				labels = append(labels, o.label)
			}
			if fmt.Sprint(labels) != fmt.Sprint(tc.want) {
				t.Fatalf("labels = %q, want %q", labels, tc.want)
			}
		})
	}
}

func TestSocketRoundTrip(t *testing.T) {
	dir, err := os.MkdirTemp("", "af")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "run", "hooks.sock")
	if err := os.WriteFile(filepath.Join(dir, "stale"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	fx := newFixture(t)
	fx.panes.show("%16", bashPrompt)
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode: %v %v", info, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var frames []wire.EventFrame
	go fx.f.Serve(ctx, ln, func(f wire.EventFrame) {
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
	})

	pre, perm := bashCall("toolu_01")
	for _, ev := range []map[string]any{pre, perm} {
		raw, _ := json.Marshal(ev)
		if err := Forward(path, Envelope{Pane: "%16", Socket: ownSocket, Event: raw}); err != nil {
			t.Fatal(err)
		}
	}
	items := fx.pending(t)
	if len(items) != 1 || items[0].ID != "toolu_01" {
		t.Fatalf("items = %+v", items)
	}
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		n := len(frames)
		mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			if n != 1 {
				t.Fatalf("frames = %d", n)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := Listen(path); err != nil {
		t.Fatalf("relisten over a live socket: %v", err)
	}
}

func TestForwardWithoutAListenerFails(t *testing.T) {
	err := Forward(filepath.Join(t.TempDir(), "none.sock"), Envelope{Pane: "%1"})
	if err == nil {
		t.Fatal("expected a dial error")
	}
}
