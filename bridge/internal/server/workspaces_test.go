package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sodre90/term-bridge/internal/wire"
)

const (
	wsID    = "21B6A522-37FB-46A8-ABB3-F69917522BEF"
	surfID  = "2620FAA3-A6AE-41AD-8C6A-4A4ED139D622"
	surfID2 = "E35FB51D-003D-4E1B-A094-EBDF47130FAB"
	paneID  = "077B6A4E-9967-4DE6-84C7-9E56512FA821"
	paneID2 = "28D4EADE-4CEF-489C-865D-FA92A09584AD"
	newSurf = "B9040FB4-ADA0-4473-9A08-8152BC33B1B1"
	newPane = "9C1D0C6B-6E4F-4B2A-9C3D-1E2F3A4B5C6D"
)

// fakeLayoutScript answers the RPCs the create/select/close routes make with
// the shapes captured live on 2026-09-12 (a two-column scratch workspace)
// and logs every call. Method names it does not know fail, so a route that
// reaches for the wrong RPC is caught.
const fakeLayoutScript = `#!/bin/sh
printf '%s\n' "$*" >> "$CMUX_FAKE_LOG"
case "$2" in
  workspace.create) echo '{"workspace_id":"` + wsID + `","surface_id":"` + surfID + `","workspace_ref":"workspace:16"}' ;;
  surface.split) echo '{"surface_id":"` + newSurf + `","pane_id":"` + newPane + `","workspace_id":"` + wsID + `"}' ;;
  surface.create) echo '{"surface_id":"` + newSurf + `","pane_id":"` + paneID2 + `","type":"terminal"}' ;;
  pane.list) cat <<'JSON'
{"container_frame":{"height":1382,"width":2312},"panes":[
 {"focused":true,"id":"` + paneID + `","index":0,"pixel_frame":{"height":1382,"width":1156,"x":248,"y":28},
  "selected_surface_id":"` + surfID + `","surface_ids":["` + surfID + `"]},
 {"focused":false,"id":"` + paneID2 + `","index":1,"pixel_frame":{"height":1382,"width":1156,"x":1404,"y":28},
  "selected_surface_id":"` + surfID2 + `","surface_ids":["` + surfID2 + `"]}]}
JSON
  ;;
  workspace.select|surface.focus|workspace.close|surface.close) echo '{"ok":true}' ;;
  *) echo "unknown method $2" >&2; exit 1 ;;
esac
`

type rpcCall struct {
	Method string
	Params map[string]any
}

// rpcCalls reads back what the fake cmux was asked, in order.
func rpcCalls(t *testing.T, logPath string) []rpcCall {
	t.Helper()
	data, _ := os.ReadFile(logPath)
	var calls []rpcCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 3)
		if len(fields) < 2 || fields[0] != "rpc" {
			t.Fatalf("unexpected cmux invocation %q", line)
		}
		call := rpcCall{Method: fields[1], Params: map[string]any{}}
		if len(fields) == 3 {
			if err := json.Unmarshal([]byte(fields[2]), &call.Params); err != nil {
				t.Fatalf("params of %q not json: %v", line, err)
			}
		}
		calls = append(calls, call)
	}
	return calls
}

// assertExplicitTargets is the guard against cmux's default-target
// behaviour: a create or close that names no id lands wherever the Mac's
// focus happens to be.
func assertExplicitTargets(t *testing.T, calls []rpcCall) {
	t.Helper()
	for _, c := range calls {
		var key string
		switch c.Method {
		case "surface.split", "surface.focus", "surface.close":
			key = "surface_id"
		case "workspace.select", "workspace.close", "surface.create", "pane.list":
			key = "workspace_id"
		case "workspace.create":
			key = "cwd"
		default:
			continue
		}
		if v, _ := c.Params[key].(string); v == "" {
			t.Fatalf("%s called without %s: %v", c.Method, key, c.Params)
		}
		if c.Method == "surface.create" {
			if v, _ := c.Params["pane_id"].(string); v == "" {
				t.Fatalf("surface.create without pane_id: %v", c.Params)
			}
		}
		if focus, present := c.Params["focus"]; present && focus != false {
			t.Fatalf("%s must not take focus on the Mac: %v", c.Method, c.Params)
		}
	}
}

func layoutTestServer(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	logPath := t.TempDir() + "/cmux.log"
	t.Setenv("CMUX_FAKE_LOG", logPath)
	s, tok := newTestServer(t, fakeLayoutScript)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, tok, logPath
}

func do(t *testing.T, method, url, tok, body string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, reader)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func TestCreateWorkspaceInADirectoryUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "prj", "thing")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	srv, tok, logPath := layoutTestServer(t)

	resp, body := do(t, "POST", srv.URL+"/sessions", tok, `{"cwd":"`+project+`","title":"  Thing  "}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	var created wire.CreateWorkspaceResponse
	if err := json.Unmarshal(body, &created); err != nil || created.WorkspaceID != wsID || created.SurfaceID != surfID {
		t.Fatalf("unexpected reply %s (%v)", body, err)
	}
	calls := rpcCalls(t, logPath)
	assertExplicitTargets(t, calls)
	if len(calls) != 1 || calls[0].Method != "workspace.create" {
		t.Fatalf("want one workspace.create, got %+v", calls)
	}
	resolvedProject, _ := filepath.EvalSymlinks(project)
	if calls[0].Params["cwd"] != resolvedProject || calls[0].Params["title"] != "Thing" || calls[0].Params["focus"] != false {
		t.Fatalf("params %v", calls[0].Params)
	}
}

func TestCreateWorkspaceRefusesBadDirectoriesBeforeCallingCmux(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	file := filepath.Join(home, "notes.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	escape := filepath.Join(home, "link-out")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	srv, tok, logPath := layoutTestServer(t)

	cases := map[string]string{
		`{"cwd":"prj/relative"}`:                                            "cwd_not_absolute",
		`{"cwd":"` + home + `/does-not-exist"}`:                             "cwd_not_found",
		`{"cwd":"` + file + `"}`:                                            "cwd_not_dir",
		`{"cwd":"` + outside + `"}`:                                         "cwd_outside_home",
		`{"cwd":"` + escape + `"}`:                                          "cwd_outside_home",
		`{"cwd":"` + home + `/../"}`:                                        "cwd_outside_home",
		`{"cwd":"` + home + `","title":"` + strings.Repeat("x", 121) + `"}`: "title too long",
	}
	for body, want := range cases {
		resp, reply := do(t, "POST", srv.URL+"/sessions", tok, body)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(reply), want) {
			t.Errorf("%s: want 400 %q, got %d %s", body, want, resp.StatusCode, reply)
		}
	}
	if calls := rpcCalls(t, logPath); len(calls) != 0 {
		t.Fatalf("cmux must not be called on refused input; got %+v", calls)
	}
}

func TestCreateWorkspaceHomeItselfIsAllowed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv, tok, _ := layoutTestServer(t)
	resp, body := do(t, "POST", srv.URL+"/sessions", tok, `{"cwd":"`+home+`"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for $HOME itself, got %d: %s", resp.StatusCode, body)
	}
}

func TestCreateWorkspaceCmuxFailure502(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, tok := newTestServer(t, "#!/bin/sh\nexit 1\n")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, _ := do(t, "POST", srv.URL+"/sessions", tok, `{"cwd":"`+home+`"}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
}

func TestSelectWorkspaceThenFocusSurface(t *testing.T) {
	srv, tok, logPath := layoutTestServer(t)
	resp, body := do(t, "POST", srv.URL+"/sessions/"+wsID+"/select", tok, `{"surface_id":"`+surfID2+`"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	calls := rpcCalls(t, logPath)
	assertExplicitTargets(t, calls)
	if len(calls) != 2 || calls[0].Method != "workspace.select" || calls[1].Method != "surface.focus" {
		t.Fatalf("want select then focus, got %+v", calls)
	}
	if calls[0].Params["workspace_id"] != wsID || calls[1].Params["surface_id"] != surfID2 {
		t.Fatalf("params %+v", calls)
	}
}

func TestSelectWorkspaceWithoutABodySelectsOnly(t *testing.T) {
	srv, tok, logPath := layoutTestServer(t)
	resp, _ := do(t, "POST", srv.URL+"/sessions/"+wsID+"/select", tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if calls := rpcCalls(t, logPath); len(calls) != 1 || calls[0].Method != "workspace.select" {
		t.Fatalf("want only workspace.select, got %+v", calls)
	}
}

func TestCloseWorkspaceNamesTheWorkspace(t *testing.T) {
	srv, tok, logPath := layoutTestServer(t)
	resp, _ := do(t, "DELETE", srv.URL+"/sessions/"+wsID, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	calls := rpcCalls(t, logPath)
	assertExplicitTargets(t, calls)
	if len(calls) != 1 || calls[0].Method != "workspace.close" || calls[0].Params["workspace_id"] != wsID {
		t.Fatalf("got %+v", calls)
	}
}

// A ref, an index or an empty id must never reach cmux: its create and
// close methods fall back to whatever is focused on the Mac.
func TestMutationsRefuseAnythingButAUUIDBeforeCallingCmux(t *testing.T) {
	srv, tok, logPath := layoutTestServer(t)
	requests := []struct{ method, path, body string }{
		{"DELETE", "/sessions/workspace:9", ""},
		{"DELETE", "/sessions/1", ""},
		{"POST", "/sessions/workspace:9/select", ""},
		{"POST", "/sessions/" + wsID + "/select", `{"surface_id":"surface:40"}`},
		{"GET", "/sessions/workspace:9/layout", ""},
		{"POST", "/sessions/workspace:9/panes", `{"surface_id":"` + surfID + `","placement":"right"}`},
		{"POST", "/sessions/" + wsID + "/panes", `{"surface_id":"","placement":"right"}`},
		{"POST", "/sessions/" + wsID + "/panes", `{"surface_id":"surface:40","placement":"right"}`},
		{"DELETE", "/sessions/" + wsID + "/panes/surface:40", ""},
		{"DELETE", "/sessions/workspace:9/panes/" + surfID, ""},
	}
	for _, rq := range requests {
		resp, _ := do(t, rq.method, srv.URL+rq.path, tok, rq.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s %s %s: want 400, got %d", rq.method, rq.path, rq.body, resp.StatusCode)
		}
	}
	if calls := rpcCalls(t, logPath); len(calls) != 0 {
		t.Fatalf("cmux must not be called; got %+v", calls)
	}
}

func TestMutationsRequireAToken(t *testing.T) {
	srv, _, _ := layoutTestServer(t)
	for _, rq := range []struct{ method, path string }{
		{"POST", "/sessions"},
		{"DELETE", "/sessions/" + wsID},
		{"POST", "/sessions/" + wsID + "/select"},
		{"POST", "/sessions/" + wsID + "/panes"},
		{"DELETE", "/sessions/" + wsID + "/panes/" + surfID},
		{"GET", "/sessions/" + wsID + "/layout"},
	} {
		req, _ := http.NewRequest(rq.method, srv.URL+rq.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s: want 401, got %d", rq.method, rq.path, resp.StatusCode)
		}
	}
}
