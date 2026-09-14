package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sodre90/term-bridge/internal/host/agentfeed"
)

func TestInstallHookAddsEveryEventAndKeepsTheRest(t *testing.T) {
	before := `{
  "theme": "dark",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/local/bin/lint"}]}
    ]
  }
}`
	after, changed, err := installHook([]byte(before), "/home/u/bin/term-bridge hook")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var root map[string]any
	if err := json.Unmarshal(after, &root); err != nil {
		t.Fatal(err)
	}
	if root["theme"] != "dark" {
		t.Fatalf("unrelated key lost: %s", after)
	}
	hooks := root["hooks"].(map[string]any)
	for _, event := range agentfeed.Events {
		if !hasHookCommand(hooks[event].([]any), isTermBridgeHook) {
			t.Fatalf("%s not installed: %s", event, after)
		}
	}
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 2 || !hasHookCommand(pre[:1], func(c string) bool { return c == "/usr/local/bin/lint" }) {
		t.Fatalf("existing PreToolUse hook disturbed: %v", pre)
	}
	entry := pre[1].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if entry["type"] != "command" || entry["command"] != "/home/u/bin/term-bridge hook" || entry["timeout"] != float64(hookTimeoutSeconds) {
		t.Fatalf("entry = %v", entry)
	}

	again, changed, err := installHook(after, "/elsewhere/term-bridge hook")
	if err != nil || changed || string(again) != string(after) {
		t.Fatalf("second install should be a no-op: changed=%v err=%v", changed, err)
	}
}

func TestInstallHookStartsFromNothing(t *testing.T) {
	after, changed, err := installHook(nil, "/home/u/bin/term-bridge hook")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var root struct {
		Hooks map[string][]any `json:"hooks"`
	}
	if err := json.Unmarshal(after, &root); err != nil || len(root.Hooks) != len(agentfeed.Events) {
		t.Fatalf("after = %s (%v)", after, err)
	}
	if _, _, err := installHook([]byte("{not json"), "x hook"); err == nil {
		t.Fatal("malformed settings should not be overwritten")
	}
}

func TestHookInstallDryRunWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if code := runHookInstall([]string{"--dry-run", "--settings", path}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote %s", path)
	}
	if code := runHookInstall([]string{"--settings", path}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), " hook\"") {
		t.Fatalf("settings = %s (%v)", b, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode())
	}
}

func TestHookPassesThroughOutsideTmuxOrWithoutASocket(t *testing.T) {
	t.Setenv("TMUX_PANE", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if code := runHook(nil); code != 0 {
		t.Fatalf("no pane: exit %d", code)
	}
	t.Setenv("TMUX_PANE", "%3")
	t.Setenv("TMUX", "/tmp/tmux-1000/default,123,0")
	t.Setenv("XDG_RUNTIME_DIR", "")
	if code := runHook(nil); code != 0 {
		t.Fatalf("no runtime dir: exit %d", code)
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if code := runHook(nil); code != 0 {
		t.Fatalf("no socket: exit %d", code)
	}
}

func TestTmuxSocketFromEnv(t *testing.T) {
	if got := tmuxSocketFromEnv("/tmp/tmux-1000/default,4242,0"); got != "/tmp/tmux-1000/default" {
		t.Fatalf("got %q", got)
	}
	if got := tmuxSocketFromEnv(""); got != "" {
		t.Fatalf("got %q", got)
	}
}
