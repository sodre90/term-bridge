package cmuxhost

import (
	"encoding/json"
	"testing"
)

func TestNeedsAttention(t *testing.T) {
	// cmux feed items carry the Claude Code hook event in payload.hook_event_name
	// and fire twice: phase "received" then "completed". We alert once, on
	// "received", and only for the events that block on the user.
	cases := []struct {
		hook, phase string
		want        bool
	}{
		{"Notification", "received", true},
		{"AskUserQuestion", "received", true},
		{"Notification", "completed", false},
		{"AskUserQuestion", "completed", false},
		{"PreToolUse", "received", false},
		{"Stop", "received", false},
		{"SubagentStop", "received", false},
		{"UserPromptSubmit", "received", false},
		{"SessionStart", "received", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := needsAttention(c.hook, c.phase); got != c.want {
			t.Errorf("needsAttention(%q,%q)=%v want %v", c.hook, c.phase, got, c.want)
		}
	}
}

func TestClassifyDropsNoise(t *testing.T) {
	var m map[string]any
	_ = json.Unmarshal([]byte(`{"type":"event","name":"surface.selected","category":"surface","payload":{}}`), &m)
	if _, ok := classify(m); ok {
		t.Fatal("surface churn should be dropped")
	}
	var ack map[string]any
	_ = json.Unmarshal([]byte(`{"type":"ack","protocol":"cmux-events"}`), &ack)
	if _, ok := classify(ack); ok {
		t.Fatal("ack should be dropped")
	}
}

func TestClassifyFeedAttentionPrompt(t *testing.T) {
	// A real cmux feed frame: the blocking signal is payload.hook_event_name,
	// the feed id is the top-level id, and the only human label available
	// (cmux redacts the prompt) is the cwd basename.
	raw := `{"type":"event","name":"feed.item.received","category":"feed",
		"id":"BOOT-168","workspace_id":"W1",
		"payload":{"hook_event_name":"Notification","phase":"received",
			"session_id":"claude-abc","cwd":"/Users/perdos/prj/cmux-app","workspace_id":"W1"}}`
	var m map[string]any
	_ = json.Unmarshal([]byte(raw), &m)
	f, ok := classify(m)
	if !ok {
		t.Fatal("feed prompt should be forwarded")
	}
	if f.Type != "feed" || !f.NeedsAttention || f.FeedID != "BOOT-168" || f.Kind != "Notification" {
		t.Fatalf("unexpected frame: %+v", f)
	}
	if f.WorkspaceID != "W1" {
		t.Fatalf("workspace id wrong: %+v", f)
	}
	if f.Title != "cmux-app" { // cwd basename, used as the "which agent" label
		t.Fatalf("title (cwd basename) wrong: %+v", f)
	}
}

func TestClassifyFeedNonBlocking(t *testing.T) {
	// PreToolUse fires constantly and must never alert; the "completed" phase of
	// an attention event must not re-alert either.
	for _, raw := range []string{
		`{"type":"event","category":"feed","id":"BOOT-1","payload":{"hook_event_name":"PreToolUse","phase":"received","cwd":"/x/y","tool_name":"Bash"}}`,
		`{"type":"event","category":"feed","id":"BOOT-2","payload":{"hook_event_name":"Notification","phase":"completed","cwd":"/x/y"}}`,
	} {
		var m map[string]any
		_ = json.Unmarshal([]byte(raw), &m)
		f, ok := classify(m)
		if !ok {
			t.Fatalf("feed frame should be forwarded: %s", raw)
		}
		if f.NeedsAttention {
			t.Fatalf("frame must not set attention: %+v", f)
		}
	}
}

func TestClassifyNotificationNoAttention(t *testing.T) {
	raw := `{"type":"event","name":"notification.created","category":"notification",
		"payload":{"notification_id":"N1","title":null,"surface_id":"S1","workspace_id":"W1"}}`
	var m map[string]any
	_ = json.Unmarshal([]byte(raw), &m)
	f, ok := classify(m)
	if !ok {
		t.Fatal("notification should be forwarded")
	}
	if f.Type != "notification" || f.NeedsAttention {
		t.Fatalf("notification must not set attention (redacted body): %+v", f)
	}
	if f.SurfaceID != "S1" || f.WorkspaceID != "W1" {
		t.Fatalf("ids wrong: %+v", f)
	}
}
