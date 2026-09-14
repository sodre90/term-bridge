// Package host is the seam between the bridge's HTTP/WebSocket server and
// whatever terminal multiplexer it is fronting. The server speaks only in the
// terms below; an implementation (internal/host/cmuxhost today) owns every
// detail of one backend's RPC names, payload shapes and quirks.
//
// Types the app receives on the wire (wire.Workspace, wire.TerminalDown's
// grid, the pending feed body) are produced by the implementation in the
// exact form the app already reads, so switching backends is invisible to
// the phone beyond the Capabilities it advertises.
package host

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/sodre90/term-bridge/internal/wire"
)

// ErrUnsupported is returned by an operation the backend has no analog for
// (see Capabilities). The server answers it as a client error, not a
// backend failure.
var ErrUnsupported = errors.New("unsupported by this host")

// ErrPromptGone is FeedReply refusing to answer a prompt that is no longer
// (or not yet) on screen -- someone else answered it, or the reply named a
// prompt this host is not holding. The server answers it as a conflict so
// the phone drops the stale item instead of retrying.
var ErrPromptGone = errors.New("prompt is no longer on screen")

// ErrMalformed wraps a backend reply that arrived but could not be read,
// as distinct from the backend being unreachable.
var ErrMalformed = errors.New("malformed backend response")

// Capabilities says which optional operations a host supports, so the app
// can hide affordances the backend cannot honour.
type Capabilities struct {
	// Tabs: a pane can hold several surfaces (cmux tabs); CreateTab works.
	Tabs bool
	// Feed: PendingFeed/FeedReply carry structured agent prompts.
	Feed bool
}

// Kind names a backend on the wire (wire.HostInfo.Kind).
const (
	KindCmux = "cmux"
	KindTmux = "tmux"
)

// Pane is one pane of a workspace with its frame in the backend's own
// units; the server normalises frames onto the unit square itself. A frame
// of all zeros means the backend has no geometry for the pane yet.
type Pane struct {
	ID                string
	Focused           bool
	SurfaceIDs        []string
	SelectedSurfaceID string
	Frame             Frame
}

// Frame is a pane's rectangle in backend units (pixels for cmux).
type Frame struct {
	X, Y, Width, Height float64
}

// Replay is one snapshot of a surface: the render grid in the
// cmux.render-grid.v1 form the app renders, plus its dimensions and the
// backend's own change counter.
type Replay struct {
	Grid    json.RawMessage
	Columns int
	Rows    int
	Seq     int64
}

// Host is everything the server needs from a terminal backend. Every
// mutation names its target by id; an implementation must never fall back
// to "whatever is focused" when given none.
type Host interface {
	// Kind is one of the Kind* constants.
	Kind() string
	Capabilities() Capabilities
	// ValidID reports whether id has the shape of one of this host's
	// workspace/surface ids, so the server can refuse a malformed one before
	// any backend call.
	ValidID(id string) bool

	// ListWorkspaces reports each workspace's CWD in the same canonical
	// form PendingFeed uses for an item's cwd, because that string equality
	// is how the server and the app tie a prompt to its workspace.
	ListWorkspaces(ctx context.Context) ([]wire.Workspace, error)
	ListPanes(ctx context.Context, workspaceID string) ([]Pane, error)

	CreateWorkspace(ctx context.Context, cwd, title string) (wire.CreateWorkspaceResponse, error)
	SplitPane(ctx context.Context, surfaceID, direction string) (wire.CreatePaneResponse, error)
	CreateTab(ctx context.Context, workspaceID, paneID string) (wire.CreatePaneResponse, error)
	SelectWorkspace(ctx context.Context, workspaceID string) error
	FocusSurface(ctx context.Context, surfaceID string) error
	RenameWorkspace(ctx context.Context, workspaceID, title string) error
	CloseWorkspace(ctx context.Context, workspaceID string) error
	CloseSurface(ctx context.Context, surfaceID string) error

	Replay(ctx context.Context, surfaceID string) (Replay, error)
	Input(ctx context.Context, surfaceID, text string) error
	Paste(ctx context.Context, surfaceID, text string) error
	Resize(ctx context.Context, surfaceID string, columns, rows int) error

	// PendingFeed returns the backend's pending agent prompts as the JSON
	// body the app reads (items[] with request_id, kind, cwd, question
	// structure...), with every item's cwd in the form ListWorkspaces
	// reports wire.Workspace.CWD (cmux reports the two through different
	// RPCs that disagree on symlinks; tmux's pane_current_path is the
	// kernel's resolved path and the hook feed reuses it as the item cwd).
	PendingFeed(ctx context.Context) (json.RawMessage, error)
	// FeedReply answers the prompt requestID of the given wire kind
	// ("permissionRequest" | "question" | "exitPlan") with the kind's own
	// params (e.g. {"mode": "once"}).
	FeedReply(ctx context.Context, kind, requestID string, params map[string]any) error

	// RunEvents streams the backend's feed and notification events into sink
	// until ctx ends, reconnecting on its own after a stream failure.
	RunEvents(ctx context.Context, sink func(wire.EventFrame))
}

// notFounder is satisfied by a backend error that says the named object
// does not exist -- terminal for that id, unlike a transport failure.
type notFounder interface {
	NotFound() bool
}

// IsNotFound reports whether err is the backend saying the object is gone.
func IsNotFound(err error) bool {
	var nf notFounder
	return errors.As(err, &nf) && nf.NotFound()
}
