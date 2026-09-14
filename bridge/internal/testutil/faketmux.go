package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// WriteFakeTmux writes script to an executable file named "tmux" in a temp
// dir and returns its absolute path, for tmux.Client.Bin.
func WriteFakeTmux(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}
