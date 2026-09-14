package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sodre90/cmux-bridge/internal/wire"
)

// wsDial connects a websocket client to the test server's /events with token.
func wsDial(t *testing.T, srvURL, tok string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srvURL, "http") + "/events"
	h := http.Header{"Authorization": {"Bearer " + tok}}
	c, resp, err := websocket.DefaultDialer.Dial(u, h)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("ws dial failed (status %d): %v", code, err)
	}
	return c
}

func TestWSEventsDeliversBroadcast(t *testing.T) {
	s, tok := newTestServer(t, "#!/bin/sh\necho '{}'\n")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsDial(t, srv.URL, tok)
	defer c.Close()
	waitForSubscribers(t, s.hub, 1)

	s.hub.broadcast(wire.EventFrame{Type: "feed", FeedID: "X", NeedsAttention: true})

	armReadDeadline(t, c)
	var got wire.EventFrame
	if err := c.ReadJSON(&got); err != nil {
		t.Fatal(err)
	}
	if got.FeedID != "X" || !got.NeedsAttention {
		t.Fatalf("unexpected frame: %+v", got)
	}
}

func TestWSEventsRejectsNoToken(t *testing.T) {
	s, _ := newTestServer(t, "#!/bin/sh\necho '{}'\n")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/events"
	_, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err == nil {
		t.Fatal("expected dial to fail without token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 handshake, got %v", resp)
	}
}

func TestIngestEventsBroadcastsClassified(t *testing.T) {
	s, tok := newTestServer(t, "#!/bin/sh\necho '{}'\n")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsDial(t, srv.URL, tok)
	defer c.Close()
	waitForSubscribers(t, s.hub, 1)

	feed := `{"type":"event","name":"feed.item.received","category":"feed","id":"BOOT-9","payload":{"hook_event_name":"AskUserQuestion","phase":"received","cwd":"/Users/perdos/prj/cmux-app"}}`
	noise := `{"type":"event","name":"pane.focused","category":"pane","payload":{}}`
	go s.ingestEvents(context.Background(), strings.NewReader(noise+"\n"+feed+"\n"))

	armReadDeadline(t, c)
	var got wire.EventFrame
	if err := c.ReadJSON(&got); err != nil {
		t.Fatal(err)
	}
	if got.FeedID != "BOOT-9" || got.Kind != "AskUserQuestion" || !got.NeedsAttention {
		t.Fatalf("expected the classified feed frame, got %+v", got)
	}
}

func TestIngestEventsUpdatesLastEventAt(t *testing.T) {
	s, _ := newTestServer(t, "#!/bin/sh\necho '{}'\n")

	if got := s.LastEventAt(); !got.IsZero() {
		t.Fatalf("LastEventAt before any event = %v, want zero", got)
	}

	notification := `{"type":"event","name":"notification.shown","category":"notification","payload":{"title":"hi"}}`
	s.ingestEvents(context.Background(), strings.NewReader(notification+"\n"))

	got := s.LastEventAt()
	if got.IsZero() {
		t.Fatal("LastEventAt after a classified event should be non-zero")
	}
	if time.Since(got) > 5*time.Second {
		t.Fatalf("LastEventAt = %v, too far in the past", got)
	}
}

func TestIngestEventsSkipsLastEventAtForUnclassifiedNoise(t *testing.T) {
	s, _ := newTestServer(t, "#!/bin/sh\necho '{}'\n")

	noise := `{"type":"event","name":"pane.focused","category":"pane","payload":{}}`
	s.ingestEvents(context.Background(), strings.NewReader(noise+"\n"))

	if got := s.LastEventAt(); !got.IsZero() {
		t.Fatalf("LastEventAt after only unclassified noise = %v, want zero", got)
	}
}

func TestIngestEventsEnrichesAttentionTitle(t *testing.T) {
	// Reuses sessions_test.go's fakeWorkspaceList: workspace "882CA6F0" has
	// title "✳ Build options" (cleaned to "Build options") and preview "Build
	// options trading system".
	script := "#!/bin/sh\ncat <<'JSON'\n" + fakeWorkspaceList + "\nJSON\n"
	s, tok := newTestServer(t, script)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsDial(t, srv.URL, tok)
	defer c.Close()
	waitForSubscribers(t, s.hub, 1)

	feed := `{"type":"event","name":"feed.item.received","category":"feed","id":"BOOT-9","payload":{"hook_event_name":"Notification","phase":"received","cwd":"/Users/u/prj/trading","workspace_id":"882CA6F0"}}`
	go s.ingestEvents(context.Background(), strings.NewReader(feed+"\n"))

	armReadDeadline(t, c)
	var got wire.EventFrame
	if err := c.ReadJSON(&got); err != nil {
		t.Fatal(err)
	}
	if got.Title != "Build options" {
		t.Fatalf("title not enriched: got %q want %q", got.Title, "Build options")
	}
	// The fixture's preview ("Build options trading system") is ordinary
	// last-activity text, not one of cmux's agent-status lines, so it says
	// nothing about why the agent needs the user and must not become the
	// notification body -- see agentStatusLine.
	if got.Preview != "" {
		t.Fatalf("preview should drop a non-status workspace preview, got %q", got.Preview)
	}
}

// promptScript answers the two RPCs resolveAttention makes for workspace WS1 at
// /tmp/proj. Its workspace preview is the real cmux system banner that shipped
// as a push notification body on a workspace that was in fact waiting for the
// user -- the bug this file's next few tests pin down.
func promptScript(pendingItems string) string {
	return `#!/bin/sh
case "$2" in
  mobile.workspace.list)
    echo '{"workspaces":[{"id":"WS1","current_directory":"/tmp/proj","title":"✳ Optimize llama.cpp for Laguna-S","preview":"macOS is reporting sustained critical memory pressure. cmux has shed hidden resources; close idle workspaces or restart cmux if pressure co…","terminals":[]}]}'
    ;;
  feed.list)
    echo '{"items":[` + pendingItems + `]}'
    ;;
  *)
    echo '{"ok":true}'
    ;;
esac
`
}

func enrichedPreview(t *testing.T, script string) wire.EventFrame {
	t.Helper()
	s, _ := newTestServer(t, script)
	f := wire.EventFrame{Type: "feed", Kind: "Notification", NeedsAttention: true, WorkspaceID: "WS1"}
	s.resolveAttention(context.Background(), &f)
	return f
}

// TestResolveAttentionPrefersPendingQuestionText is the regression for the
// reported bug: a workspace whose live preview held an unrelated cmux system
// banner pushed that banner as the notification body. The pending item's own
// question text is what the user needs to see instead.
func TestResolveAttentionPrefersPendingQuestionText(t *testing.T) {
	item := `{"request_id":"REQ1","kind":"question","status":"pending","cwd":"/tmp/proj","created_at":"2026-07-25T13:14:16Z","question_prompt":"Should the phone be able to write in the torrent folder?"}`
	got := enrichedPreview(t, promptScript(item))

	if got.Title != "Optimize llama.cpp for Laguna-S" {
		t.Fatalf("title not enriched: got %q", got.Title)
	}
	want := "Should the phone be able to write in the torrent folder?"
	if got.Preview != want {
		t.Fatalf("preview = %q, want the pending question text %q", got.Preview, want)
	}
}

func TestResolveAttentionDescribesPendingPermission(t *testing.T) {
	item := `{"request_id":"REQ1","kind":"permissionRequest","status":"pending","cwd":"/tmp/proj","created_at":"2026-07-25T13:14:16Z","tool_name":"Bash","tool_input":"{\"command\":\"./gradlew :app:assembleDebug\",\"description\":\"Build\"}"}`
	got := enrichedPreview(t, promptScript(item))

	want := "Wants to run Bash: ./gradlew :app:assembleDebug"
	if got.Preview != want {
		t.Fatalf("preview = %q, want %q", got.Preview, want)
	}
}

// An idle "Claude is waiting for your input" Notification has no pending feed
// item at all -- that status line is then the best thing we have, and unlike
// the system banner it does answer "why does this need me?".
func TestResolveAttentionFallsBackToAgentStatusLine(t *testing.T) {
	script := strings.Replace(promptScript(""),
		"macOS is reporting sustained critical memory pressure. cmux has shed hidden resources; close idle workspaces or restart cmux if pressure co…",
		"Claude is waiting for your input", 1)
	got := enrichedPreview(t, script)

	if got.Preview != "Claude is waiting for your input" {
		t.Fatalf("preview = %q, want the agent status line", got.Preview)
	}
}

// With neither a pending item nor a recognizable status line there is nothing
// truthful to say, so Preview stays empty and PushBody falls back to its
// hook-derived phrase rather than showing whatever cmux last displayed.
func TestResolveAttentionLeavesPreviewEmptyWhenNothingIsKnown(t *testing.T) {
	got := enrichedPreview(t, promptScript(""))

	if got.Preview != "" {
		t.Fatalf("preview = %q, want empty", got.Preview)
	}
	if body := got.PushBody(); body != "Needs your attention" {
		t.Fatalf("PushBody() = %q, want the hook-derived fallback", body)
	}
}

// A pending item in another workspace's cwd must never describe this one.
func TestResolveAttentionIgnoresPendingItemFromAnotherCWD(t *testing.T) {
	item := `{"request_id":"REQ1","kind":"question","status":"pending","cwd":"/tmp/other","created_at":"2026-07-25T13:14:16Z","question_prompt":"Someone else's question?"}`
	got := enrichedPreview(t, promptScript(item))

	if got.Preview != "" {
		t.Fatalf("preview = %q, want empty for a non-matching cwd", got.Preview)
	}
}

func TestIngestEventsKeepsFallbackTitleWhenWorkspaceNotFound(t *testing.T) {
	script := "#!/bin/sh\ncat <<'JSON'\n" + fakeWorkspaceList + "\nJSON\n"
	s, tok := newTestServer(t, script)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsDial(t, srv.URL, tok)
	defer c.Close()
	waitForSubscribers(t, s.hub, 1)

	feed := `{"type":"event","name":"feed.item.received","category":"feed","id":"BOOT-10","payload":{"hook_event_name":"Notification","phase":"received","cwd":"/x/y","workspace_id":"UNKNOWN"}}`
	go s.ingestEvents(context.Background(), strings.NewReader(feed+"\n"))

	armReadDeadline(t, c)
	var got wire.EventFrame
	if err := c.ReadJSON(&got); err != nil {
		t.Fatal(err)
	}
	if got.Preview != "" {
		t.Fatalf("preview should stay empty when the lookup misses, got %q", got.Preview)
	}
	if got.Title != "y" { // cwd basename fallback, unchanged when the lookup misses
		t.Fatalf("title should fall back to cwd basename, got %q", got.Title)
	}
}

func TestIngestEventsAutoResolvesWhenYoloEnabled(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeYoloScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if err := s.yolo.SetMode("WS1", "bypass"); err != nil {
		t.Fatal(err)
	}

	c := wsDial(t, srv.URL, tok)
	defer c.Close()
	waitForSubscribers(t, s.hub, 1)

	feed := `{"type":"event","name":"feed.item.received","category":"feed","id":"BOOT-11","payload":{"hook_event_name":"Notification","phase":"received","cwd":"/tmp/proj","workspace_id":"WS1"}}`
	go s.ingestEvents(context.Background(), strings.NewReader(feed+"\n"))

	armReadDeadline(t, c)
	var got wire.EventFrame
	if err := c.ReadJSON(&got); err != nil {
		t.Fatal(err)
	}

	// resolveAttention answers the prompt synchronously before broadcast (see
	// events.go), so by the time the frame arrives the reply has already
	// happened and NeedsAttention has been cleared -- neither this agent's own
	// push nor the relay's pushmon (which trusts this flag alone) should fire.
	if got.NeedsAttention {
		t.Fatal("NeedsAttention should be cleared once YOLO auto-resolves the pending permission")
	}
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "feed.permission.reply") {
		t.Fatalf("expected an auto-reply for the yolo-enabled workspace; log:\n%s", data)
	}
}

func TestIngestEventsDoesNotAutoResolveWhenYoloOff(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeYoloScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	// WS1's yolo mode is left off (the default).

	c := wsDial(t, srv.URL, tok)
	defer c.Close()
	waitForSubscribers(t, s.hub, 1)

	feed := `{"type":"event","name":"feed.item.received","category":"feed","id":"BOOT-12","payload":{"hook_event_name":"Notification","phase":"received","cwd":"/tmp/proj","workspace_id":"WS1"}}`
	go s.ingestEvents(context.Background(), strings.NewReader(feed+"\n"))

	armReadDeadline(t, c)
	var got wire.EventFrame
	if err := c.ReadJSON(&got); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // let a stray goroutine misbehave, if any
	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "feed.permission.reply") {
		t.Fatalf("must not auto-reply when yolo mode is off; log:\n%s", data)
	}
}

// TestResolveAttentionMakesOneLookupPerRPC pins the shared-lookup structure:
// notification content and YOLO's auto-approve both need the frame's workspace
// and the prompts pending in its cwd, and each doing its own pair of calls
// blocked ingestEvents's single-goroutine scan loop for twice as long as
// necessary. YOLO on is the worst case -- it is the path that used to make the
// second pair.
func TestResolveAttentionMakesOneLookupPerRPC(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, _ := newTestServer(t, fakeYoloScript)
	if err := s.yolo.SetMode("WS1", "bypass"); err != nil {
		t.Fatal(err)
	}

	f := wire.EventFrame{Type: "feed", Kind: "Notification", NeedsAttention: true, WorkspaceID: "WS1"}
	if s.resolveAttention(context.Background(), &f) {
		t.Fatal("YOLO should have answered the pending permission")
	}

	data, _ := os.ReadFile(logPath)
	log := string(data)
	for _, method := range []string{"mobile.workspace.list", "feed.list"} {
		if got := strings.Count(log, method); got != 1 {
			t.Fatalf("%s called %d times, want exactly 1; log:\n%s", method, got, log)
		}
	}
	if !strings.Contains(log, "feed.permission.reply") {
		t.Fatalf("the shared lookup must still reach the auto-reply; log:\n%s", log)
	}
}
