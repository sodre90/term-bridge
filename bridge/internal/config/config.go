// Package config loads the bridge's TOML configuration. A missing file yields
// sensible defaults so the bridge can run with zero configuration on the Mac
// where cmux is already installed.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the bridge configuration. Field tags map to TOML keys.
type Config struct {
	// Listen is the host:port the bridge binds. Default loopback only.
	Listen string `toml:"listen"`
	// CmuxBin is the path to the cmux CLI. Default "cmux" (resolved via PATH).
	CmuxBin string `toml:"cmux_bin"`
	// TokenStore is the path to the tenant/device SQLite database.
	TokenStore string `toml:"token_store"`
	// FCMProjectID is the Firebase project id for push. Empty disables push.
	FCMProjectID string `toml:"fcm_project_id"`
	// FCMCredentials is the path to a Google service-account JSON key. Empty
	// disables push.
	FCMCredentials string `toml:"fcm_credentials"`
	// FCMAppID, FCMAPIKey and FCMSenderID are the client half of the
	// Firebase configuration, handed to a phone at pairing (see
	// wire.FCMClientConfig) so the app can initialise FCM without a
	// google-services.json compiled into it. FCMProjectID above doubles as
	// the project id.
	FCMAppID    string `toml:"fcm_app_id"`
	FCMAPIKey   string `toml:"fcm_api_key"`
	FCMSenderID string `toml:"fcm_sender_id"`
	// RelayToken is the shared secret the relay injects and the agent checks.
	RelayToken string `toml:"relay_token"`
	// EdgeToken is the shared secret the trusted edge (nginx) must present on
	// every relay request. Empty disables the check (loopback-only relay).
	EdgeToken string `toml:"edge_token"`
	// CACert is the path to the relay's own CA certificate (PEM). Auto-
	// created on first run if absent.
	CACert string `toml:"ca_cert"`
	// CAKey is the path to the relay's own CA private key (PEM), 0600.
	CAKey string `toml:"ca_key"`
}

func defaults() Config {
	return Config{
		Listen:     "127.0.0.1:8765",
		CmuxBin:    "cmux",
		TokenStore: expandHome("~/.config/term-bridge-relay/store.db"),
		CACert:     expandHome("~/.config/term-bridge-relay/ca.crt"),
		CAKey:      expandHome("~/.config/term-bridge-relay/ca.key"),
	}
}

// Load reads the TOML file at path. If the file does not exist, defaults are
// returned. Any "~/" prefixes in path-valued fields are expanded to the user's
// home directory. CMUX_RELAY_* environment variables, if set, override the
// secrets and listen address either way (see applyEnvOverrides).
func Load(path string) (Config, error) {
	cfg := defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			applyEnvOverrides(&cfg)
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.TokenStore = expandHome(cfg.TokenStore)
	cfg.FCMCredentials = expandHome(cfg.FCMCredentials)
	cfg.CACert = expandHome(cfg.CACert)
	cfg.CAKey = expandHome(cfg.CAKey)
	if cfg.Listen == "" {
		cfg.Listen = defaults().Listen
	}
	if cfg.CmuxBin == "" {
		cfg.CmuxBin = defaults().CmuxBin
	}
	applyEnvOverrides(&cfg)
	return cfg, nil
}

// applyEnvOverrides lets CMUX_RELAY_TOKEN / CMUX_RELAY_EDGE_TOKEN /
// CMUX_RELAY_FCM_CREDENTIALS / CMUX_RELAY_LISTEN win over whatever
// config.toml (or the defaults) set, for exactly these four fields -- the
// ones a containerized relay deployment wants supplied by its orchestrator's
// secret store rather than a checked-in file. This is deliberately not a
// general env-config system: every other field stays TOML-only.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("CMUX_RELAY_TOKEN"); v != "" {
		cfg.RelayToken = v
	}
	if v := os.Getenv("CMUX_RELAY_EDGE_TOKEN"); v != "" {
		cfg.EdgeToken = v
	}
	if v := os.Getenv("CMUX_RELAY_FCM_CREDENTIALS"); v != "" {
		cfg.FCMCredentials = expandHome(v)
	}
	if v := os.Getenv("CMUX_RELAY_LISTEN"); v != "" {
		cfg.Listen = v
	}
}

// expandHome replaces a leading "~/" with the user's home directory. On
// failure or for non-"~" paths, the input is returned unchanged.
func expandHome(p string) string {
	if p == "" || !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}
