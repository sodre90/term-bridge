package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sodre90/term-bridge/internal/version"
	"github.com/sodre90/term-bridge/internal/wire"
)

func TestVersionReportsTheRunningBuild(t *testing.T) {
	s, tok := newTestServer(t, fakeTerminalScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/version", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got wire.VersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	// Compared against the package rather than a literal: the release process
	// bumps version.v, and a hard-coded string here would have to be bumped
	// with it or start lying.
	if got.Bridge != version.String() {
		t.Fatalf("bridge = %q, want %q", got.Bridge, version.String())
	}
	if got.Bridge == "" {
		t.Fatal("bridge version is empty, so the field carries nothing")
	}
}

// A build number tells an unauthenticated caller which fixes this agent is
// missing, and nothing needs it before pairing.
func TestVersionRequiresAuth(t *testing.T) {
	s, _ := newTestServer(t, fakeTerminalScript)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		t.Fatal("GET /version answered an unauthenticated caller")
	}
}

// The app decodes this by field name, so the JSON key is part of the contract
// and renaming it would silently blank the Connections screen.
func TestVersionUsesTheWireFieldNameTheAppDecodes(t *testing.T) {
	raw, err := json.Marshal(wire.VersionResponse{Bridge: "9.9.9"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"bridge":"9.9.9"`) {
		t.Fatalf("encoded as %s, want a \"bridge\" key", raw)
	}
}
