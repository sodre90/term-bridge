package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sodre90/term-bridge/internal/cmux"
	"github.com/sodre90/term-bridge/internal/wire"
)

// fakeTerminalScript answers replay with a canned render-grid and logs every
// invocation's argv to $CMUX_FAKE_LOG so the test can assert input dispatch.
const fakeTerminalScript = `#!/bin/sh
printf '%s\n' "$*" >> "$CMUX_FAKE_LOG"
case "$2" in
  mobile.terminal.replay)
    cat <<'JSON'
{"columns":80,"rows":24,"seq":1,"surface_id":"S","workspace_id":"W","render_grid":{"format":"cmux.render-grid.v1","columns":80,"rows":24,"row_spans":[]}}
JSON
    ;;
  *) echo '{"ok":true}' ;;
esac
`

// fakeChangingTerminalScript answers replay with content that changes on every
// call while keeping seq constant at 0 — mirroring real cmux, whose top-level
// seq (and render_grid.state_seq) never increments. The poll loop must detect
// the content change itself rather than gating on seq.
const fakeChangingTerminalScript = `#!/bin/sh
printf '%s\n' "$*" >> "$CMUX_FAKE_LOG"
case "$2" in
  mobile.terminal.replay)
    n=$(grep -c 'mobile.terminal.replay' "$CMUX_FAKE_LOG")
    cat <<JSON
{"columns":80,"rows":24,"seq":0,"surface_id":"S","workspace_id":"W","render_grid":{"format":"cmux.render-grid.v1","columns":80,"rows":24,"row_spans":[{"row":0,"text":"line-$n"}]}}
JSON
    ;;
  *) echo '{"ok":true}' ;;
esac
`

// fakeIdleTerminalScript answers replay with an unchanging screen whose
// render_revision and terminal_theme_revision advance on every call. This is
// what a real idle pane does (cmux-app-2nj): the content is byte-identical
// poll to poll and only the two counters move.
const fakeIdleTerminalScript = `#!/bin/sh
printf '%s\n' "$*" >> "$CMUX_FAKE_LOG"
case "$2" in
  mobile.terminal.replay)
    n=$(grep -c 'mobile.terminal.replay' "$CMUX_FAKE_LOG")
    cat <<JSON
{"columns":80,"rows":24,"seq":0,"surface_id":"S","workspace_id":"W","render_grid":{"format":"cmux.render-grid.v1","columns":80,"rows":24,"render_revision":$n,"terminal_theme_revision":$n,"row_spans":[{"row":0,"text":"idle"}]}}
JSON
    ;;
  *) echo '{"ok":true}' ;;
esac
`

func wsConnect(t *testing.T, srvURL, path, tok string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srvURL, "http") + path
	c, resp, err := websocket.DefaultDialer.Dial(u, map[string][]string{"Authorization": {"Bearer " + tok}})
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("ws dial %s failed (status %d): %v", path, code, err)
	}
	return c
}

func TestTerminalReplayOnConnect(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeTerminalScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsConnect(t, srv.URL, "/terminal/SURF1", tok)
	defer c.Close()

	armReadDeadline(t, c)
	var down wire.TerminalDown
	if err := c.ReadJSON(&down); err != nil {
		t.Fatal(err)
	}
	if down.Type != "replay" {
		t.Fatalf("first frame must be replay, got %q", down.Type)
	}
	if down.Columns != 80 || down.Rows != 24 {
		t.Fatalf("dimensions wrong: %+v", down)
	}
	if !strings.Contains(string(down.Grid), "cmux.render-grid.v1") {
		t.Fatalf("grid not passed through: %s", down.Grid)
	}
}

func TestTerminalInputDispatched(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeTerminalScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsConnect(t, srv.URL, "/terminal/SURF1", tok)
	defer c.Close()

	// Drain the initial replay frame.
	armReadDeadline(t, c)
	var down wire.TerminalDown
	if err := c.ReadJSON(&down); err != nil {
		t.Fatal(err)
	}

	if err := c.WriteJSON(wire.TerminalUp{Type: "input", Text: "ls\r"}); err != nil {
		t.Fatal(err)
	}

	waitForRPCLog(t, logPath, "mobile.terminal.input", "SURF1", "ls")
}

// fakeTerminalFailingInputScript replays fine but fails every
// mobile.terminal.input call, so the read loop's resulting ack carries Ok: false.
const fakeTerminalFailingInputScript = `#!/bin/sh
printf '%s\n' "$*" >> "$CMUX_FAKE_LOG"
case "$2" in
  mobile.terminal.replay)
    cat <<'JSON'
{"columns":80,"rows":24,"seq":1,"surface_id":"S","workspace_id":"W","render_grid":{"format":"cmux.render-grid.v1","columns":80,"rows":24,"row_spans":[]}}
JSON
    ;;
  mobile.terminal.input)
    echo "boom" >&2
    exit 1
    ;;
  *) echo '{"ok":true}' ;;
esac
`

func TestTerminalInputAcked(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeTerminalScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsConnect(t, srv.URL, "/terminal/SURF1", tok)
	defer c.Close()

	// Drain the initial replay frame.
	armReadDeadline(t, c)
	var down wire.TerminalDown
	if err := c.ReadJSON(&down); err != nil {
		t.Fatal(err)
	}

	if err := c.WriteJSON(wire.TerminalUp{Type: "input", Text: "ls\r", Seq: 42}); err != nil {
		t.Fatal(err)
	}

	armReadDeadline(t, c)
	var ack wire.TerminalDown
	if err := c.ReadJSON(&ack); err != nil {
		t.Fatalf("expected an ack frame, got: %v", err)
	}
	if ack.Type != "ack" || ack.Seq != 42 || !ack.Ok {
		t.Fatalf("unexpected ack frame: %+v", ack)
	}
}

func TestTerminalInputAckReflectsRpcFailure(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeTerminalFailingInputScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsConnect(t, srv.URL, "/terminal/SURF1", tok)
	defer c.Close()

	armReadDeadline(t, c)
	var down wire.TerminalDown
	if err := c.ReadJSON(&down); err != nil {
		t.Fatal(err)
	}

	if err := c.WriteJSON(wire.TerminalUp{Type: "input", Text: "ls\r", Seq: 7}); err != nil {
		t.Fatal(err)
	}

	armReadDeadline(t, c)
	var ack wire.TerminalDown
	if err := c.ReadJSON(&ack); err != nil {
		t.Fatalf("expected an ack frame, got: %v", err)
	}
	if ack.Type != "ack" || ack.Seq != 7 || ack.Ok || ack.Reason != "" {
		t.Fatalf("expected a failed ack (ok=false) with no reason -- cmux failed it, the bridge did not refuse it -- got: %+v", ack)
	}
}

func TestTerminalNoAckWhenSeqUnset(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeTerminalScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsConnect(t, srv.URL, "/terminal/SURF1", tok)
	defer c.Close()

	armReadDeadline(t, c)
	var down wire.TerminalDown
	if err := c.ReadJSON(&down); err != nil {
		t.Fatal(err)
	}

	if err := c.WriteJSON(wire.TerminalUp{Type: "resize", Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	// Nudge a second, seq'd message through and confirm *its* ack is the very
	// next frame -- proving the unseq'd resize above never produced one.
	if err := c.WriteJSON(wire.TerminalUp{Type: "resize", Columns: 81, Rows: 24, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	armReadDeadline(t, c)
	var ack wire.TerminalDown
	if err := c.ReadJSON(&ack); err != nil {
		t.Fatalf("expected an ack frame, got: %v", err)
	}
	if ack.Type != "ack" || ack.Seq != 1 {
		t.Fatalf("expected the seq=1 ack directly (no stray ack for the unseq'd resize), got: %+v", ack)
	}
}

func TestTerminalForwardsContentChange(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeChangingTerminalScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsConnect(t, srv.URL, "/terminal/SURF1", tok)
	defer c.Close()

	// First frame: the full replay.
	armReadDeadline(t, c)
	var replay wire.TerminalDown
	if err := c.ReadJSON(&replay); err != nil {
		t.Fatal(err)
	}
	if replay.Type != "replay" {
		t.Fatalf("first frame must be replay, got %q", replay.Type)
	}

	// The screen content changes on the next poll while seq stays 0. The poll
	// loop must forward it as an output frame rather than freezing on seq.
	armReadDeadline(t, c)
	var out wire.TerminalDown
	if err := c.ReadJSON(&out); err != nil {
		t.Fatalf("expected an output frame after content change, got: %v", err)
	}
	if out.Type != "output" {
		t.Fatalf("second frame must be output, got %q", out.Type)
	}
	if !strings.Contains(string(out.Grid), "line-") {
		t.Fatalf("output grid missing changed content: %s", out.Grid)
	}
}

// An idle pane must cost nothing after its replay. Before the fingerprint
// check this failed: the two counters moved every poll, so the unchanged-grid
// comparison never matched and a full grid went out four times a second.
func TestTerminalDoesNotForwardCounterOnlyChanges(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeIdleTerminalScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsConnect(t, srv.URL, "/terminal/SURF1", tok)
	defer c.Close()

	armReadDeadline(t, c)
	var replay wire.TerminalDown
	if err := c.ReadJSON(&replay); err != nil {
		t.Fatal(err)
	}
	if replay.Type != "replay" {
		t.Fatalf("first frame must be replay, got %q", replay.Type)
	}

	// Long enough for several 250ms polls to have run and been discarded.
	if err := c.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var out wire.TerminalDown
	err := c.ReadJSON(&out)
	if err == nil {
		t.Fatalf("forwarded a frame for a counter-only change: %s", out.Grid)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("want a read timeout (nothing forwarded), got: %v", err)
	}

	// The poll loop must still be running, not wedged: assert it actually
	// called replay repeatedly while forwarding none of it.
	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if n := strings.Count(string(data), "mobile.terminal.replay"); n < 2 {
		t.Fatalf("poll loop stopped polling: only %d replay calls", n)
	}
}

func TestGridFingerprintIgnoresVolatileCounters(t *testing.T) {
	a := json.RawMessage(`{"rows":24,"render_revision":1,"terminal_theme_revision":7,"row_spans":[{"row":0,"text":"x"}]}`)
	b := json.RawMessage(`{"rows":24,"render_revision":2,"terminal_theme_revision":8,"row_spans":[{"row":0,"text":"x"}]}`)
	if !bytes.Equal(gridFingerprint(a), gridFingerprint(b)) {
		t.Fatal("grids differing only in the volatile counters must fingerprint equal")
	}
}

func TestGridFingerprintKeepsRealContentChanges(t *testing.T) {
	a := json.RawMessage(`{"render_revision":1,"row_spans":[{"row":0,"text":"x"}]}`)
	b := json.RawMessage(`{"render_revision":1,"row_spans":[{"row":0,"text":"y"}]}`)
	if bytes.Equal(gridFingerprint(a), gridFingerprint(b)) {
		t.Fatal("a changed row span must not fingerprint equal")
	}
}

// Key order is not guaranteed across cmux replies, and a map marshals in
// sorted order, so the same content encoded two ways must still compare equal.
func TestGridFingerprintIsOrderIndependent(t *testing.T) {
	a := json.RawMessage(`{"rows":24,"columns":80}`)
	b := json.RawMessage(`{"columns":80,"rows":24}`)
	if !bytes.Equal(gridFingerprint(a), gridFingerprint(b)) {
		t.Fatal("the same content in a different key order must fingerprint equal")
	}
}

// A grid that will not decode must compare exactly as it did before the
// fingerprint existed -- redundant frames, never suppressed ones.
func TestGridFingerprintFallsBackToRawBytes(t *testing.T) {
	bad := json.RawMessage(`not json`)
	if !bytes.Equal(gridFingerprint(bad), bad) {
		t.Fatal("an undecodable grid must fall back to its raw bytes")
	}
	if bytes.Equal(gridFingerprint(bad), gridFingerprint(json.RawMessage(`also not json`))) {
		t.Fatal("two different undecodable grids must not collapse together")
	}
}

func TestTerminalMissingIDRejected(t *testing.T) {
	s, tok := newTestServer(t, fakeTerminalScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	// The {id} pattern won't match an empty segment, so /terminal/ is 404 — a
	// non-upgrade response. Assert the dial fails rather than upgrading.
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/terminal/"
	_, _, err := websocket.DefaultDialer.Dial(u, map[string][]string{"Authorization": {"Bearer " + tok}})
	if err == nil {
		t.Fatal("expected dial to fail for empty surface id")
	}
}

// closeGoneProbe stands a WebSocket up and hands closeIfSurfaceGone the given
// error on the server side, returning the close code the client observes.
// gorilla reports a peer that just hangs up as CloseAbnormalClosure (1006),
// which is what "no close frame was sent" looks like from here.
func closeGoneProbe(t *testing.T, serverErr error) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		closeIfSurfaceGone(c, serverErr)
	}))
	defer srv.Close()

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	armReadDeadline(t, c)
	_, _, readErr := c.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(readErr, &closeErr) {
		t.Fatalf("want a close error, got %v", readErr)
	}
	return closeErr.Code
}

// cmux-app-34c. Without this the socket just ended, which is indistinguishable
// from a dropped connection, so the app backed off and reconnected to the same
// dead surface every 5s -- showing a spinner the whole time.
func TestASurfaceCmuxNoLongerHasIsClosedAsGone(t *testing.T) {
	gone := &cmux.RPCError{Method: "mobile.terminal.replay", Code: "not_found", Message: "Terminal surface not found"}

	if got := closeGoneProbe(t, gone); got != wire.CloseSurfaceGone {
		t.Fatalf("close code = %d, want %d", got, wire.CloseSurfaceGone)
	}
}

// Everything else can succeed on the next attempt, so it must keep closing the
// old way and be retried. Telling a live pane it is gone is the worse failure.
func TestARetryableFailureIsNotClosedAsGone(t *testing.T) {
	cases := map[string]error{
		"a different cmux refusal": &cmux.RPCError{Method: "mobile.terminal.replay", Code: "internal", Message: "boom"},
		"cmux unreachable":         errors.New("dial /tmp/cmux.sock: connection refused"),
		"a timeout":                context.DeadlineExceeded,
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			if got := closeGoneProbe(t, err); got == wire.CloseSurfaceGone {
				t.Fatalf("%v was closed as surface-gone", err)
			}
		})
	}
}

// The reason string stays empty: the relay is deliberately blind, and a close
// code it can already infer from the connection ending tells it nothing new,
// while text about the surface would.
func TestTheGoneCloseCarriesNoReasonText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		closeIfSurfaceGone(c, &cmux.RPCError{Method: "m", Code: "not_found", Message: "Terminal surface not found"})
	}))
	defer srv.Close()

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	armReadDeadline(t, c)
	_, _, readErr := c.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(readErr, &closeErr) {
		t.Fatalf("want a close error, got %v", readErr)
	}
	if closeErr.Text != "" {
		t.Fatalf("close reason = %q, want empty", closeErr.Text)
	}
}

// -- a slow cmux must not tear down a live pane (cmux-app-8a0)

// fakeStallingTerminalScript answers the first replay, then fails the next
// three, then answers again. Mirrors the measured failure: cmux stops
// answering mobile.terminal.replay for a while and then comes back.
const fakeStallingTerminalScript = `#!/bin/sh
printf '%s\n' "$*" >> "$CMUX_FAKE_LOG"
case "$2" in
  mobile.terminal.replay)
    n=$(grep -c mobile.terminal.replay "$CMUX_FAKE_LOG")
    if [ "$n" -ge 2 ] && [ "$n" -le 4 ]; then
      echo "Error: internal: cmux is busy" >&2
      exit 1
    fi
    cat <<JSON
{"columns":80,"rows":24,"seq":0,"surface_id":"S","workspace_id":"W","render_grid":{"format":"cmux.render-grid.v1","columns":80,"rows":24,"row_spans":[{"row":0,"text":"line-$n"}]}}
JSON
    ;;
  *) echo '{"ok":true}' ;;
esac
`

// fakeDeadTerminalScript answers the first replay and then never answers again.
const fakeDeadTerminalScript = `#!/bin/sh
printf '%s\n' "$*" >> "$CMUX_FAKE_LOG"
case "$2" in
  mobile.terminal.replay)
    n=$(grep -c mobile.terminal.replay "$CMUX_FAKE_LOG")
    if [ "$n" -ge 2 ]; then
      echo "Error: internal: cmux is busy" >&2
      exit 1
    fi
    cat <<JSON
{"columns":80,"rows":24,"seq":0,"surface_id":"S","workspace_id":"W","render_grid":{"format":"cmux.render-grid.v1","columns":80,"rows":24,"row_spans":[{"row":0,"text":"line-1"}]}}
JSON
    ;;
  *) echo '{"ok":true}' ;;
esac
`

// THE regression. A failed poll used to return false, which ended the handler
// and closed the socket -- so one slow replay dropped a healthy pane, the phone
// reconnected, and the reconnect issued another full replay against the cmux
// that was already too slow to serve one. The grid on screen is still valid, so
// the socket must survive and recover on its own.
func TestASlowReplayDoesNotCloseALivePane(t *testing.T) {
	t.Setenv("CMUX_FAKE_LOG", t.TempDir()+"/cmux.log")
	s, tok := newTestServer(t, fakeStallingTerminalScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsConnect(t, srv.URL, "/terminal/SURF1", tok)
	defer c.Close()

	armReadDeadline(t, c)
	var replay wire.TerminalDown
	if err := c.ReadJSON(&replay); err != nil {
		t.Fatal(err)
	}
	if replay.Type != "replay" {
		t.Fatalf("first frame must be replay, got %q", replay.Type)
	}

	// Three replays fail in between. If any of them closed the socket this read
	// returns a close/EOF error instead of the frame from the fifth call.
	armReadDeadline(t, c)
	var out wire.TerminalDown
	if err := c.ReadJSON(&out); err != nil {
		t.Fatalf("socket did not survive the failing replays: %v", err)
	}
	if !strings.Contains(string(out.Grid), "line-5") {
		t.Fatalf("want the frame from after the recovery, got: %s", out.Grid)
	}
}

// The safety valve: holding the pane is bounded, so a cmux that never comes
// back does not keep the socket forever.
func TestAPaneIsGivenUpOnceTheGraceRunsOut(t *testing.T) {
	t.Setenv("CMUX_FAKE_LOG", t.TempDir()+"/cmux.log")
	s, tok := newTestServer(t, fakeDeadTerminalScript)
	s.replayGrace = 300 * time.Millisecond
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsConnect(t, srv.URL, "/terminal/SURF1", tok)
	defer c.Close()

	armReadDeadline(t, c)
	var replay wire.TerminalDown
	if err := c.ReadJSON(&replay); err != nil {
		t.Fatal(err)
	}

	// Must be a CLOSE, not merely silence: a socket that simply stops
	// answering would satisfy "err != nil" via the read deadline and hide an
	// unbounded hold.
	armReadDeadline(t, c)
	_, _, err := c.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("want the socket closed once the grace elapsed, got %v", err)
	}
}

// -- replayOutage

func outageAt(grace time.Duration, clock *time.Time) *replayOutage {
	return &replayOutage{now: func() time.Time { return *clock }, grace: grace}
}

func TestAnOutageIsNotOngoingUntilSomethingFails(t *testing.T) {
	now := time.Date(2026, 9, 8, 23, 0, 0, 0, time.UTC)
	o := outageAt(time.Minute, &now)

	if o.ongoing() {
		t.Fatal("a fresh outage must not report itself as ongoing")
	}
	if o.recovered() != 0 {
		t.Fatal("recovered must be zero when nothing was wrong -- otherwise every successful poll logs a recovery")
	}
}

func TestThePaneIsHeldUntilTheGraceElapses(t *testing.T) {
	now := time.Date(2026, 9, 8, 23, 0, 0, 0, time.UTC)
	o := outageAt(time.Minute, &now)

	if !o.keepWaiting() {
		t.Fatal("the first failure must never give up -- that is the storm this fixes")
	}
	now = now.Add(59 * time.Second)
	if !o.keepWaiting() {
		t.Fatal("still inside the grace, must keep holding")
	}
	now = now.Add(2 * time.Second)
	if o.keepWaiting() {
		t.Fatal("past the grace, must give up")
	}
}

func TestRecoveryReportsHowLongTheOutageRanAndClearsIt(t *testing.T) {
	now := time.Date(2026, 9, 8, 23, 0, 0, 0, time.UTC)
	o := outageAt(time.Minute, &now)
	o.keepWaiting()

	now = now.Add(30 * time.Second)
	if down := o.recovered(); down != 30*time.Second {
		t.Fatalf("outage length = %v, want 30s", down)
	}
	if o.ongoing() {
		t.Fatal("recovered must end the outage")
	}
}

// A second outage after a recovery gets its own full grace -- otherwise a pane
// that flaps all day would be given up on for a failure it had just recovered
// from.
func TestAFreshOutageAfterRecoveryGetsTheFullGraceAgain(t *testing.T) {
	now := time.Date(2026, 9, 8, 23, 0, 0, 0, time.UTC)
	o := outageAt(time.Minute, &now)
	o.keepWaiting()
	now = now.Add(59 * time.Second)
	o.recovered()

	now = now.Add(time.Hour)
	if !o.keepWaiting() {
		t.Fatal("a new outage must start its grace from scratch")
	}
}

// The interval is a client preference, so the only thing the bridge owes it is
// bounds: never faster than the old fixed rate (every tick is a cmux replay on
// the user's Mac), never so slow the pane stops feeling live.
func TestTheClientPollIntervalIsHeldToItsBounds(t *testing.T) {
	fallback := 250 * time.Millisecond
	for _, c := range []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"absent falls back", "", fallback},
		{"unreadable falls back", "soon", fallback},
		{"a plain value is honoured", "1000", time.Second},
		{"below the floor is raised", "10", minTerminalPoll},
		{"zero is raised, not treated as off", "0", minTerminalPoll},
		{"negative is raised", "-5000", minTerminalPoll},
		{"above the ceiling is capped", "600000", maxTerminalPoll},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := terminalPollInterval(c.raw, fallback); got != c.want {
				t.Fatalf("terminalPollInterval(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

// The clamp is unit-tested above; this is the wiring. A pane whose content
// changes every poll delivers an output frame promptly at the default rate, and
// must not at a ten-second one -- which is only observable if the handler
// actually gave the ticker the client's interval.
func TestASlowClientIntervalHoldsBackTheNextOutputFrame(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeChangingTerminalScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	readReplayThenWaitForOutput := func(t *testing.T, query string) error {
		t.Helper()
		c := wsConnect(t, srv.URL, "/terminal/SURF1"+query, tok)
		defer c.Close()
		armReadDeadline(t, c)
		var replay wire.TerminalDown
		if err := c.ReadJSON(&replay); err != nil {
			t.Fatalf("replay: %v", err)
		}
		// Well inside the slow interval and well outside the default one.
		if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var out wire.TerminalDown
		return c.ReadJSON(&out)
	}

	if err := readReplayThenWaitForOutput(t, ""); err != nil {
		t.Fatalf("at the default interval an output frame must arrive: %v", err)
	}
	if err := readReplayThenWaitForOutput(t, "?poll_ms=10000"); err == nil {
		t.Fatal("at a 10s interval no output frame may arrive within 2s")
	}
}
