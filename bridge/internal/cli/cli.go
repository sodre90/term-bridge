// Package cli holds bootstrap helpers shared by the term-bridge and term-bridge-relay
// command binaries: resolving config paths and opening the device-token store
// the config points at.
package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/config"
)

// ConfigPath returns ~/.config/<app>/<file>, falling back to <file> in the
// working directory when the home directory cannot be resolved.
func ConfigPath(app, file string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return file
	}
	return filepath.Join(home, ".config", app, file)
}

// RefuseLegacyConfigDir keeps a machine from running half-renamed. The
// binaries were cmux-bridge and cmux-relay before 0.9; a config directory
// still under the old name next to none under the new one would have the
// agent mint a fresh identity, and every phone paired to this host would
// find it gone. The operator moves the directory (and the paths inside its
// config) by hand; this only makes sure they know.
func RefuseLegacyConfigDir(legacyApp, app string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	legacy := filepath.Join(home, ".config", legacyApp)
	current := filepath.Join(home, ".config", app)
	if _, err := os.Stat(legacy); err != nil {
		return nil
	}
	if _, err := os.Stat(current); err == nil {
		return nil
	}
	return fmt.Errorf("found %s but no %s: move it (mv %s %s), update the paths inside its config, then retry",
		legacy, current, legacy, current)
}

// LoadStore loads the config at cfgPath and opens the device-token store it
// names, creating it if it is not there yet. That is what `serve` wants on a
// first run; the admin commands want OpenExistingStore instead.
func LoadStore(cfgPath string) (config.Config, *auth.Store, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return cfg, nil, err
	}
	store, err := auth.Open(cfg.TokenStore)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, store, nil
}

// OpenExistingStore is LoadStore for the read-write admin commands, which
// must never bring a store into existence: a missing config file silently
// yields defaults (config.Load), so pointing at the wrong config makes
// LoadStore create a fresh, empty store and answer confidently from it.
// Observed 2026-08-11: `podman exec term-bridge-relay term-bridge-relay devices list`
// reported "no paired devices" against a relay holding 19 (cmux-app-xdc).
// A tool that answers a question about the wrong database is worse than one
// that fails, so this refuses -- and names the path it looked at, since that
// is the fact the operator needs.
func OpenExistingStore(cfgPath string) (config.Config, *auth.Store, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return cfg, nil, err
	}
	if _, err := os.Stat(cfg.TokenStore); err != nil {
		return cfg, nil, fmt.Errorf("no device store at %s (config: %s) -- is --config pointing at the right file?",
			cfg.TokenStore, cfgPath)
	}
	store, err := auth.Open(cfg.TokenStore)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, store, nil
}
