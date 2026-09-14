package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sodre90/cmux-bridge/internal/auth"
	"github.com/sodre90/cmux-bridge/internal/host/tmuxhost"
	"github.com/sodre90/cmux-bridge/internal/testutil"
	"github.com/sodre90/cmux-bridge/internal/tmux"
	"github.com/sodre90/cmux-bridge/internal/wire"
)

// A tmux server with one window of one pane, as list-panes -a prints it.
const fakeTmuxOneWindow = `#!/bin/sh
case "$*" in
  "list-panes -a -F "*) printf '1789367814\037main\037@3\037bash\0371\037%%9\0371\037bash\037/home/u\0370\0370\037100\03730\n';;
  "display-message -p -t "*"#{start_time}") echo 1789367814;;
esac
`

func newTmuxTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	store, err := auth.Open(t.TempDir() + "/d.db")
	if err != nil {
		t.Fatal(err)
	}
	tenant, _ := store.CreateTenant()
	tok, _ := store.Issue(tenant, "phone", "test-device-pubkey-b64")
	h := tmuxhost.New(&tmux.Client{Bin: testutil.WriteFakeTmux(t, fakeTmuxOneWindow)})
	return NewWithHost(h, store), tok
}

func tmuxRequest(t *testing.T, srv *httptest.Server, tok, method, path, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestTmuxHostAdvertisesItselfAndRefusesWhatItLacks(t *testing.T) {
	s, tok := newTmuxTestServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp := tmuxRequest(t, srv, tok, "GET", "/sessions", "")
	var sessions wire.SessionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if sessions.Host.Kind != "tmux" || sessions.Host.Capabilities.Tabs || sessions.Host.Capabilities.Feed {
		t.Fatalf("host = %+v", sessions.Host)
	}
	if len(sessions.Workspaces) != 1 || sessions.Workspaces[0].ID != "tmux-1789367814-w3" {
		t.Fatalf("workspaces = %+v", sessions.Workspaces)
	}

	resp = tmuxRequest(t, srv, tok, "POST", "/sessions/tmux-1789367814-w3/panes",
		`{"surface_id":"tmux-1789367814-p9","placement":"tab"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("tab placement on tmux = %d, want 400", resp.StatusCode)
	}

	resp = tmuxRequest(t, srv, tok, "POST", "/feed/x/reply", `{"kind":"permissionRequest","request_id":"r"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("feed reply on tmux = %d, want 400", resp.StatusCode)
	}

	resp = tmuxRequest(t, srv, tok, "GET", "/feed/pending", "")
	var pending struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pending); err != nil || pending.Items == nil || len(pending.Items) != 0 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	_ = resp.Body.Close()

	// A cmux UUID is not one of this host's ids: refused before any tmux call.
	resp = tmuxRequest(t, srv, tok, "GET", "/sessions/0B1D3CA1-7E2A-4E7B-9E2F-0A1B2C3D4E5F/layout", "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("uuid on tmux = %d, want 400", resp.StatusCode)
	}
}
