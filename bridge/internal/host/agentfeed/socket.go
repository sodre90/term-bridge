package agentfeed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/sodre90/term-bridge/internal/wire"
)

// The hooks socket lives in the user's runtime directory, which is already
// owner-only; the directory and socket are made owner-only again anyway.
// Without XDG_RUNTIME_DIR there is no socket, and hooks pass through.
const (
	socketDirName  = "term-bridge"
	socketFileName = "hooks.sock"
)

// SocketPath returns the hooks socket path, or "" when the runtime dir is
// not known.
func SocketPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, socketDirName, socketFileName)
}

// Listen binds the hooks socket at path, replacing a stale one.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// One hook invocation is a few KB; a megabyte is far past any tool_input
// worth mirroring.
const maxEnvelope = 1 << 20

// connDeadline bounds one hook's round trip; Claude Code waits on the hook
// process meanwhile.
const connDeadline = 3 * time.Second

// Serve accepts forwarded hooks on ln until ctx ends. Each connection
// carries one Envelope and is closed once the feed has recorded it -- that
// close is the acknowledgement `term-bridge hook` waits for, so a
// PermissionRequest is never processed before the PreToolUse that precedes
// it. Frames the hook produced go to sink after the close, so a reply they
// trigger finds the prompt Claude draws once the hook returns.
func (f *Feed) Serve(ctx context.Context, ln net.Listener, sink func(wire.EventFrame)) {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("agentfeed: accept", "err", err)
			continue
		}
		go f.serveConn(ctx, conn, sink)
	}
}

func (f *Feed) serveConn(ctx context.Context, conn net.Conn, sink func(wire.EventFrame)) {
	_ = conn.SetDeadline(time.Now().Add(connDeadline))
	var env Envelope
	err := json.NewDecoder(io.LimitReader(conn, maxEnvelope)).Decode(&env)
	var frames []wire.EventFrame
	if err == nil {
		frames, err = f.Handle(ctx, env)
	}
	_ = conn.Close()
	if err != nil {
		slog.Debug("agentfeed: hook dropped", "pane", env.Pane, "err", err)
		return
	}
	for _, fr := range frames {
		sink(fr)
	}
}

// Forward is the client side: it delivers env to the socket at path and
// waits for the agent to close the connection. Errors are for the caller
// to swallow -- a hook must never fail Claude's tool call over the bridge.
func Forward(path string, env Envelope) error {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(connDeadline))
	if err := json.NewEncoder(conn).Encode(env); err != nil {
		return err
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	_, err = io.Copy(io.Discard, conn)
	return err
}
