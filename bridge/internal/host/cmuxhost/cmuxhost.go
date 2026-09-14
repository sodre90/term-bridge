// Package cmuxhost implements host.Host on top of cmux's documented CLI
// (`cmux rpc` / `cmux events`, via internal/cmux). Every cmux method name,
// payload shape and quirk the bridge knows about lives here and nowhere
// else.
package cmuxhost

import (
	"regexp"

	"github.com/sodre90/term-bridge/internal/cmux"
	"github.com/sodre90/term-bridge/internal/host"
)

// Host talks to one cmux instance.
type Host struct {
	client *cmux.Client
}

// New wraps a cmux client. The client's own OnReached/FastPath wiring is the
// caller's business.
func New(c *cmux.Client) *Host { return &Host{client: c} }

var _ host.Host = (*Host)(nil)

func (h *Host) Kind() string { return host.KindCmux }

// Capabilities: cmux has tabs (several surfaces per pane) and a native
// agent feed.
func (h *Host) Capabilities() host.Capabilities {
	return host.Capabilities{Tabs: true, Feed: true}
}

// Every cmux workspace and surface id is a UUID. Mutating routes refuse
// anything else before an RPC is made: cmux's create methods fall back to
// the window or pane focused on the Mac when a target is missing, which is
// never what the phone meant.
var uuidPattern = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

// ValidID reports whether id is a cmux UUID.
func (h *Host) ValidID(id string) bool { return uuidPattern.MatchString(id) }
