// Package tmux is the only seam that talks to tmux. It shells out to the
// documented CLI (`tmux [-S socket] <command> [args]`, chained with `;`) and
// reads the documented `-F` formats and `-C` control-mode notifications;
// nothing here knows tmux's socket protocol or source.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Client invokes the tmux CLI. The zero value uses "tmux" from PATH and the
// default server socket.
type Client struct {
	// Bin is the path to the tmux binary; empty means "tmux".
	Bin string
	// Socket is passed as `-S` when set, for a server on a non-default path.
	Socket string
	// OnReached, if set, is called after a command succeeds. Drives the
	// "last reached the backend" status surface; nil is a no-op.
	OnReached func()
}

// NotFoundError is tmux refusing a target it has no object for ("can't
// find pane: %9"). Terminal for that id: retrying cannot succeed.
type NotFoundError struct {
	Target string
	Stderr string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("tmux: %s", strings.TrimSpace(e.Stderr))
}

// NotFound satisfies host.IsNotFound's interface without importing host.
func (e *NotFoundError) NotFound() bool { return true }

// ErrNoServer is tmux reporting that no server is running on the socket: no
// sessions exist, which the host reads as an empty list rather than an
// outage.
var ErrNoServer = errors.New("tmux: no server running")

// Run executes one tmux command line and returns its stdout. Chain several
// commands in one server turn by passing ";" as its own argument between
// them, exactly as the CLI does.
func (c *Client) Run(ctx context.Context, args ...string) ([]byte, error) {
	return c.run(ctx, nil, args...)
}

// RunWithStdin is Run with data on the command's stdin (load-buffer -).
func (c *Client) RunWithStdin(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	return c.run(ctx, stdin, args...)
}

func (c *Client) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.bin(), c.baseArgs(args)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, classify(args, stderr.String(), err)
	}
	if c.OnReached != nil {
		c.OnReached()
	}
	return stdout.Bytes(), nil
}

// Command returns an unstarted tmux process for a long-lived invocation
// (control mode), with the client's binary and socket applied.
func (c *Client) Command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, c.bin(), c.baseArgs(args)...)
}

func (c *Client) bin() string {
	if c.Bin == "" {
		return "tmux"
	}
	return c.Bin
}

func (c *Client) baseArgs(args []string) []string {
	if c.Socket == "" {
		return args
	}
	return append([]string{"-S", c.Socket}, args...)
}

// classify turns tmux's stderr into the typed errors callers branch on.
// tmux's wording ("can't find window: @3", "no server running on …") is
// documented behaviour the CLI has kept for years, not a parsed protocol.
func classify(args []string, stderr string, err error) error {
	msg := strings.TrimSpace(stderr)
	switch {
	case strings.HasPrefix(msg, "can't find "):
		return &NotFoundError{Target: targetOf(args), Stderr: msg}
	case strings.HasPrefix(msg, "no server running"), strings.HasPrefix(msg, "error connecting to"):
		return fmt.Errorf("%w: %s", ErrNoServer, msg)
	case msg != "":
		return fmt.Errorf("tmux %s: %s", firstCommand(args), msg)
	}
	return fmt.Errorf("tmux %s: %w", firstCommand(args), err)
}

func targetOf(args []string) string {
	for i, a := range args {
		if a == "-t" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func firstCommand(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}
