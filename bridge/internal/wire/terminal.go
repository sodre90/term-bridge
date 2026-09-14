package wire

import "encoding/json"

// TerminalDown is a server->client terminal message. Grid carries the cmux
// render-grid object (format "cmux.render-grid.v1") verbatim; the app renders it
// as a styled cell grid. "ack" echoes back an input/paste/resize message's Seq
// once its RPC has run, with Ok reflecting whether that RPC actually succeeded.
type TerminalDown struct {
	Type    string          `json:"type"` // "replay" | "output" | "ack"
	Grid    json.RawMessage `json:"grid,omitempty"`
	Columns int             `json:"columns,omitempty"`
	Rows    int             `json:"rows,omitempty"`
	Seq     int64           `json:"seq,omitempty"`
	// Ok is only meaningful for "ack" frames; not omitempty because a failed
	// RPC's ack (Ok: false) must be distinguishable on the wire from an "ok"
	// field that was never set.
	Ok bool `json:"ok"`
	// Reason says why an "ack" is not Ok when the bridge itself refused the
	// message rather than cmux failing it -- today only attachments have
	// such refusals (see server.attachRefusalReason). Empty on every Ok ack
	// and on a plain RPC failure, so an app that does not know a reason
	// shows what it always showed.
	Reason string `json:"reason,omitempty"`
	// Unchanged names the render-grid blocks left out of Grid because they are
	// identical to the ones this socket already sent; the client carries its
	// own copy forward. Only ever set on "output" frames, and only for a client
	// that negotiated it -- see the delta handshake in server/terminal.go.
	//
	// It lives here rather than inside Grid on purpose: which blocks the bridge
	// chose to omit is the bridge's protocol, not cmux's data, and RenderGrid
	// stays a faithful mirror of cmux.render-grid.v1.
	//
	// An explicit list rather than "absent means unchanged": absent is already
	// how an EMPTY block arrives, so without this a cleared scrollback and an
	// unchanged one would be the same frame.
	Unchanged []string `json:"unchanged,omitempty"`
	// RowsChanged lists the visible rows whose spans Grid's row_spans carries;
	// every other row is as this socket last sent it, and a listed row with no
	// spans has emptied. Absent means row_spans is whole, so a client that did
	// not ask for it (?rows=1) is never handed a partial block. Like Unchanged,
	// this is the bridge's protocol, not cmux's data.
	RowsChanged []int `json:"rows_changed,omitempty"`
}

// CloseSurfaceGone is the WebSocket close code WS /terminal/{id} uses to say
// the surface no longer exists, so the client stops reconnecting to an id cmux
// will never have again (cmux-app-34c). Mirrored in the app as
// TerminalSocket's CLOSE_SURFACE_GONE.
//
// A close code rather than a TerminalDown field, deliberately: it rides the
// transport, so it needs no e2e frame (the socket may be closing before any
// session is usable) and a client that ignores it behaves exactly as before.
// It is sent with an empty reason string -- the number says everything the
// deliberately blind relay could not already infer from the connection ending.
//
// 4404 is in the 4000-4999 range RFC 6455 reserves for private application use.
const CloseSurfaceGone = 4404

// TerminalUp is a client->server terminal message. Seq is a client-assigned
// monotonic id echoed back in the matching "ack" TerminalDown.
//
// An "attach" carries an image the bridge writes to disk and pastes the path
// of into the pane: Image is the file's bytes, base64; Name is the phone's
// idea of a name, a hint the bridge may log but never uses to form a path.
type TerminalUp struct {
	Type    string `json:"type"` // "input" | "paste" | "resize" | "attach"
	Text    string `json:"text,omitempty"`
	Columns int    `json:"columns,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Seq     int64  `json:"seq,omitempty"`
	Image   string `json:"image,omitempty"`
	Name    string `json:"name,omitempty"`
}
