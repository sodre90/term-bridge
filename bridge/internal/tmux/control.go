package tmux

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

// Notification is one `%name args` line from a control-mode client, minus
// the reply framing (%begin/%end/%error and the lines between them).
type Notification struct {
	Name string // e.g. "%window-add"
	Args string // the rest of the line, untouched
}

// Watch attaches a control-mode client to session and calls sink for every
// notification until the process exits or ctx ends. The client asks for no
// pane output and no say in window sizes: it exists to learn about
// structural change (windows and sessions coming and going, layouts
// changing), which tmux reports for every session, not only the attached
// one -- windows elsewhere arrive as %unlinked-window-*.
//
// stdin is held open for the life of the process: a control client whose
// stdin closes exits at once, which is how a first probe died. Returns nil
// when tmux ends the attachment itself (%exit), the error otherwise.
func (c *Client) Watch(ctx context.Context, session string, sink func(Notification)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := c.Command(ctx, "-C", "attach-session", "-t", session, "-f", "no-output,ignore-size")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer func() { _ = stdin.Close() }()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("tmux control mode: %w", err)
	}
	Notifications(stdout, sink)
	err = cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("tmux control mode: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Notifications parses a control-mode stream from r, calling sink for each
// notification, until r ends. Exported so a recorded transcript can drive
// tests.
func Notifications(r io.Reader, sink func(Notification)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	inReply := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "%begin "):
			inReply = true
		case strings.HasPrefix(line, "%end "), strings.HasPrefix(line, "%error "):
			inReply = false
		case inReply, !strings.HasPrefix(line, "%"):
			// A command reply's body, or pane output we asked not to get.
		default:
			name, args, _ := strings.Cut(line, " ")
			sink(Notification{Name: name, Args: args})
		}
	}
}
