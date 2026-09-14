package logging

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func restoreDefaultLogger(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func TestTheAgentWritesItsOwnLogFile(t *testing.T) {
	restoreDefaultLogger(t)
	path := filepath.Join(t.TempDir(), "term-bridge.log")

	if err := UseRotatingFile(path); err != nil {
		t.Fatalf("UseRotatingFile: %v", err)
	}
	slog.Info("agent: dialing relay", "relay_url", "wss://relay.example/agent/tunnel")

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(written), "dialing relay") {
		t.Errorf("log line did not reach the file:\n%s", written)
	}
}

// The configured path is under ~/Library/Logs on a fresh Mac, which need not
// exist yet.
func TestAMissingLogDirectoryIsCreated(t *testing.T) {
	restoreDefaultLogger(t)
	path := filepath.Join(t.TempDir(), "Library", "Logs", "term-bridge.log")

	if err := UseRotatingFile(path); err != nil {
		t.Fatalf("UseRotatingFile: %v", err)
	}
	slog.Info("hello")

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("log file not created under a missing directory: %v", err)
	}
}

// log_file = "" is the documented way back to stderr.
func TestAnEmptyPathLeavesTheLoggerAlone(t *testing.T) {
	restoreDefaultLogger(t)
	Init()
	before := slog.Default()

	if err := UseRotatingFile(""); err != nil {
		t.Fatalf("UseRotatingFile(\"\"): %v", err)
	}
	if slog.Default() != before {
		t.Error("an empty path must not replace the handler")
	}
}

// The whole point of the bead: the file has a ceiling. Writing past MaxSize
// must leave a rolled generation behind rather than one ever-growing file.
func TestTheLogFileIsBounded(t *testing.T) {
	restoreDefaultLogger(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "term-bridge.log")
	if err := UseRotatingFile(path); err != nil {
		t.Fatalf("UseRotatingFile: %v", err)
	}

	noise := strings.Repeat("x", 4096)
	for range (maxLogSizeMB << 20 / 4096) + 64 {
		slog.Info("agent: relay unreachable", "err", noise)
	}

	live, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if live.Size() >= maxLogSizeMB<<20 {
		t.Errorf("live log is %d bytes, past the %dMB roll point", live.Size(), maxLogSizeMB)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) < 2 {
		t.Errorf("nothing was rolled: %v", entries)
	}
}
