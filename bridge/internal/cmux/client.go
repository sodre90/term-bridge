// Package cmux is the only seam that talks to cmux. It shells out to the
// documented cmux CLI: `cmux rpc <method> [json]` for control calls and
// `cmux events <args...>` for the NDJSON event stream. The bridge relies on the
// CLI's local auto-resolution of the socket path and password, so no socket
// credentials are handled here.
package cmux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// CodeNotFound is cmux's error code for a surface, workspace or other object
// it does not have. Unlike a transport failure it is terminal for that id:
// retrying the same call can only fail the same way (cmux-app-34c).
const CodeNotFound = "not_found"

// RPCError is a refusal from cmux itself -- a well-formed response saying no --
// as opposed to a failure to reach it. Code is cmux's own error code, which is
// what callers should branch on rather than matching text in Error().
//
// Only the fast-path socket transport produces one. The `cmux rpc` subprocess
// fallback has nothing but the CLI's stderr to go on, so a caller that must
// distinguish codes should not assume every failure is typed.
type RPCError struct {
	Method  string
	Code    string
	Message string
}

// Error keeps the exact wording the untyped fmt.Errorf produced, so log lines
// and any test matching on them are unchanged.
func (e *RPCError) Error() string {
	return fmt.Sprintf("cmux rpc %s: %s: %s", e.Method, e.Code, e.Message)
}

// NotFound satisfies host.IsNotFound's interface without this package having
// to know about host.
func (e *RPCError) NotFound() bool { return e.Code == CodeNotFound }

// IsNotFound reports whether err is cmux telling us the object is gone.
func IsNotFound(err error) bool {
	var rpcErr *RPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == CodeNotFound
}

// Client invokes the cmux CLI. The zero value uses "cmux" from PATH.
type Client struct {
	// Bin is the path to the cmux binary. When empty, "cmux" is used.
	Bin string

	// FastPath, when true, has Rpc try a pool of persistent, authenticated
	// direct connections to cmux's control socket first (see
	// socket_client.go and socket_pool.go), falling back to the `cmux` CLI
	// subprocess only when a connection itself can't be established -- never
	// after a request has actually been sent over one, since retrying an
	// ambiguous send through a different transport risks double-executing
	// non-idempotent input. Off by default so tests built around a fake Bin
	// script (no real cmux socket to reach) get the exact same
	// subprocess-only behavior as before.
	FastPath bool

	// OnReached, if set, is called after Rpc completes successfully (either
	// fast-path socket or subprocess). Used only to drive the
	// "last successfully reached cmux" status surface (cmux-bridge status,
	// see internal/status); nil is a safe no-op default, so every existing
	// caller and test is unaffected.
	OnReached func()

	poolOnce sync.Once
	pool     *socketPool
}

func (c *Client) fastPathPool() *socketPool {
	c.poolOnce.Do(func() { c.pool = newSocketPool(fastPathPoolSize) })
	return c.pool
}

func (c *Client) bin() string {
	if c.Bin == "" {
		return "cmux"
	}
	return c.Bin
}

// Rpc calls a cmux v2 socket method and returns its raw JSON result. When
// FastPath is set, it first tries a pooled persistent socket connection;
// only a connect/auth failure there (nothing sent yet, so no idempotency
// risk) falls back to the `cmux rpc <method> [json(params)]` subprocess
// below. On a subprocess non-zero exit, the returned error includes the
// captured stderr.
func (c *Client) Rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.FastPath {
		if raw, committed, err := c.fastPathPool().rpc(ctx, method, params); committed {
			if err == nil {
				c.markReached()
			}
			return raw, err
		}
	}
	args := []string{"rpc", method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		args = append(args, string(b))
	}
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Env = append(cmd.Environ(), "CMUX_QUIET=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("cmux rpc %s: %w: %s", method, err, stderr.String())
	}
	c.markReached()
	return json.RawMessage(stdout.Bytes()), nil
}

// markReached calls OnReached if set.
func (c *Client) markReached() {
	if c.OnReached != nil {
		c.OnReached()
	}
}

// Events starts `cmux events <args...>` and returns the running command plus a
// reader over its stdout (newline-delimited JSON). Stop it by cancelling ctx and
// closing the reader; the caller is responsible for cmd.Wait.
func (c *Client) Events(ctx context.Context, args ...string) (*exec.Cmd, io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, c.bin(), append([]string{"events"}, args...)...)
	cmd.Env = append(cmd.Environ(), "CMUX_QUIET=1")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return cmd, pipe, nil
}
