package server

import (
	"bytes"
	"compress/flate"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sodre90/term-bridge/internal/cmux"
	"github.com/sodre90/term-bridge/internal/e2e"
	"github.com/sodre90/term-bridge/internal/testutil"
	"github.com/sodre90/term-bridge/internal/wire"
)

func wsConnectEncrypted(t *testing.T, srvURL, path, relayTok, deviceID string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srvURL, "http") + path
	h := http.Header{"X-Relay-Token": {relayTok}, "X-Device-ID": {deviceID}}
	c, resp, err := websocket.DefaultDialer.Dial(u, h)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("ws dial %s failed (status %d): %v", path, code, err)
	}
	return c
}

func TestTerminalReplayEncryptedWhenSessionsSet(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	bin := testutil.WriteFakeCmux(t, fakeTerminalScript)
	s := New(&cmux.Client{Bin: bin}, nil)
	sessions, deviceID, secret := pairedSessions(t)
	s.SetSessions(sessions)

	const relayTok = "relay-secret"
	srv := httptest.NewServer(s.TrustedHandler(relayTok))
	defer srv.Close()

	c := wsConnectEncrypted(t, srv.URL, "/terminal/SURF1", relayTok, deviceID)
	defer c.Close()

	armReadDeadline(t, c)
	msgType, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if msgType != websocket.BinaryMessage {
		t.Fatalf("want a binary (encrypted) frame, got message type %d", msgType)
	}
	counter, plain, err := e2e.DecodeFrame(secret, e2e.DirAgentToDevice, raw)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if counter != 0 {
		t.Fatalf("want first frame counter 0, got %d", counter)
	}
	var down wire.TerminalDown
	if err := json.Unmarshal(plain, &down); err != nil {
		t.Fatalf("unmarshal decrypted frame: %v", err)
	}
	if down.Type != "replay" || down.Columns != 80 || down.Rows != 24 {
		t.Fatalf("unexpected decrypted frame: %+v", down)
	}
}

func TestTerminalInputDispatchedWhenEncrypted(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	bin := testutil.WriteFakeCmux(t, fakeTerminalScript)
	s := New(&cmux.Client{Bin: bin}, nil)
	sessions, deviceID, secret := pairedSessions(t)
	s.SetSessions(sessions)

	const relayTok = "relay-secret"
	srv := httptest.NewServer(s.TrustedHandler(relayTok))
	defer srv.Close()

	c := wsConnectEncrypted(t, srv.URL, "/terminal/SURF1", relayTok, deviceID)
	defer c.Close()

	// Drain the initial encrypted replay frame.
	armReadDeadline(t, c)
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatal(err)
	}

	upBytes, err := json.Marshal(wire.TerminalUp{Type: "input", Text: "ls\r"})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := e2e.EncodeFrame(secret, e2e.DirDeviceToAgent, 0, upBytes)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if err := c.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatal(err)
	}

	waitForRPCLog(t, logPath, "mobile.terminal.input", "SURF1", "ls")
}

func TestTerminalInputAckedWhenEncrypted(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	bin := testutil.WriteFakeCmux(t, fakeTerminalScript)
	s := New(&cmux.Client{Bin: bin}, nil)
	sessions, deviceID, secret := pairedSessions(t)
	s.SetSessions(sessions)

	const relayTok = "relay-secret"
	srv := httptest.NewServer(s.TrustedHandler(relayTok))
	defer srv.Close()

	c := wsConnectEncrypted(t, srv.URL, "/terminal/SURF1", relayTok, deviceID)
	defer c.Close()

	// Drain the initial encrypted replay frame (agent->device counter 0).
	armReadDeadline(t, c)
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatal(err)
	}

	upBytes, err := json.Marshal(wire.TerminalUp{Type: "input", Text: "ls\r", Seq: 9})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := e2e.EncodeFrame(secret, e2e.DirDeviceToAgent, 0, upBytes)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if err := c.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatal(err)
	}

	armReadDeadline(t, c)
	_, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("expected an encrypted ack frame, got: %v", err)
	}
	_, plain, err := e2e.DecodeFrame(secret, e2e.DirAgentToDevice, raw)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	var ack wire.TerminalDown
	if err := json.Unmarshal(plain, &ack); err != nil {
		t.Fatalf("unmarshal decrypted ack: %v", err)
	}
	if ack.Type != "ack" || ack.Seq != 9 || !ack.Ok {
		t.Fatalf("unexpected ack frame: %+v", ack)
	}
}

func TestTerminalRejectsMissingDeviceIDWhenEncrypted(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, fakeTerminalScript)
	s := New(&cmux.Client{Bin: bin}, nil)
	sessions, _, _ := pairedSessions(t)
	s.SetSessions(sessions)

	const relayTok = "relay-secret"
	srv := httptest.NewServer(s.TrustedHandler(relayTok))
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/terminal/SURF1"
	h := http.Header{"X-Relay-Token": {relayTok}}
	_, resp, err := websocket.DefaultDialer.Dial(u, h)
	if err == nil {
		t.Fatal("expected dial to fail without X-Device-ID once encryption is enabled")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", resp)
	}
}

// dialTerminal is wsConnectEncrypted plus the handshake response, so a test can
// assert what the bridge answered about compression on the 101 itself.
func dialTerminal(t *testing.T, srvURL, path, relayTok, deviceID string) (*websocket.Conn, *http.Response) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srvURL, "http") + path
	h := http.Header{"X-Relay-Token": {relayTok}, "X-Device-ID": {deviceID}}
	c, resp, err := websocket.DefaultDialer.Dial(u, h)
	if err != nil {
		t.Fatalf("ws dial %s failed: %v", path, err)
	}
	return c, resp
}

func newEncryptedTerminalServer(t *testing.T, script string) (*httptest.Server, string, []byte) {
	t.Helper()
	t.Setenv("CMUX_FAKE_LOG", t.TempDir()+"/cmux.log")
	bin := testutil.WriteFakeCmux(t, script)
	s := New(&cmux.Client{Bin: bin}, nil)
	sessions, deviceID, secret := pairedSessions(t)
	s.SetSessions(sessions)
	srv := httptest.NewServer(s.TrustedHandler("relay-secret"))
	t.Cleanup(srv.Close)
	return srv, deviceID, secret
}

// readFrame decrypts one frame and, when compression was negotiated, strips
// the codec tag -- the app's side of wire.EncodePayload.
func readFrame(t *testing.T, c *websocket.Conn, secret []byte, counter uint64, deflate bool) (byte, wire.TerminalDown) {
	t.Helper()
	armReadDeadline(t, c)
	_, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	n, plain, err := e2e.DecodeFrame(secret, e2e.DirAgentToDevice, raw)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if n != counter {
		t.Fatalf("want frame counter %d, got %d", counter, n)
	}
	var tag byte
	if deflate {
		tag = plain[0]
		if plain, err = wire.DecodePayload(plain); err != nil {
			t.Fatalf("DecodePayload: %v", err)
		}
	}
	var down wire.TerminalDown
	if err := json.Unmarshal(plain, &down); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	return tag, down
}

func TestTerminalCompressesOnlyWhenTheClientAsks(t *testing.T) {
	srv, deviceID, secret := newEncryptedTerminalServer(t, fakeTerminalScript)

	c, resp := dialTerminal(t, srv.URL, "/terminal/SURF1", "relay-secret", deviceID)
	defer c.Close()
	if got := resp.Header.Get(deflateHeader); got != "" {
		t.Fatalf("bridge confirmed compression to a client that never asked: %q", got)
	}
	// Untagged: the payload must unmarshal straight from the sealed bytes.
	if _, down := readFrame(t, c, secret, 0, false); down.Type != "replay" {
		t.Fatalf("want an untagged replay frame, got %+v", down)
	}
}

func TestTerminalConfirmsCompressionAndTagsItsFrames(t *testing.T) {
	srv, deviceID, secret := newEncryptedTerminalServer(t, fakeTerminalScript)

	c, resp := dialTerminal(t, srv.URL, "/terminal/SURF1?deflate=1", "relay-secret", deviceID)
	defer c.Close()
	if got := resp.Header.Get(deflateHeader); got != "1" {
		t.Fatalf("bridge did not confirm compression on the 101: %q", got)
	}
	tag, down := readFrame(t, c, secret, 0, true)
	if down.Type != "replay" || down.Columns != 80 || down.Rows != 24 {
		t.Fatalf("unexpected frame through the codec: %+v", down)
	}
	if tag != wire.PayloadIdentity && tag != wire.PayloadDeflate {
		t.Fatalf("unknown codec tag %d", tag)
	}
}

// An ack is far too small for deflate to shrink, so it must go out as identity
// -- a negotiated stream must never make a frame bigger than it was.
func TestASmallFrameStaysUncompressedOnANegotiatedStream(t *testing.T) {
	srv, deviceID, secret := newEncryptedTerminalServer(t, fakeTerminalScript)

	c, _ := dialTerminal(t, srv.URL, "/terminal/SURF1?deflate=1", "relay-secret", deviceID)
	defer c.Close()
	readFrame(t, c, secret, 0, true) // drain the replay

	upBytes, err := json.Marshal(wire.TerminalUp{Type: "input", Text: "ls\r", Seq: 7})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := e2e.EncodeFrame(secret, e2e.DirDeviceToAgent, 0, upBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatal(err)
	}
	for counter := uint64(1); counter < 6; counter++ {
		tag, down := readFrame(t, c, secret, counter, true)
		if down.Type != "ack" {
			continue // an output frame raced in front of the ack
		}
		if down.Seq != 7 {
			t.Fatalf("want ack for seq 7, got %d", down.Seq)
		}
		if tag != wire.PayloadIdentity {
			t.Fatalf("an ack must not be deflated, got tag %d", tag)
		}
		return
	}
	t.Fatal("no ack frame arrived")
}

// A pane whose scrollback never changes but whose visible rows do -- the
// ordinary case for an agent printing output below a long history.
const fakeStickyScrollbackScript = `#!/bin/sh
printf '%s\n' "$*" >> "$CMUX_FAKE_LOG"
case "$2" in
  mobile.terminal.replay)
    n=$(grep -c 'mobile.terminal.replay' "$CMUX_FAKE_LOG")
    cat <<JSON
{"columns":80,"rows":24,"seq":0,"surface_id":"S","workspace_id":"W","render_grid":{"format":"cmux.render-grid.v1","columns":80,"rows":24,"scrollback_rows":2,"scrollback_spans":[{"row":0,"text":"history"}],"terminal_theme":{"bg":"#000"},"row_spans":[{"row":0,"text":"line-$n"}]}}
JSON
    ;;
  *) echo '{"ok":true}' ;;
esac
`

func TestOutputFramesOmitAnUnchangedScrollback(t *testing.T) {
	srv, deviceID, secret := newEncryptedTerminalServer(t, fakeStickyScrollbackScript)

	c, resp := dialTerminal(t, srv.URL, "/terminal/SURF1?delta=1", "relay-secret", deviceID)
	defer c.Close()
	if got := resp.Header.Get(deltaHeader); got != "1" {
		t.Fatalf("bridge did not confirm delta frames on the 101: %q", got)
	}

	// The replay must be whole: it is what a reconnecting app rebuilds from.
	_, replay := readFrame(t, c, secret, 0, false)
	if replay.Type != "replay" {
		t.Fatalf("want replay first, got %q", replay.Type)
	}
	if !strings.Contains(string(replay.Grid), "scrollback_spans") {
		t.Fatal("the replay frame must carry the scrollback in full")
	}
	if len(replay.Unchanged) != 0 {
		t.Fatalf("a replay frame must omit nothing, got %v", replay.Unchanged)
	}

	_, out := readFrame(t, c, secret, 1, false)
	if out.Type != "output" {
		t.Fatalf("want an output frame, got %q", out.Type)
	}
	if strings.Contains(string(out.Grid), "scrollback_spans") {
		t.Fatalf("unchanged scrollback was repeated: %s", out.Grid)
	}
	if strings.Contains(string(out.Grid), "terminal_theme") {
		t.Fatalf("unchanged theme was repeated: %s", out.Grid)
	}
	// The visible rows did change, so they must still be there.
	if !strings.Contains(string(out.Grid), "line-") {
		t.Fatalf("output frame lost its row spans: %s", out.Grid)
	}
	want := map[string]bool{"scrollback_spans": true, "terminal_theme": true}
	if len(out.Unchanged) != len(want) {
		t.Fatalf("want %d omitted blocks named, got %v", len(want), out.Unchanged)
	}
	for _, k := range out.Unchanged {
		if !want[k] {
			t.Fatalf("unexpected block named unchanged: %q", k)
		}
	}
}

// The regression that makes the handshake necessary: an app that never asked
// must keep getting whole grids, or its scrollback silently empties.
func TestOutputFramesStayWholeWithoutTheDeltaHandshake(t *testing.T) {
	srv, deviceID, secret := newEncryptedTerminalServer(t, fakeStickyScrollbackScript)

	c, resp := dialTerminal(t, srv.URL, "/terminal/SURF1", "relay-secret", deviceID)
	defer c.Close()
	if got := resp.Header.Get(deltaHeader); got != "" {
		t.Fatalf("bridge confirmed delta frames to a client that never asked: %q", got)
	}
	readFrame(t, c, secret, 0, false) // replay

	_, out := readFrame(t, c, secret, 1, false)
	if out.Type != "output" {
		t.Fatalf("want an output frame, got %q", out.Type)
	}
	if !strings.Contains(string(out.Grid), "scrollback_spans") {
		t.Fatalf("an un-negotiated client lost its scrollback: %s", out.Grid)
	}
	if len(out.Unchanged) != 0 {
		t.Fatalf("an un-negotiated client must never be told about omissions, got %v", out.Unchanged)
	}
}

// This test used to assert that styles and modes could never be sticky,
// because they "decide what is on screen now". That reason was wrong: strip
// omits a block only while its bytes are unchanged, so a client carrying them
// forward holds what the bridge holds, and cmux-app-8wk made both sticky for
// 28% of a compressed frame. What is left is the part that was right.
//
// row_spans and cursor stay out because stickiness cannot pay there -- they
// differ on nearly every frame, so the comparison would run each time and
// almost never save anything.
func TestTheBlocksThatChangeEveryFrameAreNeverSticky(t *testing.T) {
	for _, field := range []string{"row_spans", "cursor"} {
		if slices.Contains(stickyGridFields, field) {
			t.Errorf("%q changes on nearly every frame; making it sticky costs more than it saves", field)
		}
	}
}

// Wire-format lockstep: every name the bridge can put in `unchanged` needs a
// branch in the app's RenderGrid.mergedOnto, or the app decodes the absent
// block as empty and silently loses it -- a blank scrollback, an unstyled
// screen, or arrow keys that stop matching the pane's mode.
//
// Go cannot see the Kotlin, so this pins the list instead. Adding a block here
// without adding UnchangedBlock.X and a mergedOnto branch in the same commit
// fails this test, which is the reminder.
func TestTheStickyListMatchesWhatTheAppCanCarry(t *testing.T) {
	appCanCarry := []string{
		"scrollback_spans",      // UnchangedBlock.SCROLLBACK_SPANS
		"terminal_theme",        // not modelled by the app at all, so safe to drop
		"terminal_config_theme", // likewise
		"styles",                // UnchangedBlock.STYLES
		"modes",                 // UnchangedBlock.MODES
	}
	for _, sticky := range stickyGridFields {
		if !slices.Contains(appCanCarry, sticky) {
			t.Errorf("%q is omitted by the bridge but the app has no way to carry it forward", sticky)
		}
	}
	for _, known := range appCanCarry {
		if !slices.Contains(stickyGridFields, known) {
			t.Errorf("%q is listed here but no longer sticky -- update this list with the change", known)
		}
	}
}

func TestTheDeltaEncoderRepeatsABlockThatChanged(t *testing.T) {
	d := newDeltaEncoder()
	first := json.RawMessage(`{"scrollback_spans":[{"row":0,"text":"a"}],"row_spans":[]}`)
	if _, omitted := d.strip(first); len(omitted) != 0 {
		t.Fatalf("nothing can be omitted from the first frame, got %v", omitted)
	}
	// Same scrollback -> omitted.
	if _, omitted := d.strip(first); len(omitted) != 1 || omitted[0] != "scrollback_spans" {
		t.Fatalf("want scrollback omitted on a repeat, got %v", omitted)
	}
	// Changed scrollback -> sent again, and remembered at its new value.
	grown := json.RawMessage(`{"scrollback_spans":[{"row":0,"text":"a"},{"row":1,"text":"b"}],"row_spans":[]}`)
	got, omitted := d.strip(grown)
	if len(omitted) != 0 {
		t.Fatalf("a changed scrollback must be re-sent, got %v", omitted)
	}
	if !strings.Contains(string(got), `"b"`) {
		t.Fatalf("the changed scrollback is missing from the frame: %s", got)
	}
	if _, omitted := d.strip(grown); len(omitted) != 1 {
		t.Fatalf("the new value must now be the one remembered, got %v", omitted)
	}
}

// styles and modes were deliberately excluded from stickyGridFields until
// cmux-app-8wk on the theory that a carried-over copy could go stale. It cannot
// -- strip omits a block only while its bytes are unchanged -- and together
// they are 28% of a compressed frame.
func TestStylesAndModesAreCarriedLikeAnyOtherStickyBlock(t *testing.T) {
	d := newDeltaEncoder()
	grid := json.RawMessage(`{"styles":[{"id":1}],"modes":[{"code":1,"on":true}],"row_spans":[]}`)
	if _, omitted := d.strip(grid); len(omitted) != 0 {
		t.Fatalf("nothing can be omitted from the first frame, got %v", omitted)
	}
	got, omitted := d.strip(grid)
	if len(omitted) != 2 {
		t.Fatalf("want both styles and modes omitted on a repeat, got %v", omitted)
	}
	for _, want := range []string{"styles", "modes"} {
		if !slices.Contains(omitted, want) {
			t.Errorf("%s was not omitted: %v", want, omitted)
		}
		if strings.Contains(string(got), `"`+want+`"`) {
			t.Errorf("%s was named unchanged but still sent: %s", want, got)
		}
	}
}

// The property that makes carrying them safe: the moment either changes, it is
// in the frame again. A pane that leaves application-cursor mode must not have
// the app spelling arrows against the old modes.
func TestAChangedModeIsSentAgainRatherThanCarried(t *testing.T) {
	d := newDeltaEncoder()
	on := json.RawMessage(`{"modes":[{"code":1,"on":true}],"row_spans":[]}`)
	d.strip(on)
	if _, omitted := d.strip(on); len(omitted) != 1 {
		t.Fatalf("an unchanged mode set should be omitted, got %v", omitted)
	}
	off := json.RawMessage(`{"modes":[{"code":1,"on":false}],"row_spans":[]}`)
	got, omitted := d.strip(off)
	if len(omitted) != 0 {
		t.Fatalf("a changed mode set must be re-sent, got %v", omitted)
	}
	if !strings.Contains(string(got), `"on":false`) {
		t.Fatalf("the new mode value is missing from the frame: %s", got)
	}
}

func TestTheDeltaEncoderPassesAnUndecodableGridThrough(t *testing.T) {
	d := newDeltaEncoder()
	bad := json.RawMessage(`not json`)
	got, omitted := d.strip(bad)
	if !bytes.Equal(got, bad) || omitted != nil {
		t.Fatalf("an undecodable grid must pass through whole, got %s / %v", got, omitted)
	}
}

// terminalStream decodes a socket's frames the way the app does: one decoder
// fed every chunk in arrival order. Re-inflating from the start each time is
// test-only laziness; the app keeps a single Inflater primed instead.
type terminalStream struct {
	raw  []byte
	seen int
}

func (s *terminalStream) decode(t *testing.T, payload []byte) wire.TerminalDown {
	t.Helper()
	if len(payload) < 1 || payload[0] != wire.PayloadDeflateStream {
		t.Fatalf("want a stream-tagged payload, got tag %v", payload)
	}
	s.raw = append(s.raw, payload[1:]...)
	r := flate.NewReader(bytes.NewReader(s.raw))
	defer func() { _ = r.Close() }()
	all, err := io.ReadAll(r)
	// Every chunk ends on a sync flush rather than a final block, so running
	// out of input is the normal end of one.
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("inflate: %v", err)
	}
	var down wire.TerminalDown
	if err := json.Unmarshal(all[s.seen:], &down); err != nil {
		t.Fatalf("unmarshal streamed frame: %v", err)
	}
	s.seen = len(all)
	return down
}

// readSealed returns the codec-tagged payload without decoding it, for tests
// that need to see the tag and drive their own decoder.
func readSealed(t *testing.T, c *websocket.Conn, secret []byte, counter uint64) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read frame %d: %v", counter, err)
	}
	n, plain, err := e2e.DecodeFrame(secret, e2e.DirAgentToDevice, raw)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if n != counter {
		t.Fatalf("want frame counter %d, got %d", counter, n)
	}
	return plain
}

func TestTerminalCompressesAgainstOneWindowWhenTheClientAsks(t *testing.T) {
	srv, deviceID, secret := newEncryptedTerminalServer(t, fakeStickyScrollbackScript)

	c, resp := dialTerminal(t, srv.URL, "/terminal/SURF1?deflate=1&stream=1", "relay-secret", deviceID)
	defer c.Close()
	if got := resp.Header.Get(streamHeader); got != "1" {
		t.Fatalf("bridge did not confirm the shared window on the 101: %q", got)
	}

	var dec terminalStream
	if replay := dec.decode(t, readSealed(t, c, secret, 0)); replay.Type != "replay" {
		t.Fatalf("want replay first, got %q", replay.Type)
	}
	// The frame that matters: it is only readable because the replay before it
	// primed the same decoder.
	if out := dec.decode(t, readSealed(t, c, secret, 1)); out.Type != "output" {
		t.Fatalf("want an output frame, got %q", out.Type)
	}
}

// The saving, end to end. A pane whose frames barely differ must cost a
// fraction of a frame once the window has warmed -- if it does not, the
// encoder is not surviving between writes.
func TestASettledPaneCostsAFractionOfAFramePerUpdate(t *testing.T) {
	srv, deviceID, secret := newEncryptedTerminalServer(t, fakeStickyScrollbackScript)

	c, _ := dialTerminal(t, srv.URL, "/terminal/SURF1?deflate=1&stream=1", "relay-secret", deviceID)
	defer c.Close()
	var dec terminalStream
	replay := readSealed(t, c, secret, 0)
	dec.decode(t, replay)

	var settled int
	for counter := uint64(1); counter <= 4; counter++ {
		chunk := readSealed(t, c, secret, counter)
		dec.decode(t, chunk)
		settled = len(chunk)
	}
	if settled*4 > len(replay) {
		t.Fatalf("a settled frame cost %d B against a %d B replay -- the window is not being reused", settled, len(replay))
	}
}

// Streaming has to be asked for on its own. An app that negotiated only
// per-frame deflate cannot read chunks of a stream, so ?deflate=1 alone must
// never produce them.
func TestTheSharedWindowIsNotUsedWithoutItsOwnHandshake(t *testing.T) {
	for _, query := range []string{"?deflate=1", "?stream=1", ""} {
		t.Run(query, func(t *testing.T) {
			srv, deviceID, secret := newEncryptedTerminalServer(t, fakeTerminalScript)

			c, resp := dialTerminal(t, srv.URL, "/terminal/SURF1"+query, "relay-secret", deviceID)
			defer c.Close()
			if got := resp.Header.Get(streamHeader); got != "" {
				t.Fatalf("bridge confirmed a shared window nobody asked for: %q", got)
			}
			plain := readSealed(t, c, secret, 0)
			if query == "?deflate=1" && plain[0] == wire.PayloadDeflateStream {
				t.Fatal("a deflate-only client was sent a chunk of a stream")
			}
		})
	}
}

// The invariant the shared window rests on: EVERY frame goes through the
// stream, including the acks that per-frame deflate deliberately skips. An ack
// sent standalone would be readable on its own, so nothing would look broken --
// but it would leave the app's window a frame behind the bridge's, and the next
// real frame would desync. This exists to make that tempting optimisation fail
// loudly.
func TestEvenAnAckStaysOnTheSharedWindow(t *testing.T) {
	srv, deviceID, secret := newEncryptedTerminalServer(t, fakeTerminalScript)

	c, _ := dialTerminal(t, srv.URL, "/terminal/SURF1?deflate=1&stream=1", "relay-secret", deviceID)
	defer c.Close()
	var dec terminalStream
	dec.decode(t, readSealed(t, c, secret, 0)) // drain the replay

	upBytes, err := json.Marshal(wire.TerminalUp{Type: "input", Text: "ls\r", Seq: 7})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := e2e.EncodeFrame(secret, e2e.DirDeviceToAgent, 0, upBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatal(err)
	}
	for counter := uint64(1); counter < 6; counter++ {
		plain := readSealed(t, c, secret, counter)
		if plain[0] != wire.PayloadDeflateStream {
			t.Fatalf("frame %d left the shared window with tag %d", counter, plain[0])
		}
		if down := dec.decode(t, plain); down.Type == "ack" {
			if down.Seq != 7 {
				t.Fatalf("want ack for seq 7, got %d", down.Seq)
			}
			return
		}
	}
	t.Fatal("no ack frame arrived")
}

// What cmux actually does on a scrolling pane, measured live (cmux-app-bly):
// the same two styles come back with different ids on every replay, so every
// span's bytes change while its text does not. The visible row changes each
// call so a frame is sent; the scrollback and styles are content-identical.
const fakeRenumberingScript = `#!/bin/sh
printf '%s\n' "$*" >> "$CMUX_FAKE_LOG"
case "$2" in
  mobile.terminal.replay)
    n=$(grep -c 'mobile.terminal.replay' "$CMUX_FAKE_LOG")
    if [ $((n % 2)) -eq 0 ]; then a=1; b=2; else a=2; b=1; fi
    cat <<JSON
{"columns":80,"rows":24,"seq":0,"surface_id":"S","workspace_id":"W","render_grid":{"format":"cmux.render-grid.v1","columns":80,"rows":24,"render_epoch":"E","scrollback_rows":2,"cleared_rows":[],"scrolled_rows":0,"anchor":"viewport","active_screen":"primary","styles":[{"id":0,"foreground":"#fff","background":"#000"},{"id":$a,"foreground":"#aaa"},{"id":$b,"foreground":"#bbb"}],"scrollback_spans":[{"row":0,"column":0,"style_id":$a,"text":"history","cell_width":1}],"row_spans":[{"row":0,"column":0,"style_id":$b,"text":"line-$n","cell_width":1}]}}
JSON
    ;;
  *) echo '{"ok":true}' ;;
esac
`

// The acceptance test for cmux-app-bly's first commit. Before it, a renumbered
// table made unchanged=[] on every frame -- the delta encoder saw different
// bytes in blocks whose content had not moved.
func TestARenumberedStyleTableNoLongerDefeatsTheDelta(t *testing.T) {
	srv, deviceID, secret := newEncryptedTerminalServer(t, fakeRenumberingScript)

	c, _ := dialTerminal(t, srv.URL, "/terminal/SURF1?delta=1", "relay-secret", deviceID)
	defer c.Close()
	_, replay := readFrame(t, c, secret, 0, false)
	if replay.Type != "replay" {
		t.Fatalf("want replay first, got %q", replay.Type)
	}

	_, out := readFrame(t, c, secret, 1, false)
	if out.Type != "output" {
		t.Fatalf("want an output frame, got %q", out.Type)
	}
	for _, block := range []string{"scrollback_spans", "styles"} {
		if !slices.Contains(out.Unchanged, block) {
			t.Fatalf("%s did not change in content and must be omitted; unchanged=%v grid=%s", block, out.Unchanged, out.Grid)
		}
	}
	// The row that did change still points at a style the carried table has.
	if !strings.Contains(string(out.Grid), `"style_id":1`) && !strings.Contains(string(out.Grid), `"style_id":2`) {
		t.Fatalf("output row lost its style: %s", out.Grid)
	}
}

// The app only ever sees ids the frame's own table (or the carried one)
// defines, so canonicalisation cannot leave a span dangling.
func TestEveryStyleIdASpanUsesIsInTheTableItArrivedWith(t *testing.T) {
	srv, deviceID, secret := newEncryptedTerminalServer(t, fakeRenumberingScript)

	c, _ := dialTerminal(t, srv.URL, "/terminal/SURF1", "relay-secret", deviceID)
	defer c.Close()
	for counter := range uint64(3) {
		_, down := readFrame(t, c, secret, counter, false)
		var g struct {
			Styles []struct {
				ID int `json:"id"`
			} `json:"styles"`
			Rows []struct {
				StyleID int `json:"style_id"`
			} `json:"row_spans"`
			Back []struct {
				StyleID int `json:"style_id"`
			} `json:"scrollback_spans"`
		}
		if err := json.Unmarshal(down.Grid, &g); err != nil {
			t.Fatal(err)
		}
		have := map[int]bool{}
		for _, s := range g.Styles {
			have[s.ID] = true
		}
		for _, s := range append(g.Rows, g.Back...) {
			if !have[s.StyleID] {
				t.Fatalf("frame %d: span uses style %d, table has %v", counter, s.StyleID, have)
			}
		}
	}
}
