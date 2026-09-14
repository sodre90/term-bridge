package server

import (
	"context"
	"io"

	"github.com/sodre90/cmux-bridge/internal/host/cmuxhost"
	"github.com/sodre90/cmux-bridge/internal/wire"
)

// ingestEvents drives the server with raw cmux NDJSON, the way the
// production path does via cmuxhost.RunEvents, so the event tests below can
// keep feeding live-captured frames.
func (s *Server) ingestEvents(ctx context.Context, r io.Reader) {
	cmuxhost.Ingest(ctx, r, func(f wire.EventFrame) { s.onEvent(ctx, f) })
}
