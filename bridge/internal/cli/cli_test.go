package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigPath(t *testing.T) {
	p := filepath.ToSlash(ConfigPath("term-bridge-relay", "config.toml"))
	if !strings.HasSuffix(p, ".config/term-bridge-relay/config.toml") {
		t.Fatalf("ConfigPath = %q", p)
	}
}

func TestRefuseLegacyConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacy := filepath.Join(home, ".config", "cmux-bridge")
	current := filepath.Join(home, ".config", "term-bridge")

	if err := RefuseLegacyConfigDir("cmux-bridge", "term-bridge"); err != nil {
		t.Fatalf("a fresh machine must pass: %v", err)
	}
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	err := RefuseLegacyConfigDir("cmux-bridge", "term-bridge")
	if err == nil {
		t.Fatal("an unmoved legacy directory must be refused")
	}
	if !strings.Contains(err.Error(), legacy) || !strings.Contains(err.Error(), current) {
		t.Fatalf("the error must name both directories: %v", err)
	}
	if err := os.MkdirAll(current, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RefuseLegacyConfigDir("cmux-bridge", "term-bridge"); err != nil {
		t.Fatalf("a moved (or copied) directory must pass: %v", err)
	}
}

func TestLoadStoreOpensConfiguredStore(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "devices.json")
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("token_store = \""+storePath+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, store, err := LoadStore(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if store == nil {
		t.Fatal("store should be non-nil")
	}
	if cfg.TokenStore != storePath {
		t.Fatalf("TokenStore = %q want %q", cfg.TokenStore, storePath)
	}
	// Issuing a token should persist to the configured path.
	tenant, _ := store.CreateTenant()
	if _, err := store.Issue(tenant, "phone", "test-device-pubkey-b64"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("store file not created: %v", err)
	}
}

// A missing store used to be indistinguishable from an empty one: the admin
// command created it and answered "no paired devices" from the wrong file.
func TestOpenExistingStoreRefusesToCreateAStore(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "devices.db")
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("token_store = \""+storePath+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := OpenExistingStore(cfgPath)
	if err == nil {
		t.Fatal("opening a store that does not exist must fail, not create one")
	}
	if !strings.Contains(err.Error(), storePath) {
		t.Fatalf("the error must name the path it looked at: %v", err)
	}
	if _, statErr := os.Stat(storePath); statErr == nil {
		t.Fatal("the refused open still created the store")
	}
}

func TestOpenExistingStoreOpensAStoreThatIsThere(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "devices.db")
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("token_store = \""+storePath+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadStore(cfgPath); err != nil {
		t.Fatal(err)
	}

	_, store, err := OpenExistingStore(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if store == nil {
		t.Fatal("store should be non-nil")
	}
}
