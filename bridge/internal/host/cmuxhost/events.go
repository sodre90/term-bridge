package cmuxhost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/sodre90/term-bridge/internal/backoff"
	"github.com/sodre90/term-bridge/internal/wire"
)

// eventsSilenceLimit is how long the events subprocess may go without
// writing a line before it is killed and restarted. cmux heartbeats every
// subscription every 15s, so a stream this quiet is dead however alive the
// process looks: on 2026-09-17 a `cmux events --reconnect` child was found
// 41h silent, holding no socket to cmux, and nothing had restarted it -- no
// push reached any phone for those 41h.
const eventsSilenceLimit = 90 * time.Second

// RunEvents keeps a `cmux events --reconnect` stream flowing into sink until
// ctx ends, restarting the subprocess with backoff whenever it dies or falls
// silent for eventsSilenceLimit.
func (h *Host) RunEvents(ctx context.Context, sink func(wire.EventFrame)) {
	h.runEvents(ctx, sink, eventsSilenceLimit)
}

func (h *Host) runEvents(ctx context.Context, sink func(wire.EventFrame), silenceLimit time.Duration) {
	retry := backoff.New(time.Second, 30*time.Second)
	for ctx.Err() == nil {
		cmd, pipe, err := h.client.Events(ctx,
			"--category", "feed", "--category", "notification", "--reconnect")
		if err != nil {
			backoff.Sleep(ctx, retry.Next())
			continue
		}
		stream := &lastReadReader{r: pipe}
		stream.touch()
		stopWatchdog := killWhenSilent(ctx, stream, silenceLimit, func() {
			slog.Warn("cmuxhost: events stream silent, restarting it", "silence_limit", silenceLimit)
			_ = cmd.Process.Kill()
			// Ingest is blocked in Read; killing the child alone does not
			// end that if anything it spawned still holds the pipe open.
			_ = pipe.Close()
		})
		Ingest(ctx, stream, sink)
		stopWatchdog()
		_ = cmd.Wait()
		if ctx.Err() == nil {
			backoff.Sleep(ctx, retry.Next())
		}
	}
}

// lastReadReader records when r last yielded bytes, so a watchdog can tell a
// stream that is quiet from one that is merely between lines.
type lastReadReader struct {
	r        io.Reader
	lastRead atomic.Int64
}

func (l *lastReadReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if n > 0 {
		l.touch()
	}
	return n, err
}

func (l *lastReadReader) touch() { l.lastRead.Store(time.Now().UnixNano()) }

func (l *lastReadReader) silentFor() time.Duration {
	return time.Since(time.Unix(0, l.lastRead.Load()))
}

// killWhenSilent calls kill once stream has yielded nothing for limit. The
// returned func stops the watchdog; call it as soon as the stream ends so a
// stream that ended on its own is never "killed" a second time.
func killWhenSilent(ctx context.Context, stream *lastReadReader, limit time.Duration, kill func()) (stop func()) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(limit / 4)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				if stream.silentFor() > limit {
					kill()
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

// Ingest reads NDJSON cmux event frames from r, classifies each, and hands
// the ones the app should see to sink. Exported so server tests can drive
// the server with live-captured cmux frames.
func Ingest(ctx context.Context, r io.Reader, sink func(wire.EventFrame)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if f, ok := classify(m); ok {
			sink(f)
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		// os.ErrClosed is the silence watchdog closing the pipe on purpose.
		// Otherwise most commonly bufio.ErrTooLong: a single event line exceeded the
		// scanner's 4MB cap. That kills the whole stream (Scan stops, not just
		// the one line), so RunEvents' restart-with-backoff is what actually
		// recovers -- this log exists so that isn't silent.
		slog.Error("cmuxhost: events stream ended abnormally", "err", err)
	}
}

// classify maps a raw cmux event frame to a wire.EventFrame. The bool is
// false for frames the app should not see (acks, surface/pane/workspace
// churn).
func classify(m map[string]any) (wire.EventFrame, bool) {
	switch str(m, "type") {
	case "ack":
		return wire.EventFrame{}, false
	case "heartbeat":
		return wire.EventFrame{Type: "heartbeat"}, true
	}
	name := str(m, "name")
	payload, _ := m["payload"].(map[string]any)
	wsID := firstNonEmpty(str(m, "workspace_id"), str(payload, "workspace_id"))
	surfID := firstNonEmpty(str(m, "surface_id"), str(payload, "surface_id"))

	switch str(m, "category") {
	case "feed":
		hookEvent := str(payload, "hook_event_name")
		return wire.EventFrame{
			Type:           "feed",
			Name:           name,
			Kind:           hookEvent,
			FeedID:         firstNonEmpty(str(m, "id"), str(payload, "id")),
			WorkspaceID:    firstNonEmpty(str(payload, "workspace_id"), wsID),
			SurfaceID:      surfID,
			Title:          attentionLabel(payload),
			NeedsAttention: needsAttention(hookEvent, str(payload, "phase")),
		}, true
	case "notification":
		// Note: cmux redacts notification title/body in the event stream, so we
		// forward these as informational only and do not set NeedsAttention.
		return wire.EventFrame{
			Type:        "notification",
			Name:        name,
			WorkspaceID: wsID,
			SurfaceID:   surfID,
			Title:       str(payload, "title"),
		}, true
	}
	return wire.EventFrame{}, false
}

// needsAttention reports whether a cmux feed item is a blocking agent prompt
// awaiting the user. cmux carries the Claude Code hook in payload.hook_event_name
// and emits each item twice (phase "received" then "completed"); we alert once,
// on "received". Notification covers permission prompts and idle "waiting for
// input"; AskUserQuestion is an explicit blocking choice.
func needsAttention(hookEvent, phase string) bool {
	if phase != "received" {
		return false
	}
	switch hookEvent {
	case "Notification", "AskUserQuestion":
		return true
	}
	return false
}

// attentionLabel returns the best human label for a feed item. cmux redacts the
// prompt text, so the cwd basename ("cmux-app") tells the user which agent is
// waiting; a real payload title is preferred if cmux ever provides one. This is
// only the cheap, synchronous fallback used before the server's workspace
// lookup runs (or if that lookup fails).
func attentionLabel(payload map[string]any) string {
	if t := str(payload, "title"); t != "" {
		return t
	}
	if cwd := str(payload, "cwd"); cwd != "" {
		return filepath.Base(cwd)
	}
	return ""
}

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
