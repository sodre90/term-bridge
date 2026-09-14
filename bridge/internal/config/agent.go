package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"

	"github.com/BurntSushi/toml"
)

// AgentConfig is the Mac agent's configuration. The agent dials the relay and
// serves the cmux handler over the tunnel; it holds no device secrets other
// than its own e2e identity key.
type AgentConfig struct {
	// Host names the terminal backend this agent fronts: "cmux" (the Mac,
	// the default) or "tmux" (a Linux box, see TmuxBin/TmuxSocket).
	Host    string `toml:"host"`
	CmuxBin string `toml:"cmux_bin"`
	// TmuxBin is the tmux binary for Host = "tmux"; TmuxSocket, when set,
	// is passed as `tmux -S` for a server on a non-default socket path.
	TmuxBin    string `toml:"tmux_bin"`
	TmuxSocket string `toml:"tmux_socket"`
	RelayURL   string `toml:"relay_url"`
	ClientCert string `toml:"client_cert"`
	ClientKey  string `toml:"client_key"`
	CACert     string `toml:"ca_cert"`
	RelayToken string `toml:"relay_token"`
	// BootstrapURL is the relay's no-mTLS registration endpoint
	// (e.g. https://cmux.example.com:8444/tenants/register), used exactly
	// once, on first run, when ClientCert/ClientKey/CACert don't exist yet.
	BootstrapURL string `toml:"bootstrap_url"`
	// IdentityKey is the path to this agent's X25519 e2e identity private key
	// (internal/e2e.Identity), created on first use by `term-bridge
	// pair-device`.
	IdentityKey string `toml:"identity_key"`
	// SessionStore is the path to the SQLite database holding this agent's
	// paired devices' e2e shared secrets and replay counters
	// (internal/e2e.Store). On first run against a path with no existing
	// database, a same-named legacy sessions.json sibling (the pre-SQLite
	// on-disk format) is imported automatically and then renamed to
	// "<name>.json.migrated" -- an upgrading install keeps its paired
	// devices with no manual step.
	SessionStore string `toml:"session_store"`
	// YoloStore is the path to the SQLite database holding each workspace's
	// opt-in auto-reply mode for permission prompts (internal/yolo.Store).
	// Same one-time legacy-JSON import behavior as SessionStore, above.
	YoloStore string `toml:"yolo_store"`
	// DirectListen is the address the agent listens on for direct
	// (Tailscale) connections, e.g. ":8443". Empty (the default) disables
	// direct mode entirely — the agent behaves exactly as it does today,
	// relay-only.
	DirectListen string `toml:"direct_listen"`
	// DirectAuthStore is the path to direct mode's own local SQLite device
	// store (internal/auth.Store) — deliberately separate from any
	// relay-shaped state, since direct mode has exactly one implicit tenant
	// (this Mac).
	DirectAuthStore string `toml:"direct_auth_store"`
	// FCMProjectID is the Firebase project id for direct-mode push. Empty
	// disables it -- direct mode behaves exactly as it does today. Same
	// Firebase project as the relay's own fcm_project_id, configured
	// separately here since the agent has its own independent device store.
	FCMProjectID string `toml:"fcm_project_id"`
	// FCMCredentials is the path to a Google service-account JSON key for
	// direct-mode push. Empty disables it.
	FCMCredentials string `toml:"fcm_credentials"`
	// StatusFile is the path to the small JSON health snapshot this agent
	// writes periodically (see internal/status), read by `term-bridge
	// status`.
	StatusFile string `toml:"status_file"`
	// FCMAppID, FCMAPIKey and FCMSenderID are the client half of the
	// Firebase configuration, handed to a phone at pairing (wire.
	// FCMClientConfig) so the app can initialise FCM without being rebuilt
	// with a google-services.json of its own. Read them off the Firebase
	// console's Android app, or out of a google-services.json:
	// mobilesdk_app_id, current_key, and project_number respectively.
	// FCMProjectID above doubles as the project id. All four are needed
	// before anything is sent.
	FCMAppID    string `toml:"fcm_app_id"`
	FCMAPIKey   string `toml:"fcm_api_key"`
	FCMSenderID string `toml:"fcm_sender_id"`
	// AttachmentsDir is where images sent from the phone are written before
	// their path is pasted into the pane (server.AttachmentStore). Kept
	// free of spaces so the pasted path needs no quoting. Files older than
	// seven days are removed.
	AttachmentsDir string `toml:"attachments_dir"`
	// LogFile is the path the agent writes its own rotated log to (see
	// internal/logging.UseRotatingFile). Set it empty to log to stderr
	// instead and leave the file to whatever supervises the process --
	// which, under the shipped launchd plist, means an unbounded file.
	LogFile string `toml:"log_file"`
}

// Host values.
const (
	HostCmux = "cmux"
	HostTmux = "tmux"
)

func agentDefaults() AgentConfig {
	return AgentConfig{
		Host:            HostCmux,
		CmuxBin:         "cmux",
		TmuxBin:         "tmux",
		IdentityKey:     "~/.config/cmux-bridge/identity.key",
		SessionStore:    "~/.config/cmux-bridge/sessions.db",
		YoloStore:       "~/.config/cmux-bridge/yolo.db",
		DirectAuthStore: "~/.config/cmux-bridge/direct-auth.db",
		StatusFile:      "~/.config/cmux-bridge/status.json",
		AttachmentsDir:  "~/.config/cmux-bridge/attachments",
		LogFile:         defaultLogFile(),
	}
}

// defaultLogFile is launchd's convention on the Mac; elsewhere the agent
// logs to stderr for whatever supervises it (journald under systemd).
func defaultLogFile() string {
	if runtime.GOOS == "darwin" {
		return "~/Library/Logs/cmux-bridge.log"
	}
	return ""
}

// LoadAgent reads the agent TOML at path. A missing file yields defaults.
func LoadAgent(path string) (AgentConfig, error) {
	cfg := agentDefaults()
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := toml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse agent config %s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// Fall through with defaults.
	default:
		return cfg, fmt.Errorf("read agent config %s: %w", path, err)
	}
	if cfg.CmuxBin == "" {
		cfg.CmuxBin = "cmux"
	}
	if cfg.TmuxBin == "" {
		cfg.TmuxBin = "tmux"
	}
	switch cfg.Host {
	case "":
		cfg.Host = HostCmux
	case HostCmux, HostTmux:
	default:
		return cfg, fmt.Errorf("agent config %s: host must be %q or %q, not %q", path, HostCmux, HostTmux, cfg.Host)
	}
	cfg.TmuxSocket = expandHome(cfg.TmuxSocket)
	cfg.ClientCert = expandHome(cfg.ClientCert)
	cfg.ClientKey = expandHome(cfg.ClientKey)
	cfg.CACert = expandHome(cfg.CACert)
	cfg.IdentityKey = expandHome(cfg.IdentityKey)
	cfg.SessionStore = expandHome(cfg.SessionStore)
	cfg.YoloStore = expandHome(cfg.YoloStore)
	cfg.DirectAuthStore = expandHome(cfg.DirectAuthStore)
	cfg.FCMCredentials = expandHome(cfg.FCMCredentials)
	cfg.StatusFile = expandHome(cfg.StatusFile)
	cfg.AttachmentsDir = expandHome(cfg.AttachmentsDir)
	cfg.LogFile = expandHome(cfg.LogFile)
	return cfg, nil
}
