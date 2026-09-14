package wire

// SessionsResponse is the body of GET /sessions: the live workspace list
// plus the identity of the host serving it. Host rides on the response the
// app already polls -- rather than on pairing, which the relay would have to
// carry blind -- so a capability change after an agent upgrade reaches the
// phone on its next refresh with no re-pair.
type SessionsResponse struct {
	Workspaces []Workspace `json:"workspaces"`
	Host       HostInfo    `json:"host"`
	// PendingCount is how many prompts the Inbox would list (question and
	// permissionRequest items), so the badge needs no /feed/pending fetch of
	// its own. Absent when the host has no feed, the feed could not be read,
	// or the bridge predates it -- the app then falls back to /feed/pending.
	PendingCount *int `json:"pending_count,omitempty"`
}

// HostInfo names the machine and backend behind an agent. Name is the
// host's short hostname, Kind one of "cmux" | "tmux" (host.Kind*), and
// Capabilities what the app may offer for it. Mirrored in Kotlin as
// HostInfo / HostCapabilities, whose defaults describe a cmux host so an
// older bridge that sends nothing here behaves as before.
type HostInfo struct {
	Name         string           `json:"name"`
	Kind         string           `json:"kind"`
	Capabilities HostCapabilities `json:"capabilities"`
}

// HostCapabilities is the wire form of host.Capabilities.
type HostCapabilities struct {
	// Tabs: "add as tab" placement exists (a pane holds several surfaces).
	Tabs bool `json:"tabs"`
	// Feed: the Inbox has structured prompts and replies for this host.
	Feed bool `json:"feed"`
}

// Workspace is the app-facing representation of a cmux workspace and its
// terminal surfaces (panes). Each pane's ID is a streamable terminal-surface id
// the app opens via /terminal/{id}.
type Workspace struct {
	ID        string `json:"id"`
	CWD       string `json:"cwd"`
	Title     string `json:"title"`
	Preview   string `json:"preview"`
	HasUnread bool   `json:"has_unread"`
	Attention string `json:"attention,omitempty"`
	YoloMode  string `json:"yolo_mode,omitempty"`
	// The user-picked color from cmux ("#rrggbb", "" when unset) — the app
	// renders it as an identifying dot on the workspace card.
	CustomColor string         `json:"custom_color,omitempty"`
	Terminals   []TerminalPane `json:"terminals"`
}

// TerminalPane is one terminal surface within a workspace. ID is the cmux
// terminal-surface id; Kind is a cosmetic badge derived from the title.
type TerminalPane struct {
	ID      string `json:"id"`
	CWD     string `json:"cwd"`
	Title   string `json:"title"`
	Focused bool   `json:"focused"`
	Ready   bool   `json:"ready"`
	Kind    string `json:"kind"`
}
