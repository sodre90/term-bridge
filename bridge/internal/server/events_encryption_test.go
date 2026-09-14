package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/sodre90/term-bridge/internal/cmux"
	"github.com/sodre90/term-bridge/internal/e2e"
	"github.com/sodre90/term-bridge/internal/testutil"
	"github.com/sodre90/term-bridge/internal/wire"
)

func wsDialEncrypted(t *testing.T, srvURL, relayTok, deviceID string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srvURL, "http") + "/events"
	h := http.Header{"X-Relay-Token": {relayTok}, "X-Device-ID": {deviceID}}
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

func TestEventsBroadcastEncryptedWhenSessionsSet(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, "#!/bin/sh\necho '{}'\n")
	s := New(&cmux.Client{Bin: bin}, nil)
	sessions, deviceID, secret := pairedSessions(t)
	s.SetSessions(sessions)

	const relayTok = "relay-secret"
	srv := httptest.NewServer(s.TrustedHandler(relayTok))
	defer srv.Close()

	c := wsDialEncrypted(t, srv.URL, relayTok, deviceID)
	defer c.Close()
	waitForSubscribers(t, s.hub, 1)

	s.hub.broadcast(wire.EventFrame{Type: "feed", FeedID: "X", NeedsAttention: true})

	armReadDeadline(t, c)
	msgType, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if msgType != websocket.BinaryMessage {
		t.Fatalf("want binary (encrypted) frame, got type %d", msgType)
	}
	counter, plain, err := e2e.DecodeFrame(secret, e2e.DirAgentToDevice, raw)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if counter != 0 {
		t.Fatalf("want counter 0, got %d", counter)
	}
	var got wire.EventFrame
	if err := json.Unmarshal(plain, &got); err != nil {
		t.Fatalf("unmarshal decrypted frame: %v", err)
	}
	if got.FeedID != "X" || !got.NeedsAttention {
		t.Fatalf("unexpected decrypted frame: %+v", got)
	}
}

// TestEventsServesPlaintextForRelayPushWhenSessionsSet proves that a caller
// identified only by X-Relay-Token (no X-Device-ID) -- the relay's own
// push-monitor subscription, internal/relay/pushmon.go -- still receives
// plaintext JSON frames even when e2e encryption is enabled for devices.
// Without this, enabling encryption silently breaks FCM push forever.
func TestEventsServesPlaintextForRelayPushWhenSessionsSet(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, "#!/bin/sh\necho '{}'\n")
	s := New(&cmux.Client{Bin: bin}, nil)
	sessions, _, _ := pairedSessions(t)
	s.SetSessions(sessions)

	const relayTok = "relay-secret"
	srv := httptest.NewServer(s.TrustedHandler(relayTok))
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/events"
	h := http.Header{"X-Relay-Token": {relayTok}}
	c, resp, err := websocket.DefaultDialer.Dial(u, h)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("ws dial failed (status %d): %v", code, err)
	}
	defer c.Close()
	waitForSubscribers(t, s.hub, 1)

	s.hub.broadcast(wire.EventFrame{Type: "feed", FeedID: "X", NeedsAttention: true})

	armReadDeadline(t, c)
	var got wire.EventFrame
	if err := c.ReadJSON(&got); err != nil {
		t.Fatalf("expected a plaintext JSON frame for the relay's push subscription, got: %v", err)
	}
	if got.FeedID != "X" || !got.NeedsAttention {
		t.Fatalf("unexpected frame: %+v", got)
	}
}

// TestEventsRedactsContentForRelayPushWhenSessionsSet proves the relay's own
// push-monitor subscription (X-Relay-Token, no X-Device-ID) never receives a
// tenant's workspace name or live status text -- a blind relay must not
// learn tenant content even though it needs NeedsAttention/Kind/EncryptedPush
// to fan out push notifications.
func TestEventsRedactsContentForRelayPushWhenSessionsSet(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, "#!/bin/sh\necho '{}'\n")
	s := New(&cmux.Client{Bin: bin}, nil)
	sessions, _, _ := pairedSessions(t)
	s.SetSessions(sessions)

	const relayTok = "relay-secret"
	srv := httptest.NewServer(s.TrustedHandler(relayTok))
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/events"
	h := http.Header{"X-Relay-Token": {relayTok}}
	c, resp, err := websocket.DefaultDialer.Dial(u, h)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("ws dial failed (status %d): %v", code, err)
	}
	defer c.Close()
	waitForSubscribers(t, s.hub, 1)

	s.hub.broadcast(wire.EventFrame{
		Type:           "feed",
		FeedID:         "X",
		NeedsAttention: true,
		Kind:           "Notification",
		Title:          "my-secret-project",
		Preview:        "Claude needs your permission",
		EncryptedPush:  map[string]string{"dev1": "ciphertext-blob"},
	})

	armReadDeadline(t, c)
	var got wire.EventFrame
	if err := c.ReadJSON(&got); err != nil {
		t.Fatalf("expected a plaintext JSON frame for the relay's push subscription, got: %v", err)
	}
	if got.Title != "" {
		t.Fatalf("relay must never see the workspace title, got %q", got.Title)
	}
	if got.Preview != "" {
		t.Fatalf("relay must never see the live status preview, got %q", got.Preview)
	}
	if got.FeedID != "X" || !got.NeedsAttention || got.Kind != "Notification" {
		t.Fatalf("routing metadata should still reach the relay: %+v", got)
	}
	if got.EncryptedPush["dev1"] != "ciphertext-blob" {
		t.Fatalf("EncryptedPush should still reach the relay so it can fan out push: %+v", got.EncryptedPush)
	}
}

func TestEventsRejectsUnrecognizedDeviceIDWhenEncrypted(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, "#!/bin/sh\necho '{}'\n")
	s := New(&cmux.Client{Bin: bin}, nil)
	sessions, _, _ := pairedSessions(t)
	s.SetSessions(sessions)

	const relayTok = "relay-secret"
	srv := httptest.NewServer(s.TrustedHandler(relayTok))
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/events"
	h := http.Header{"X-Relay-Token": {relayTok}, "X-Device-ID": {"unknown-device"}}
	_, resp, err := websocket.DefaultDialer.Dial(u, h)
	if err == nil {
		t.Fatal("expected dial to fail for an unrecognized X-Device-ID")
	}
	if resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %v", resp)
	}
}
