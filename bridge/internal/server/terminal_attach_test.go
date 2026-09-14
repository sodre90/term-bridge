package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sodre90/term-bridge/internal/cmux"
	"github.com/sodre90/term-bridge/internal/e2e"
	"github.com/sodre90/term-bridge/internal/testutil"
	"github.com/sodre90/term-bridge/internal/wire"
)

// attachOverPlaintextSocket opens a terminal socket on a server with an
// attachment store in dir, drains the replay, sends one attach frame and
// returns the ack for it. The fake cmux log is at logPath.
func attachOverPlaintextSocket(t *testing.T, dir string, up wire.TerminalUp) (ack wire.TerminalDown, logPath string) {
	t.Helper()
	logPath = t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeTerminalScript)
	if dir != "" {
		s.SetAttachmentStore(NewAttachmentStore(dir))
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsConnect(t, srv.URL, "/terminal/SURF1", tok)
	defer c.Close()

	armReadDeadline(t, c)
	var replay wire.TerminalDown
	if err := c.ReadJSON(&replay); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteJSON(up); err != nil {
		t.Fatal(err)
	}
	armReadDeadline(t, c)
	if err := c.ReadJSON(&ack); err != nil {
		t.Fatalf("expected an ack frame, got: %v", err)
	}
	return ack, logPath
}

func TestAnAttachedImageLandsOnDiskAndItsPathIsPastedThenAcked(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "attachments")
	ack, logPath := attachOverPlaintextSocket(t, dir, wire.TerminalUp{
		Type:  "attach",
		Seq:   9,
		Image: base64.StdEncoding.EncodeToString(imageSignatures["png"]),
		Name:  "Screenshot_2026",
	})
	if ack.Type != "ack" || ack.Seq != 9 || !ack.Ok {
		t.Fatalf("unexpected ack: %+v", ack)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("want one landed file, got %d (%v)", len(entries), err)
	}
	landed := filepath.Join(dir, entries[0].Name())
	if !strings.HasSuffix(landed, ".png") {
		t.Fatalf("png bytes landed as %s", landed)
	}
	// The paste is the path followed by one space, and nothing else was
	// pasted or typed.
	waitForRPCLog(t, logPath, "mobile.terminal.paste", "SURF1", landed+" ")
	log, _ := os.ReadFile(logPath)
	if strings.Count(string(log), "mobile.terminal.paste") != 1 {
		t.Fatalf("want exactly one paste, log:\n%s", log)
	}
	if strings.Contains(string(log), "mobile.terminal.input") {
		t.Fatalf("an attach must not type anything, log:\n%s", log)
	}
}

func TestARefusedAttachmentPastesNothingAndAcksFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "attachments")
	ack, logPath := attachOverPlaintextSocket(t, dir, wire.TerminalUp{
		Type:  "attach",
		Seq:   10,
		Image: base64.StdEncoding.EncodeToString([]byte("not an image at all")),
	})
	if ack.Type != "ack" || ack.Seq != 10 || ack.Ok || ack.Reason != "not_image" {
		t.Fatalf("want a failed ack saying not_image, got %+v", ack)
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "mobile.terminal.paste") {
		t.Fatalf("a refused attachment must paste nothing, log:\n%s", log)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("a refused attachment must not create the directory")
	}
}

func TestBadBase64IsARefusedAttachmentNotAClosedSocket(t *testing.T) {
	ack, _ := attachOverPlaintextSocket(t, filepath.Join(t.TempDir(), "a"), wire.TerminalUp{
		Type: "attach", Seq: 11, Image: "%%%not base64%%%",
	})
	if ack.Type != "ack" || ack.Seq != 11 || ack.Ok || ack.Reason != "bad_encoding" {
		t.Fatalf("want a failed ack saying bad_encoding, got %+v", ack)
	}
}

// Over the cap but under the read limit: the frame arrives whole and is
// refused for its size before any of it is decoded.
func TestAnAttachOverTheCapIsRefusedAsTooLarge(t *testing.T) {
	ack, _ := attachOverPlaintextSocket(t, filepath.Join(t.TempDir(), "a"), wire.TerminalUp{
		Type: "attach", Seq: 14, Image: strings.Repeat("A", base64.StdEncoding.EncodedLen(attachmentMaxBytes)+4),
	})
	if ack.Type != "ack" || ack.Seq != 14 || ack.Ok || ack.Reason != "too_large" {
		t.Fatalf("want a failed ack saying too_large, got %+v", ack)
	}
}

func TestWithoutAStoreAnAttachIsRefusedNotCrashed(t *testing.T) {
	ack, logPath := attachOverPlaintextSocket(t, "", wire.TerminalUp{
		Type: "attach", Seq: 12, Image: base64.StdEncoding.EncodeToString(imageSignatures["jpg"]),
	})
	if ack.Type != "ack" || ack.Seq != 12 || ack.Ok || ack.Reason != "attachments_off" {
		t.Fatalf("want a failed ack saying attachments_off, got %+v", ack)
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "mobile.terminal.paste") {
		t.Fatal("nothing may be pasted without a store")
	}
}

// The phone's name hint is logged and nothing more: a hostile one cannot
// steer where the file lands.
func TestTheNameHintCannotSteerThePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "attachments")
	ack, _ := attachOverPlaintextSocket(t, dir, wire.TerminalUp{
		Type:  "attach",
		Seq:   13,
		Image: base64.StdEncoding.EncodeToString(imageSignatures["jpg"]),
		Name:  "../../../../tmp/evil.sh",
	})
	if !ack.Ok || ack.Reason != "" {
		t.Fatalf("want a plain success, got %+v", ack)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("want one file in the store's own directory, got %d (%v)", len(entries), err)
	}
	if strings.Contains(entries[0].Name(), "evil") || !strings.HasSuffix(entries[0].Name(), ".jpg") {
		t.Fatalf("the hint leaked into the name: %s", entries[0].Name())
	}
}

// A paste, like an input, changes the PTY, so the poll loop must be nudged
// for an immediate replay rather than leaving the user to wait a tick. The
// fake script's replay output changes on every call, so a nudge shows up as
// a second output frame arriving well inside the poll interval.
func TestAPasteNudgesAnImmediateReplay(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeChangingTerminalScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsConnect(t, srv.URL, "/terminal/SURF1?poll_ms=10000", tok)
	defer c.Close()

	armReadDeadline(t, c)
	var replay wire.TerminalDown
	if err := c.ReadJSON(&replay); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteJSON(wire.TerminalUp{Type: "paste", Text: "hello", Seq: 1}); err != nil {
		t.Fatal(err)
	}
	// Well inside the 10s interval: only a nudge gets a frame here.
	sawOutput := false
	for i := 0; i < 2 && !sawOutput; i++ {
		if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var down wire.TerminalDown
		if err := c.ReadJSON(&down); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		sawOutput = down.Type != "ack"
	}
	if !sawOutput {
		t.Fatal("a paste must trigger a replay well inside a 10s poll interval")
	}
}

// The only test that sends an attach the way production does: e2e-sealed,
// at a realistic size. Every other attach test is plaintext and sixteen
// bytes; this one is what shows a 500 KB frame survives decrypt, JSON and
// base64 and comes out as one paste.
func TestAHalfMegabyteAttachSurvivesTheEncryptedPath(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	bin := testutil.WriteFakeCmux(t, fakeTerminalScript)
	s := New(&cmux.Client{Bin: bin}, nil)
	sessions, deviceID, secret := pairedSessions(t)
	s.SetSessions(sessions)
	dir := filepath.Join(t.TempDir(), "attachments")
	s.SetAttachmentStore(NewAttachmentStore(dir))
	const relayTok = "relay-secret"
	srv := httptest.NewServer(s.TrustedHandler(relayTok))
	defer srv.Close()

	c := wsConnectEncrypted(t, srv.URL, "/terminal/SURF1", relayTok, deviceID)
	defer c.Close()
	armReadDeadline(t, c)
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatal(err)
	}

	image := append(bytes.Clone(imageSignatures["jpg"]), make([]byte, 500<<10)...)
	upBytes, err := json.Marshal(wire.TerminalUp{Type: "attach", Seq: 1, Image: base64.StdEncoding.EncodeToString(image)})
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

	_, ack := readFrame(t, c, secret, 1, false)
	if ack.Type != "ack" || ack.Seq != 1 || !ack.Ok {
		t.Fatalf("unexpected ack: %+v", ack)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("want one landed file, got %d (%v)", len(entries), err)
	}
	landed, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil || !bytes.Equal(landed, image) {
		t.Fatalf("landed bytes differ from what was sent (%d vs %d)", len(landed), len(image))
	}
	waitForRPCLog(t, logPath, "mobile.terminal.paste", filepath.Join(dir, entries[0].Name())+" ")
}

// The read limit is what bounds memory: a message over it is never buffered
// whole, the socket ends, and nothing reaches cmux. A legitimate attach at
// the cap still fits, or the limit would be a cap in disguise.
func TestAMessageOverTheReadLimitEndsTheSocketWithNothingPasted(t *testing.T) {
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeTerminalScript)
	s.SetAttachmentStore(NewAttachmentStore(filepath.Join(t.TempDir(), "attachments")))
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	c := wsConnect(t, srv.URL, "/terminal/SURF1", tok)
	defer c.Close()
	armReadDeadline(t, c)
	var replay wire.TerminalDown
	if err := c.ReadJSON(&replay); err != nil {
		t.Fatal(err)
	}

	oversize := wire.TerminalUp{Type: "attach", Seq: 1, Image: strings.Repeat("A", terminalUpReadLimit)}
	if err := c.WriteJSON(oversize); err != nil {
		t.Fatal(err)
	}
	armReadDeadline(t, c)
	var down wire.TerminalDown
	err := c.ReadJSON(&down)
	if err == nil {
		t.Fatalf("the socket must end, got a frame instead: %+v", down)
	}
	if _, isClose := err.(*websocket.CloseError); !isClose && !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "reset") {
		t.Fatalf("want the socket closed, got %v", err)
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "mobile.terminal.paste") {
		t.Fatalf("nothing may be pasted, log:\n%s", log)
	}
}

func TestAnAttachAtTheCapFitsUnderTheReadLimit(t *testing.T) {
	image := append(bytes.Clone(imageSignatures["jpg"]), make([]byte, attachmentMaxBytes-len(imageSignatures["jpg"]))...)
	frame, err := json.Marshal(wire.TerminalUp{Type: "attach", Seq: 1, Image: base64.StdEncoding.EncodeToString(image), Name: "IMG_2041.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) > terminalUpReadLimit {
		t.Fatalf("a maximal attach is %d bytes, over the %d read limit", len(frame), terminalUpReadLimit)
	}
}
