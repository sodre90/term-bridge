package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsWhenMissing(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8765" {
		t.Fatalf("default listen wrong: %q", cfg.Listen)
	}
	if cfg.CmuxBin != "cmux" {
		t.Fatalf("default cmux_bin wrong: %q", cfg.CmuxBin)
	}
	if cfg.TokenStore == "" {
		t.Fatal("default token_store should be set")
	}
}

func TestLoadOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(p, []byte(`
listen = "0.0.0.0:9000"
cmux_bin = "/opt/cmux"
fcm_project_id = "proj-123"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "0.0.0.0:9000" {
		t.Fatalf("listen override failed: %q", cfg.Listen)
	}
	if cfg.CmuxBin != "/opt/cmux" {
		t.Fatalf("cmux_bin override failed: %q", cfg.CmuxBin)
	}
	if cfg.FCMProjectID != "proj-123" {
		t.Fatalf("fcm_project_id override failed: %q", cfg.FCMProjectID)
	}
}

func TestLoadEnvOverridesWinOverFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(p, []byte(`
listen      = "0.0.0.0:9000"
relay_token = "file-token"
edge_token  = "file-edge"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CMUX_RELAY_TOKEN", "env-token")
	t.Setenv("CMUX_RELAY_EDGE_TOKEN", "env-edge")
	t.Setenv("CMUX_RELAY_FCM_CREDENTIALS", "/env/fcm.json")
	t.Setenv("CMUX_RELAY_LISTEN", "0.0.0.0:9999")

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RelayToken != "env-token" {
		t.Fatalf("relay_token override failed: %q", cfg.RelayToken)
	}
	if cfg.EdgeToken != "env-edge" {
		t.Fatalf("edge_token override failed: %q", cfg.EdgeToken)
	}
	if cfg.FCMCredentials != "/env/fcm.json" {
		t.Fatalf("fcm_credentials override failed: %q", cfg.FCMCredentials)
	}
	if cfg.Listen != "0.0.0.0:9999" {
		t.Fatalf("listen override failed: %q", cfg.Listen)
	}
}

func TestLoadEnvOverridesApplyWhenFileMissing(t *testing.T) {
	t.Setenv("CMUX_RELAY_TOKEN", "env-token")

	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RelayToken != "env-token" {
		t.Fatalf("relay_token override failed on missing file: %q", cfg.RelayToken)
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(p, []byte(`token_store = "~/.config/term-bridge/devices.json"`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config/term-bridge/devices.json")
	if cfg.TokenStore != want {
		t.Fatalf("expandHome failed: got %q want %q", cfg.TokenStore, want)
	}
}
