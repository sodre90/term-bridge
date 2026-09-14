package wire

import (
	"encoding/json"
	"testing"
)

// The app mirrors this shape field for field (model/Dtos.kt HostInfo); the
// key names here are the contract.
func TestSessionsResponseWireShape(t *testing.T) {
	body, err := json.Marshal(SessionsResponse{
		Workspaces: []Workspace{},
		Host:       HostInfo{Name: "home-server", Kind: "tmux", Capabilities: HostCapabilities{Tabs: false, Feed: false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"workspaces":[],"host":{"name":"home-server","kind":"tmux","capabilities":{"tabs":false,"feed":false}}}`
	if string(body) != want {
		t.Fatalf("got %s\nwant %s", body, want)
	}
}

func TestSessionsResponseCarriesThePendingCountWhenKnown(t *testing.T) {
	two := 2
	body, err := json.Marshal(SessionsResponse{
		Workspaces:   []Workspace{},
		Host:         HostInfo{Name: "mac", Kind: "cmux", Capabilities: HostCapabilities{Tabs: true, Feed: true}},
		PendingCount: &two,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"workspaces":[],"host":{"name":"mac","kind":"cmux","capabilities":{"tabs":true,"feed":true}},"pending_count":2}`
	if string(body) != want {
		t.Fatalf("got %s\nwant %s", body, want)
	}
}
