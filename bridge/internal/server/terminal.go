package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sodre90/term-bridge/internal/host"
	"github.com/sodre90/term-bridge/internal/httpjson"
	"github.com/sodre90/term-bridge/internal/metrics"
	"github.com/sodre90/term-bridge/internal/wire"
)

// deviceLogID returns the last 6 hex characters of a device ID (itself the
// full SHA-256 hash of a bearer token, see auth.Device.TokenHash) -- enough
// to correlate log lines for one device without ever logging the full hash,
// mirroring auth.Device.HashSuffix.
func deviceLogID(deviceID string) string {
	if len(deviceID) < 6 {
		return deviceID
	}
	return deviceID[len(deviceID)-6:]
}

func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		httpjson.Error(w, http.StatusBadRequest, "missing surface id")
		return
	}
	var deviceID string
	if s.sessions != nil {
		// Status codes match internal/server/encryption.go's
		// encryptionMiddleware: a missing header is 401 unknown_device, a
		// present-but-unrecognized device id is 409 not_paired (see the
		// spec's error-handling section) — the two point the app at
		// different recovery UX.
		deviceID = r.Header.Get("X-Device-ID")
		if deviceID == "" {
			httpjson.Error(w, http.StatusUnauthorized, "unknown_device")
			return
		}
		if _, ok := s.sessions.SharedSecret(deviceID); !ok {
			httpjson.Error(w, http.StatusConflict, "not_paired")
			return
		}
	}
	// Compression is negotiated both ways. The app asks with ?deflate=1 and
	// arms its side only on seeing the confirming response header, so all four
	// app/bridge version pairings work: an old bridge never sends the header
	// and a new app stays uncompressed, while a new bridge never compresses for
	// an old app that did not ask. Without the confirmation half, a new app
	// against an old bridge would read the JSON's leading '{' as a codec tag and
	// drop every frame.
	//
	// Only meaningful with encryption on: compression rides inside the sealed
	// payload, and the plaintext branch of [writeTerminalFrame] has no tag byte
	// to carry it.
	// Delta frames are negotiated the same way and for the same reason: an
	// older app given a frame with scrollback_spans left out would decode the
	// absent field as an EMPTY scrollback and lose its pan-up history.
	deflate := s.sessions != nil && r.URL.Query().Get("deflate") == "1"
	delta := r.URL.Query().Get("delta") == "1"
	// Streaming compression is a third negotiated capability rather than part of
	// ?deflate=1, because it changes what a frame is: chunks of one stream can
	// only be read in order and only by a decoder that saw every chunk before
	// them. An app that asked for deflate alone must keep getting standalone
	// frames. Built before the upgrade so a failure here simply leaves the
	// socket on per-frame deflate instead of failing the connection.
	var stream *wire.StreamEncoder
	if deflate && r.URL.Query().Get("stream") == "1" {
		var err error
		if stream, err = wire.NewStreamEncoder(); err != nil {
			slog.Warn("terminal: streaming compression unavailable, falling back to per-frame", "surface_id", id, "err", err)
			stream = nil
		}
	}
	// Unlike deflate and delta this needs no confirming header: the client
	// decodes frames the same way whatever the interval, so there is nothing
	// for it to arm or leave disarmed. An old bridge simply ignores it.
	pollInterval := terminalPollInterval(r.URL.Query().Get("poll_ms"), s.terminalPoll)
	upgradeHeader := http.Header{}
	if deflate {
		upgradeHeader.Set(deflateHeader, "1")
	}
	if delta {
		upgradeHeader.Set(deltaHeader, "1")
	}
	if stream != nil {
		upgradeHeader.Set(streamHeader, "1")
	}
	c, err := upgrader.Upgrade(w, r, upgradeHeader)
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	c.SetReadLimit(terminalUpReadLimit)
	start := time.Now()
	// The negotiated capabilities decide what an open pane costs, so they are
	// worth having in the log: a socket that is quietly falling back to whole
	// uncompressed frames looks identical to a cheap one from the outside.
	slog.Info("terminal: connected", "surface_id", id, "device", deviceLogID(deviceID),
		"deflate", deflate, "delta", delta, "shared_window", stream != nil, "poll", pollInterval)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if deviceID != "" {
		defer s.sockets.track(deviceID, cancel)()
	}

	// gorilla/websocket allows only one concurrent writer per connection. The
	// poll loop below and terminalReadLoop's ack writes both write to c, so
	// every write goes through this mutex-guarded helper instead of calling
	// writeTerminalFrame directly.
	//
	// The lock has to cover compression as well as the write, not just the
	// write: a stream encoder's chunks must reach the wire in the order it
	// produced them, since each one is only readable after the one before it.
	// Keep encode and send inside the same critical section.
	var writeMu sync.Mutex
	write := func(fr wire.TerminalDown) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return s.writeTerminalFrame(c, deviceID, fr, deflate, stream)
	}

	// Initial full replay.
	fr, err := s.fetchReplay(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("terminal: initial replay failed", "surface_id", id, "dur_ms", time.Since(start).Milliseconds(), "err", err)
			closeIfSurfaceGone(c, err)
		}
		return
	}
	fr.Type = "replay"
	// Style ids get their stable identity before anything downstream compares
	// bytes: the fingerprint that decides whether to send at all, and the delta
	// encoder that decides what to leave out, both only work on a grid whose
	// unchanged parts are unchanged bytes. See styleTable.
	styles := newStyleTable()
	fr.Grid = styles.canonicalise(fr.Grid)
	if err := write(fr); err != nil {
		slog.Warn("terminal: initial write failed", "surface_id", id, "dur_ms", time.Since(start).Milliseconds(), "err", err)
		return
	}
	// The replay always goes out whole -- it is what a reconnecting app
	// rebuilds from -- but it primes the encoder, so the first output frame can
	// already leave the scrollback out.
	deltas := newDeltaEncoder()
	deltas.strip(fr.Grid)
	// cmux's top-level seq (and render_grid.state_seq) is always 0, so we can't
	// gate on it — instead we forward whenever the render-grid content changes,
	// ignoring the bookkeeping counters that change on their own (see
	// [gridFingerprint]).
	lastFingerprint := gridFingerprint(fr.Grid)

	// Output poll loop is the sole writer after the initial replay. Besides
	// the ticker, an input nudge (below) triggers an immediate replay: without
	// it, every forwarded keystroke/swipe waited out the remaining tick before
	// its effect became visible, which made remote scrolling feel seconds
	// behind the finger even though the PTY had already scrolled.
	nudge := make(chan struct{}, 1)
	go s.terminalReadLoop(ctx, cancel, c, id, deviceID, write, nudge)

	outage := newReplayOutage(s.replayGrace)

	poll := func() bool {
		next, err := s.fetchReplay(ctx, id)
		if err != nil {
			// A cancelled ctx means the connection is already closing
			// (terminalReadLoop's disconnect handler called cancel,
			// which SIGKILLs any in-flight `cmux rpc` subprocess via
			// exec.CommandContext) -- that's an expected side effect
			// of the disconnect already logged by the read loop, not
			// a genuine RPC failure worth alarming about.
			if ctx.Err() != nil {
				return false
			}
			if host.IsNotFound(err) {
				slog.Warn("terminal: surface is gone", "surface_id", id, "dur_ms", time.Since(start).Milliseconds(), "err", err)
				closeIfSurfaceGone(c, err)
				return false
			}
			metrics.TerminalReplayFailuresTotal.Add(1)
			if !outage.ongoing() {
				slog.Warn("terminal: replay failing, holding the pane on its last grid",
					"surface_id", id, "grace", s.replayGrace, "err", err)
			}
			if outage.keepWaiting() {
				return true
			}
			metrics.TerminalReplayGaveUpTotal.Add(1)
			slog.Warn("terminal: giving up, cmux answered no replay for the whole grace window",
				"surface_id", id, "grace", s.replayGrace, "err", err)
			return false
		}
		if down := outage.recovered(); down > 0 {
			slog.Info("terminal: replay working again", "surface_id", id, "down_for", down.Round(time.Second))
		}
		next.Grid = styles.canonicalise(next.Grid)
		fingerprint := gridFingerprint(next.Grid)
		if bytes.Equal(fingerprint, lastFingerprint) {
			return true
		}
		lastFingerprint = fingerprint
		next.Type = "output"
		// Fingerprinted on the whole grid above, stripped only now: what the
		// app must be told about is any change to the grid, not just to the
		// blocks that survive stripping.
		if delta {
			next.Grid, next.Unchanged = deltas.strip(next.Grid)
		}
		if err := write(next); err != nil {
			slog.Warn("terminal: output write failed", "surface_id", id, "dur_ms", time.Since(start).Milliseconds(), "err", err)
			return false
		}
		return true
	}
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("terminal: closed", "surface_id", id, "dur_ms", time.Since(start).Milliseconds())
			return
		case <-t.C:
			if !poll() {
				return
			}
		case <-nudge:
			// Coalesce any nudges that piled up while this replay was being
			// considered; one immediate refresh serves them all.
			for drained := true; drained; {
				select {
				case <-nudge:
				default:
					drained = false
				}
			}
			if !poll() {
				return
			}
		}
	}
}

// volatileGridFields are the render-grid keys cmux advances on its own clock,
// whether or not anything on screen changed. Both are plain counters the app
// never reads.
//
// They are what made the poll loop's unchanged-grid check dead code: measured
// against a live idle pane (cmux-app-2nj), three consecutive replays 400ms
// apart differed in these two fields and in nothing else -- every other key,
// including all 143KB of scrollback_spans, was byte-identical. So a pane
// sitting at a shell prompt re-sent its whole ~187KB grid four times a second
// to deliver two incrementing integers.
var volatileGridFields = []string{"render_revision", "terminal_theme_revision"}

// stickyGridFields are the render-grid blocks a client can carry over from the
// frame before, so a frame that did not change them need not repeat them.
//
// "Sticky" is about repetition, not staleness: [deltaEncoder.strip] omits a
// block only when its bytes equal the ones this socket last sent, so a block
// that changes is always sent again. A client that carries the named blocks
// forward therefore holds exactly what the bridge holds.
//
// That is why styles and modes belong here. An earlier version of this list
// left them out for fear of dressing rows in stale colours or spelling the key
// bar's arrows against a mode the pane had since dropped -- but neither can
// happen when a changed block is never omitted. Measured on a live pane
// (cmux-app-8wk), both are byte-identical between consecutive replays and
// together cost 233B of an 844B compressed frame.
//
// What stays out is anything a client cannot safely carry: the visible
// row_spans, which change on nearly every frame anyway, and the counters in
// [volatileGridFields].
var stickyGridFields = []string{
	"scrollback_spans",
	"terminal_theme",
	"terminal_config_theme",
	"styles",
	"modes",
}

// deltaEncoder remembers the sticky blocks one socket has already sent, so the
// next frame can leave the unchanged ones out. Per-socket, never shared: a
// reconnect builds a fresh one and its first frame is a full replay again.
type deltaEncoder struct{ sent map[string][]byte }

func newDeltaEncoder() *deltaEncoder { return &deltaEncoder{sent: map[string][]byte{}} }

// strip returns grid without the sticky blocks this socket last sent
// unchanged, plus the names of the blocks it left out. Priming it with the
// replay frame is what makes the first output frame able to omit anything.
//
// A grid that will not decode is returned whole with nothing omitted, the same
// fail-safe direction as [gridFingerprint]: a redundant block costs bytes, a
// wrongly omitted one costs correctness.
func (d *deltaEncoder) strip(grid json.RawMessage) (json.RawMessage, []string) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(grid, &fields); err != nil {
		return grid, nil
	}
	var omitted []string
	for _, k := range stickyGridFields {
		v, ok := fields[k]
		if !ok {
			continue
		}
		if prev, seen := d.sent[k]; seen && bytes.Equal(prev, v) {
			delete(fields, k)
			omitted = append(omitted, k)
			continue
		}
		d.sent[k] = bytes.Clone(v)
	}
	if len(omitted) == 0 {
		return grid, nil
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return grid, nil
	}
	return out, omitted
}

// gridFingerprint reduces a render grid to a value that compares equal when
// the grid's content is unchanged, by dropping [volatileGridFields]. Only the
// top level is decoded -- every value below it stays raw -- so this costs one
// shallow pass rather than parsing the span arrays.
//
// A grid that will not decode is returned verbatim, which compares exactly as
// it did before this existed. That is the same direction every failure here
// takes: an unrecognised grid, or a future cmux counter not in the list above,
// costs a redundant frame, never a suppressed one. Dropping a field the app
// DOES render would be the unsafe direction, which is why this is a list of
// known-volatile keys rather than a list of known-meaningful ones.
func gridFingerprint(grid json.RawMessage) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(grid, &fields); err != nil {
		return grid
	}
	for _, k := range volatileGridFields {
		delete(fields, k)
	}
	// json.Marshal sorts map keys, so equal content always yields equal bytes.
	out, err := json.Marshal(fields)
	if err != nil {
		return grid
	}
	return out
}

// terminalReplayGrace bounds how long the poll loop will sit on a stale grid
// waiting for a cmux that cannot answer mobile.terminal.replay.
//
// Chosen from the measured episode lengths (cmux-app-8a0): of the nine multi-
// failure episodes in 28 days, eight spanned under 7 minutes and one spanned 20.
// So this waits out all but the outlier, and the outlier degrades to the old
// behaviour rather than to something worse.
//
// Longer would be defensible too -- an idle socket costs nothing, while giving
// up costs a reconnect and a fresh full replay against the process that is
// already too slow to serve one. What sets an upper bound at all is that
// nothing else can notice a client which vanished mid-outage: the read loop
// only learns of it from a failed read, and the write path only from a write
// there is currently nothing to make.
const terminalReplayGrace = 10 * time.Minute

// replayOutage tracks one run of consecutive replay failures on a single
// terminal socket, so the poll loop can tell a first failure from a continuing
// one and a recovery from an ordinary success.
type replayOutage struct {
	now   func() time.Time
	grace time.Duration
	since time.Time
}

func newReplayOutage(grace time.Duration) *replayOutage {
	return &replayOutage{now: time.Now, grace: grace}
}

func (o *replayOutage) ongoing() bool { return !o.since.IsZero() }

// keepWaiting records a failure and reports whether the socket is still worth
// holding. The first failure starts the outage, so this is what decides when
// the grace has run out.
func (o *replayOutage) keepWaiting() bool {
	if !o.ongoing() {
		o.since = o.now()
	}
	return o.now().Sub(o.since) < o.grace
}

// recovered ends the outage and reports how long it ran. A zero duration means
// there was no outage in progress, which is the ordinary case on every
// successful poll.
func (o *replayOutage) recovered() time.Duration {
	if !o.ongoing() {
		return 0
	}
	down := o.now().Sub(o.since)
	o.since = time.Time{}
	return down
}

// closeWriteTimeout bounds the close control frame's write. Short on purpose:
// the connection is being abandoned either way, so waiting on a peer that has
// already stopped reading buys nothing.
const closeWriteTimeout = time.Second

// closeIfSurfaceGone answers a replay failure that means "this surface does not
// exist" with wire.CloseSurfaceGone, so the client stops reconnecting to an id
// cmux will never have again. Anything else -- a timeout, a cmux restart, a
// transport fault -- is left to close ordinarily and be retried, because it can
// succeed next time.
//
// The close frame goes out via WriteControl, which gorilla permits concurrently
// with the poll loop's data writes, so it does not take writeMu. A failure to
// send it is ignored on purpose: the socket is already going away, and the
// pre-existing behaviour (client retries) is the fallback.
func closeIfSurfaceGone(c *websocket.Conn, err error) {
	if !host.IsNotFound(err) {
		return
	}
	_ = c.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(wire.CloseSurfaceGone, ""),
		time.Now().Add(closeWriteTimeout),
	)
}

// deflateHeader is the response header the bridge sets on the 101 to confirm
// it accepted a client's ?deflate=1 request. Mirrored in the app as
// TerminalSocket's DEFLATE_HEADER.
const deflateHeader = "X-Cmux-Deflate"

// deltaHeader is the matching confirmation for ?delta=1. Mirrored in the app as
// TerminalSocket's DELTA_HEADER.
const deltaHeader = "X-Cmux-Delta"

// streamHeader confirms ?stream=1, the request to compress the socket's frames
// against one shared window instead of each on its own. Mirrored in the app as
// TerminalSocket's STREAM_HEADER.
const streamHeader = "X-Cmux-Deflate-Stream"

// The bounds a client's ?poll_ms= request is held to. The floor is the old
// fixed rate: a client may spend more of its data allowance than the default
// but not less of the Mac's, since every tick is a cmux replay. The ceiling is
// where a pane stops feeling live at all.
const (
	minTerminalPoll = 250 * time.Millisecond
	maxTerminalPoll = 10 * time.Second
)

// terminalPollInterval reads a client's requested poll interval, clamped to
// [minTerminalPoll, maxTerminalPoll]. Anything absent or unreadable falls back
// to the server default rather than failing the connection: an interval is a
// preference, not a correctness requirement, and an old client sends none.
//
// Slowing the tick does not slow typing. Input nudges an immediate replay (see
// the nudge channel in terminalReadLoop), so this governs only how quickly
// output the user did not type appears.
func terminalPollInterval(raw string, fallback time.Duration) time.Duration {
	ms, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return min(max(time.Duration(ms)*time.Millisecond, minTerminalPoll), maxTerminalPoll)
}

// writeTerminalFrame sends fr as a plain JSON text frame when encryption is
// disabled (s.sessions == nil), or as a binary e2e-encrypted frame otherwise.
// When deflate is set, the sealed payload carries a wire codec tag and is
// compressed where that helps -- see wire.EncodePayload. A non-nil stream
// compresses against the socket's shared window instead, which is far smaller
// but requires the caller to serialize encoding with sending.
//
// A stream that fails mid-socket falls back to a standalone frame. That frame
// arrives, but it leaves the app's decoder short of everything the encoder
// still believes it has, so the app will fail the next chunk and resync -- one
// visible reconnect rather than a silently wrong pane.
func (s *Server) writeTerminalFrame(c *websocket.Conn, deviceID string, fr wire.TerminalDown, deflate bool, stream *wire.StreamEncoder) error {
	if s.sessions == nil {
		return c.WriteJSON(fr)
	}
	raw, err := json.Marshal(fr)
	if err != nil {
		return err
	}
	switch {
	case stream != nil:
		chunk, err := stream.Encode(raw)
		if err != nil {
			slog.Warn("terminal: stream compression failed, sending the frame standalone", "err", err)
			raw = wire.EncodePayload(raw)
		} else {
			raw = chunk
		}
	case deflate:
		raw = wire.EncodePayload(raw)
	}
	frame, err := s.sessions.EncryptFrame(deviceID, raw)
	if err != nil {
		return err
	}
	return c.WriteMessage(websocket.BinaryMessage, frame)
}

// terminalUpReadLimit bounds one message from the phone. Without it the
// socket buffered whatever the peer chose to send before anything looked at
// it -- harmless while the largest legitimate frame was a paste of a few
// KB, not once an attachment of attachmentMaxBytes is legitimate. That
// attachment arrives as base64 (4/3) inside a JSON envelope inside an e2e
// frame; 64 KB covers the latter two many times over. A message past the
// limit ends the socket the way a decrypt failure does.
const terminalUpReadLimit = attachmentMaxBytes/3*4 + 64<<10

func (s *Server) terminalReadLoop(ctx context.Context, cancel context.CancelFunc, c *websocket.Conn, id, deviceID string, write func(wire.TerminalDown) error, nudge chan<- struct{}) {
	defer cancel()
	for {
		var up wire.TerminalUp
		if s.sessions == nil {
			if err := c.ReadJSON(&up); err != nil {
				slog.Warn("terminal: read loop ended", "surface_id", id, "err", err)
				return
			}
		} else {
			_, raw, err := c.ReadMessage()
			if err != nil {
				slog.Warn("terminal: read loop ended", "surface_id", id, "err", err)
				return
			}
			plain, err := s.sessions.DecryptFrame(deviceID, raw)
			if err != nil {
				slog.Warn("terminal: decrypt failed", "surface_id", id, "device", deviceLogID(deviceID), "err", err)
				metrics.E2EDecryptFailuresTotal.Add("terminal_frame", 1)
				return
			}
			if err := json.Unmarshal(plain, &up); err != nil {
				slog.Warn("terminal: bad frame json", "surface_id", id, "err", err)
				return
			}
		}
		// The PTY is about to change (keystroke, paste, page scroll): ask the
		// poll loop for an immediate replay so the user sees the effect now
		// rather than on the next tick. Non-blocking: a flood of inputs
		// leaves at most one pending nudge, which is the point.
		nudgePoll := func() {
			select {
			case nudge <- struct{}{}:
			default:
			}
		}
		var rpcErr error
		switch up.Type {
		case "input":
			rpcErr = s.host.Input(ctx, id, up.Text)
			nudgePoll()
		case "paste":
			rpcErr = s.host.Paste(ctx, id, up.Text)
			nudgePoll()
		case "attach":
			rpcErr = s.attachImage(ctx, id, up)
			nudgePoll()
		case "resize":
			rpcErr = s.host.Resize(ctx, id, up.Columns, up.Rows)
		default:
			continue
		}
		if up.Seq == 0 {
			continue // no seq set (shouldn't happen from the app) -- nothing to ack.
		}
		if err := write(wire.TerminalDown{Type: "ack", Seq: up.Seq, Ok: rpcErr == nil, Reason: attachRefusalReason(rpcErr)}); err != nil {
			slog.Warn("terminal: ack write failed", "surface_id", id, "err", err)
			return
		}
	}
}

// replayTimeout is what mobile.terminal.replay gets instead of the cmux
// package's default, because it is categorically heavier than every other
// call the bridge makes: it serialises a whole render grid, measured at
// 1.0-4.45s for 240-390KB per surface against a 5s default, and 6.9-10.4s
// once cmux itself was busy (cmux-app-69y).
//
// This used to be the only defence against the retry storm -- a failure closed
// the socket, the phone reconnected, and the reconnect issued another full
// replay against the cmux that was already too slow to serve one -- so the
// deadline was lengthened until failures got rare. The poll loop now holds the
// socket through a failure instead (see [terminalReplayGrace]), which removes
// the storm rather than making it rarer, and leaves this as what it says it is:
// how long one replay may take.
const replayTimeout = 20 * time.Second

// fetchReplay asks the host for the surface's render grid and returns a
// wire.TerminalDown (Type unset) holding it and its dimensions.
func (s *Server) fetchReplay(ctx context.Context, id string) (wire.TerminalDown, error) {
	ctx, cancel := context.WithTimeout(ctx, replayTimeout)
	defer cancel()
	replay, err := s.host.Replay(ctx, id)
	if err != nil {
		return wire.TerminalDown{}, err
	}
	return wire.TerminalDown{
		Grid:    replay.Grid,
		Columns: replay.Columns,
		Rows:    replay.Rows,
		Seq:     replay.Seq,
	}, nil
}
