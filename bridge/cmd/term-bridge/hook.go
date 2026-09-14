package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sodre90/term-bridge/internal/host/agentfeed"
)

// runHook is the Claude Code command hook. It forwards the hook's stdin to
// the agent's hooks socket and exits 0 with no output whatever happens: an
// empty stdout is "no decision" to Claude Code, and any other exit code or
// output could block or alter a tool call in a session that is not ours.
func runHook(args []string) int {
	if len(args) > 0 && args[0] == "install" {
		return runHookInstall(args[1:])
	}
	pane := os.Getenv("TMUX_PANE")
	socket := agentfeed.SocketPath()
	if pane == "" || socket == "" {
		return 0
	}
	event, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil || !json.Valid(event) {
		return 0
	}
	_ = agentfeed.Forward(socket, agentfeed.Envelope{
		Pane:   pane,
		Socket: tmuxSocketFromEnv(os.Getenv("TMUX")),
		Event:  event,
	})
	return 0
}

// tmuxSocketFromEnv reads the server socket path out of $TMUX, which tmux
// sets to "<socket>,<pid>,<session index>".
func tmuxSocketFromEnv(v string) string {
	socket, _, _ := strings.Cut(v, ",")
	return socket
}

// hookTimeoutSeconds is what Claude Code allows the hook before giving up
// on it; the forwarder's own deadlines are shorter.
const hookTimeoutSeconds = 5

// runHookInstall adds the term-bridge hook to ~/.claude/settings.json for
// every event the feed listens to, leaving everything else in the file as
// it was. It prints what will change and, unless --dry-run, writes it.
func runHookInstall(args []string) int {
	fs := flag.NewFlagSet("hook install", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print the resulting settings without writing them")
	settingsPath := fs.String("settings", defaultClaudeSettingsPath(), "path to Claude Code's settings.json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "hook install:", err)
		return 1
	}
	before, err := os.ReadFile(*settingsPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "hook install:", err)
		return 1
	}
	after, changed, err := installHook(before, exe+" hook")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook install: %s: %v\n", *settingsPath, err)
		return 1
	}
	if !changed {
		fmt.Printf("%s already runs the term-bridge hook; nothing to do\n", *settingsPath)
		return 0
	}
	fmt.Printf("--- %s (before)\n%s\n+++ %s (after)\n%s\n", *settingsPath, strings.TrimSpace(string(before)), *settingsPath, after)
	if *dryRun {
		fmt.Println("dry run: not written")
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(*settingsPath), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "hook install:", err)
		return 1
	}
	if err := replaceFile(*settingsPath, append(after, '\n')); err != nil {
		fmt.Fprintln(os.Stderr, "hook install:", err)
		return 1
	}
	fmt.Printf("wrote %s; restart Claude Code sessions to pick it up\n", *settingsPath)
	return 0
}

// replaceFile swaps the file in whole, so a crash mid-write leaves Claude
// Code its old settings rather than half a JSON document.
func replaceFile(path string, data []byte) error {
	tmp := path + ".term-bridge.tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func defaultClaudeSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "settings.json"
	}
	return filepath.Join(home, ".claude", "settings.json")
}

// installHook merges the hook command into the settings JSON for each of
// agentfeed.Events, keeping unknown keys, and reports whether anything
// changed. An event already running a term-bridge hook is left alone.
func installHook(settings []byte, command string) ([]byte, bool, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(settings)) > 0 {
		if err := json.Unmarshal(settings, &root); err != nil {
			return nil, false, err
		}
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	changed := false
	for _, event := range agentfeed.Events {
		groups, _ := hooks[event].([]any)
		if hasHookCommand(groups, isTermBridgeHook) {
			continue
		}
		hooks[event] = append(groups, map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": hookTimeoutSeconds}},
		})
		changed = true
	}
	if !changed {
		return settings, false, nil
	}
	root["hooks"] = hooks
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// isTermBridgeHook recognises our entry whatever path the binary had when
// it was written: "<dir>/term-bridge hook".
func isTermBridgeHook(command string) bool {
	bin, ok := strings.CutSuffix(command, " hook")
	return ok && strings.Contains(filepath.Base(bin), "term-bridge")
}

func hasHookCommand(groups []any, match func(command string) bool) bool {
	for _, g := range groups {
		group, _ := g.(map[string]any)
		entries, _ := group["hooks"].([]any)
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			if cmd, _ := entry["command"].(string); match(cmd) {
				return true
			}
		}
	}
	return false
}
