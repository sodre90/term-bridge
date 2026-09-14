// Package agentfeed turns Claude Code's hooks into the host seam's feed on a
// tmux host. `term-bridge hook`, installed as a command hook, forwards each
// hook's stdin JSON plus the pane it ran in over a unix socket that the
// agent owns; this package keeps one record per pane of what Claude is
// asking, serves it as pending feed items, and answers a reply by typing
// the digit of the matching option into the pane -- the prompt stays Claude
// Code's own, drawn in the terminal exactly as if no hook existed, and the
// phone mirrors it.
//
// Nothing here is logged beyond event names and pane ids: tool_input and
// the agent's last message are the user's content.
package agentfeed

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sodre90/term-bridge/internal/host"
	"github.com/sodre90/term-bridge/internal/wire"
)

// Envelope is one forwarded hook invocation: Claude Code's stdin JSON and
// the tmux pane and server socket from the hook's environment.
type Envelope struct {
	Pane   string          `json:"pane"`
	Socket string          `json:"socket"`
	Event  json.RawMessage `json:"event"`
}

// Location is where a tmux pane sits in wire terms.
type Location struct {
	WorkspaceID string
	SurfaceID   string
	CWD         string
}

// Panes is what the feed needs from tmux, supplied by tmuxhost so this
// package never spells a tmux command.
type Panes interface {
	// Locate maps a pane of the server at socket to wire ids; a pane of
	// some other tmux server must be refused with an error.
	Locate(ctx context.Context, socket, pane string) (Location, error)
	// Capture returns the pane's visible screen as text, wrapped lines
	// joined.
	Capture(ctx context.Context, pane string) (string, error)
	// SendKeys types one tmux key name ("1", "Escape") into the pane.
	SendKeys(ctx context.Context, pane, key string) error
}

// Hook events this package acts on; anything else is ignored.
const (
	eventPreToolUse        = "PreToolUse"
	eventPermissionRequest = "PermissionRequest"
	eventPostToolUse       = "PostToolUse"
	eventNotification      = "Notification"
	eventStop              = "Stop"
	eventUserPromptSubmit  = "UserPromptSubmit"
	eventSessionEnd        = "SessionEnd"

	notificationPermissionPrompt = "permission_prompt"
	notificationIdlePrompt       = "idle_prompt"

	toolAskUserQuestion = "AskUserQuestion"
)

// Events lists the hook events `term-bridge hook install` subscribes to.
var Events = []string{
	eventPreToolUse, eventPermissionRequest, eventPostToolUse,
	eventNotification, eventStop, eventUserPromptSubmit, eventSessionEnd,
}

type hookEvent struct {
	Name                 string          `json:"hook_event_name"`
	SessionID            string          `json:"session_id"`
	CWD                  string          `json:"cwd"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolUseID            string          `json:"tool_use_id"`
	NotificationType     string          `json:"notification_type"`
	Message              string          `json:"message"`
	LastAssistantMessage string          `json:"last_assistant_message"`
}

// record is a PreToolUse waiting for its PermissionRequest: the only hook
// of the pair that carries tool_use_id.
type record struct {
	toolUseID string
	toolName  string
	toolInput json.RawMessage
}

// maxRecords bounds the PreToolUse records kept per pane; parallel tool
// calls fire several before the first prompt.
const maxRecords = 8

type item struct {
	id        string
	kind      string
	toolName  string
	toolInput json.RawMessage
	cwd       string
	sessionID string
	createdAt time.Time
	pane      string
	loc       Location
}

type paneState struct {
	loc         Location
	sessionID   string
	records     []record
	pending     *item
	waiting     bool
	lastMessage string
}

// Attention states a pane can be in, as wire.Workspace.Attention spells them.
const (
	attentionPermission = "permission"
	attentionInput      = "input"
)

// Status is what the workspace list shows for a pane the feed knows about.
type Status struct {
	Attention string
	Preview   string
}

// Feed is the per-pane state behind PendingFeed/FeedReply on a tmux host.
type Feed struct {
	panes Panes
	now   func() time.Time
	// replyWait bounds how long a reply polls the screen for the prompt
	// before refusing, and how long a new item is believed without a
	// screen check: Claude draws the prompt only after the
	// PermissionRequest hook exits, so a YOLO reply or a refetch issued on
	// the frame that hook produced arrives a beat early.
	replyWait, replyPoll time.Duration

	mu    sync.Mutex
	state map[string]*paneState
}

// New returns a feed over panes.
func New(panes Panes) *Feed {
	return &Feed{
		panes:     panes,
		now:       time.Now,
		replyWait: 2 * time.Second,
		replyPoll: 100 * time.Millisecond,
		state:     map[string]*paneState{},
	}
}

// Handle applies one forwarded hook and returns the event frames it
// produced, for the caller to broadcast once the hook has been acknowledged.
func (f *Feed) Handle(ctx context.Context, env Envelope) ([]wire.EventFrame, error) {
	var ev hookEvent
	if err := json.Unmarshal(env.Event, &ev); err != nil {
		return nil, err
	}
	if env.Pane == "" || ev.Name == "" {
		return nil, errors.New("agentfeed: envelope without pane or event name")
	}
	loc, err := f.panes.Locate(ctx, env.Socket, env.Pane)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	ps := f.state[env.Pane]
	if ps == nil {
		ps = &paneState{}
		f.state[env.Pane] = ps
	}
	ps.loc = loc
	if ev.SessionID != "" {
		ps.sessionID = ev.SessionID
	}

	switch ev.Name {
	case eventPreToolUse:
		ps.records = append(ps.records, record{toolUseID: ev.ToolUseID, toolName: ev.ToolName, toolInput: ev.ToolInput})
		if len(ps.records) > maxRecords {
			ps.records = ps.records[len(ps.records)-maxRecords:]
		}
	case eventPermissionRequest:
		ps.waiting = false
		ps.pending = f.newItem(env.Pane, ps, ev)
		return []wire.EventFrame{attentionFrame(ps.pending, promptKind(ps.pending))}, nil
	case eventPostToolUse:
		ps.dropRecord(ev.ToolUseID)
		if ps.pending != nil && ps.pending.id == ev.ToolUseID {
			ps.pending = nil
		}
	case eventNotification:
		return f.handleNotification(env.Pane, ps, ev), nil
	case eventStop:
		ps.pending = nil
		ps.records = nil
		ps.waiting = true
		ps.lastMessage = truncate(ev.LastAssistantMessage)
		return []wire.EventFrame{{Type: "notification", Name: ev.Name, WorkspaceID: loc.WorkspaceID, SurfaceID: loc.SurfaceID}}, nil
	case eventUserPromptSubmit:
		ps.pending = nil
		ps.waiting = false
		ps.lastMessage = ""
	case eventSessionEnd:
		delete(f.state, env.Pane)
		return []wire.EventFrame{{Type: "notification", Name: ev.Name, WorkspaceID: loc.WorkspaceID, SurfaceID: loc.SurfaceID}}, nil
	}
	return nil, nil
}

// handleNotification: permission_prompt arrives seconds after the prompt
// PermissionRequest already announced, so it only fills in for a missed
// PermissionRequest; idle_prompt is the "waiting for your input" alert.
func (f *Feed) handleNotification(pane string, ps *paneState, ev hookEvent) []wire.EventFrame {
	switch ev.NotificationType {
	case notificationPermissionPrompt:
		if ps.pending != nil {
			return nil
		}
		ps.pending = f.newItem(pane, ps, ev)
		return []wire.EventFrame{attentionFrame(ps.pending, eventNotification)}
	case notificationIdlePrompt:
		ps.waiting = true
		return []wire.EventFrame{{
			Type: "feed", Name: ev.Name, Kind: eventNotification, NeedsAttention: true,
			WorkspaceID: ps.loc.WorkspaceID, SurfaceID: ps.loc.SurfaceID, Title: filepath.Base(ev.CWD),
		}}
	}
	return []wire.EventFrame{{Type: "notification", Name: ev.Name, WorkspaceID: ps.loc.WorkspaceID, SurfaceID: ps.loc.SurfaceID}}
}

// newItem builds the pane's pending prompt from a PermissionRequest (or a
// permission_prompt Notification that had none), keyed by the tool_use_id
// of the PreToolUse record carrying the same tool call. Its cwd is the
// pane's, as the workspace list reports it, so the app's cwd match between
// item and workspace holds byte for byte.
func (f *Feed) newItem(pane string, ps *paneState, ev hookEvent) *item {
	it := &item{
		kind:      wire.FeedKindPermissionRequest,
		toolName:  ev.ToolName,
		toolInput: ev.ToolInput,
		cwd:       ps.loc.CWD,
		sessionID: ps.sessionID,
		createdAt: f.now(),
		pane:      pane,
		loc:       ps.loc,
	}
	if it.cwd == "" {
		it.cwd = ev.CWD
	}
	if ev.ToolName == toolAskUserQuestion {
		it.kind = wire.FeedKindQuestion
	}
	if r, ok := ps.takeRecord(ev.ToolName, ev.ToolInput); ok && r.toolUseID != "" {
		it.id = r.toolUseID
	} else {
		it.id = "prompt-" + strings.TrimPrefix(pane, "%") + "-" + it.createdAt.UTC().Format("20060102T150405.000000000")
	}
	return it
}

func (ps *paneState) takeRecord(toolName string, toolInput json.RawMessage) (record, bool) {
	for i := len(ps.records) - 1; i >= 0; i-- {
		r := ps.records[i]
		if r.toolName == toolName && (toolName == "" || sameJSON(r.toolInput, toolInput)) {
			ps.records = append(ps.records[:i], ps.records[i+1:]...)
			return r, true
		}
	}
	return record{}, false
}

func (ps *paneState) dropRecord(toolUseID string) {
	for i, r := range ps.records {
		if r.toolUseID == toolUseID {
			ps.records = append(ps.records[:i], ps.records[i+1:]...)
			return
		}
	}
}

func sameJSON(a, b json.RawMessage) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	ca, _ := json.Marshal(va)
	cb, _ := json.Marshal(vb)
	return string(ca) == string(cb)
}

// promptKind names the frame's Kind the way cmuxhost does (the Claude Code
// hook behind the prompt), so the server's push phrases carry over.
func promptKind(it *item) string {
	if it.kind == wire.FeedKindQuestion {
		return toolAskUserQuestion
	}
	return eventPermissionRequest
}

func attentionFrame(it *item, kind string) wire.EventFrame {
	return wire.EventFrame{
		Type: "feed", Name: eventPermissionRequest, Kind: kind, NeedsAttention: true,
		FeedID: it.id, WorkspaceID: it.loc.WorkspaceID, SurfaceID: it.loc.SurfaceID,
		Title: filepath.Base(it.cwd),
	}
}

// Statuses reports, per wire surface id, what the workspace list should
// show for panes with a prompt up or an agent waiting for input.
func (f *Feed) Statuses() map[string]Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]Status{}
	for _, ps := range f.state {
		switch {
		case ps.pending != nil:
			out[ps.loc.SurfaceID] = Status{Attention: attentionPermission, Preview: statusLine(ps.pending)}
		case ps.waiting:
			out[ps.loc.SurfaceID] = Status{Attention: attentionInput, Preview: ps.lastMessage}
		}
	}
	return out
}

func statusLine(it *item) string {
	if it.kind == wire.FeedKindQuestion {
		return "Claude has a question for you"
	}
	return "Claude needs your permission"
}

// maxPreview bounds the last assistant message kept as a workspace preview.
const maxPreview = 160

func truncate(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= maxPreview {
		return s
	}
	return strings.TrimRight(string(r[:maxPreview]), " ") + "…"
}

// PendingFeed is the items[] body the app reads, in the shape cmux's
// feed.list produces. A prompt whose option list has left the screen (the
// human at the SSH session answered, or pressed Esc) is dropped here, since
// no hook reports a dismissal. A brand-new item is exempt: the frame its
// PermissionRequest raised makes the app refetch at once, while Claude
// draws the prompt only after that hook returns (seen live 2026-09-14: the
// refetch pruned every item before its prompt existed, and the
// permission_prompt Notification six seconds later re-created it with no
// tool context).
func (f *Feed) PendingFeed(ctx context.Context) (json.RawMessage, error) {
	items := []wireItem{}
	for _, it := range f.pendingItems() {
		if f.now().Sub(it.createdAt) > f.replyWait {
			screen, err := f.panes.Capture(ctx, it.pane)
			if err != nil || len(optionsOnScreen(screen)) == 0 {
				f.clear(it)
				continue
			}
		}
		items = append(items, toWire(it))
	}
	return json.Marshal(map[string]any{"items": items})
}

func (f *Feed) pendingItems() []*item {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*item
	for _, ps := range f.state {
		if ps.pending != nil {
			out = append(out, ps.pending)
		}
	}
	return out
}

func (f *Feed) find(requestID string) (*item, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ps := range f.state {
		if ps.pending != nil && ps.pending.id == requestID {
			return ps.pending, true
		}
	}
	return nil, false
}

// clear forgets it if it is still the pane's pending prompt.
func (f *Feed) clear(it *item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ps := f.state[it.pane]; ps != nil && ps.pending == it {
		ps.pending = nil
	}
}

// FeedReply answers requestID by typing the digit of the option the reply
// picks. The option list is re-read from the pane right before typing and
// polled briefly if absent; a prompt that is not on screen is refused with
// host.ErrPromptGone rather than typed into whatever runs there now.
func (f *Feed) FeedReply(ctx context.Context, kind, requestID string, params map[string]any) error {
	it, ok := f.find(requestID)
	if !ok {
		return host.ErrPromptGone
	}
	if kind != it.kind {
		return host.ErrPromptGone
	}
	choose, err := chooserFor(it, params)
	if err != nil {
		return err
	}
	deadline := f.now().Add(f.replyWait)
	for {
		screen, err := f.panes.Capture(ctx, it.pane)
		if err != nil {
			return err
		}
		if opts := optionsOnScreen(screen); len(opts) > 0 {
			key, ok := choose(opts)
			if !ok {
				return host.ErrPromptGone
			}
			if err := f.panes.SendKeys(ctx, it.pane, key); err != nil {
				return err
			}
			f.clear(it)
			return nil
		}
		if !f.now().Before(deadline) {
			return host.ErrPromptGone
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(f.replyPoll):
		}
	}
}
