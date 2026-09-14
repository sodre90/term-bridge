package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/sodre90/term-bridge/internal/cmux"
	"github.com/sodre90/term-bridge/internal/testutil"
	"github.com/sodre90/term-bridge/internal/wire"
)

func neverPaired(string) bool { return false }

func pairedExcept(unpaired ...string) func(string) bool {
	gone := map[string]bool{}
	for _, id := range unpaired {
		gone[id] = true
	}
	return func(deviceID string) bool { return !gone[deviceID] }
}

func TestSweepClosesASocketWhoseDeviceWasUnpaired(t *testing.T) {
	tracker := newSocketTracker()
	tornDown := false
	tracker.track("device-gone", func() { tornDown = true })

	if closed := tracker.closeUnpaired(pairedExcept("device-gone")); closed != 1 {
		t.Fatalf("closed %d sockets, want 1", closed)
	}
	if !tornDown {
		t.Fatal("an unpaired device's socket must be torn down, not just forgotten")
	}
}

func TestSweepLeavesAStillPairedDeviceAlone(t *testing.T) {
	tracker := newSocketTracker()
	keptTornDown := false
	revokedTornDown := false
	tracker.track("device-live", func() { keptTornDown = true })
	tracker.track("device-gone", func() { revokedTornDown = true })

	tracker.closeUnpaired(pairedExcept("device-gone"))

	if keptTornDown {
		t.Fatal("unpairing one device must not tear down another device's socket")
	}
	if !revokedTornDown {
		t.Fatal("the unpaired device's socket should have been torn down")
	}
}

// Without this the map would grow with every terminal and events connection
// the app has ever opened.
func TestReleasingASocketDeregistersIt(t *testing.T) {
	tracker := newSocketTracker()
	tornDown := false
	release := tracker.track("device-a", func() { tornDown = true })

	release()

	tracker.mu.Lock()
	remaining := len(tracker.sockets)
	tracker.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("tracker still holds %d device entries after release", remaining)
	}
	if closed := tracker.closeUnpaired(pairedExcept("device-a")); closed != 0 {
		t.Fatalf("a released socket must not be swept, got %d", closed)
	}
	if tornDown {
		t.Fatal("a socket that ended on its own must not be torn down afterwards")
	}
}

func TestSweepDoesNotTearDownASocketTwice(t *testing.T) {
	tracker := newSocketTracker()
	teardowns := 0
	tracker.track("device-gone", func() { teardowns++ })

	tracker.closeUnpaired(pairedExcept("device-gone"))
	tracker.closeUnpaired(pairedExcept("device-gone"))

	if teardowns != 1 {
		t.Fatalf("teardown ran %d times, want 1", teardowns)
	}
}

// One device may hold a terminal and an events socket at once, and revocation
// has to reach both.
func TestSweepClosesEverySocketOfOneDevice(t *testing.T) {
	tracker := newSocketTracker()
	teardowns := 0
	tracker.track("device-gone", func() { teardowns++ })
	tracker.track("device-gone", func() { teardowns++ })

	if closed := tracker.closeUnpaired(pairedExcept("device-gone")); closed != 2 {
		t.Fatalf("closed %d sockets, want 2", closed)
	}
	if teardowns != 2 {
		t.Fatalf("teardown ran %d times, want 2", teardowns)
	}
}

func TestSweepDisconnectsAnUnpairedDevicesTerminal(t *testing.T) {
	t.Setenv("CMUX_FAKE_LOG", t.TempDir()+"/cmux.log")
	bin := testutil.WriteFakeCmux(t, fakeTerminalScript)
	s := New(&cmux.Client{Bin: bin}, nil)
	sessions, deviceID, _ := pairedSessions(t)
	s.SetSessions(sessions)

	const relayTok = "relay-secret"
	srv := httptest.NewServer(s.TrustedHandler(relayTok))
	defer srv.Close()

	c := wsConnectEncrypted(t, srv.URL, "/terminal/SURF1", relayTok, deviceID)
	defer c.Close()
	armReadDeadline(t, c)
	if _, _, err := c.ReadMessage(); err != nil { // replay frame: the handler is past registration
		t.Fatalf("initial replay: %v", err)
	}

	if closed := s.sockets.closeUnpaired(neverPaired); closed != 1 {
		t.Fatalf("closed %d sockets, want the live terminal", closed)
	}
	armReadDeadline(t, c)
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("the terminal socket should have been closed under the client")
	}
}

func TestSweepDisconnectsAnUnpairedDevicesEventStream(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, "#!/bin/sh\necho '{}'\n")
	s := New(&cmux.Client{Bin: bin}, nil)
	sessions, deviceID, _ := pairedSessions(t)
	s.SetSessions(sessions)

	const relayTok = "relay-secret"
	srv := httptest.NewServer(s.TrustedHandler(relayTok))
	defer srv.Close()

	c := wsDialEncrypted(t, srv.URL, relayTok, deviceID)
	defer c.Close()
	waitForTrackedSockets(t, s.sockets, 1)

	if closed := s.sockets.closeUnpaired(neverPaired); closed != 1 {
		t.Fatalf("closed %d sockets, want the live event stream", closed)
	}
	armReadDeadline(t, c)
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("the event stream should have been closed under the client")
	}
}

// The relay's own push-monitor subscription carries no X-Device-ID, so no
// device revocation may reach it -- tearing it down would kill FCM fan-out for
// every tenant on the relay.
func TestSweepNeverTouchesTheRelaysPushSubscription(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, "#!/bin/sh\necho '{}'\n")
	s := New(&cmux.Client{Bin: bin}, nil)
	sessions, _, _ := pairedSessions(t)
	s.SetSessions(sessions)

	const relayTok = "relay-secret"
	srv := httptest.NewServer(s.TrustedHandler(relayTok))
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/events"
	c, _, err := websocket.DefaultDialer.Dial(u, http.Header{"X-Relay-Token": {relayTok}})
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer c.Close()
	waitForSubscribers(t, s.hub, 1)

	if closed := s.sockets.closeUnpaired(neverPaired); closed != 0 {
		t.Fatalf("the relay's push subscription must not be tracked, %d closed", closed)
	}
	s.hub.broadcast(wire.EventFrame{Type: "feed", FeedID: "X"})
	armReadDeadline(t, c)
	var got wire.EventFrame
	if err := c.ReadJSON(&got); err != nil {
		t.Fatalf("push subscription should still be delivering frames: %v", err)
	}
	if got.FeedID != "X" {
		t.Fatalf("unexpected frame: %+v", got)
	}
}
