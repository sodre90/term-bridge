package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sodre90/term-bridge/internal/testutil"
)

func TestRunPassesSocketAndArgsThrough(t *testing.T) {
	c := &Client{Bin: testutil.WriteFakeTmux(t, "#!/bin/sh\nprintf '%s|' \"$@\"\n"), Socket: "/tmp/sock"}
	reached := 0
	c.OnReached = func() { reached++ }
	out, err := c.Run(context.Background(), "list-panes", "-a", "-F", "#{pane_id}", ";", "display", "-p", "x")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "-S|/tmp/sock|list-panes|-a|-F|#{pane_id}|;|display|-p|x|" {
		t.Fatalf("args = %q", got)
	}
	if reached != 1 {
		t.Fatalf("OnReached called %d times", reached)
	}
}

func TestRunClassifiesStderr(t *testing.T) {
	cases := []struct {
		stderr string
		check  func(error) bool
	}{
		{"can't find pane: %9\n", func(err error) bool {
			var nf *NotFoundError
			return errors.As(err, &nf) && nf.NotFound() && nf.Target == "%9"
		}},
		{"can't find window: @3\n", func(err error) bool {
			var nf *NotFoundError
			return errors.As(err, &nf)
		}},
		{"no server running on /tmp/tmux-1000/default\n", func(err error) bool { return errors.Is(err, ErrNoServer) }},
		{"error connecting to /tmp/tmux-1000/default (No such file or directory)\n", func(err error) bool { return errors.Is(err, ErrNoServer) }},
		{"usage: kill-window [-a] [-t target-window]\n", func(err error) bool {
			return err != nil && strings.Contains(err.Error(), "kill-window") && !errors.Is(err, ErrNoServer)
		}},
	}
	for _, tc := range cases {
		c := &Client{Bin: testutil.WriteFakeTmux(t, "#!/bin/sh\nprintf '%s' \""+tc.stderr+"\" >&2\nexit 1\n")}
		reached := false
		c.OnReached = func() { reached = true }
		_, err := c.Run(context.Background(), "kill-window", "-t", "%9")
		if !tc.check(err) {
			t.Errorf("stderr %q classified as %v", tc.stderr, err)
		}
		if reached {
			t.Errorf("stderr %q: a failure must not count as reaching tmux", tc.stderr)
		}
	}
}

func TestRunWithStdinFeedsTheCommand(t *testing.T) {
	c := &Client{Bin: testutil.WriteFakeTmux(t, "#!/bin/sh\ncat\n")}
	out, err := c.RunWithStdin(context.Background(), []byte("pasted\n"), "load-buffer", "-")
	if err != nil || string(out) != "pasted\n" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}

const recordedControlStream = `%begin 1789367846 343 0
%end 1789367846 343 0
%session-changed $0 cmux-app-scratch
%unlinked-window-add @4
%sessions-changed
%unlinked-window-renamed @4 tmux
%window-add @6
%begin 1789367850 344 0
some reply body
%output %1 should-not-be-a-notification
%end 1789367850 344 0
%window-renamed @6 renamed
%layout-change @6 ebaa,100x30,0,0{50x30,0,0,7,49x30,51,0,8} ebaa,100x30,0,0{50x30,0,0,7,49x30,51,0,8} 
%unlinked-window-close @6
%exit
`

func TestNotificationsSkipsReplyFramingAndBodies(t *testing.T) {
	var got []string
	Notifications(strings.NewReader(recordedControlStream), func(n Notification) {
		got = append(got, n.Name+"|"+n.Args)
	})
	want := []string{
		"%session-changed|$0 cmux-app-scratch",
		"%unlinked-window-add|@4",
		"%sessions-changed|",
		"%unlinked-window-renamed|@4 tmux",
		"%window-add|@6",
		"%window-renamed|@6 renamed",
		"%layout-change|@6 ebaa,100x30,0,0{50x30,0,0,7,49x30,51,0,8} ebaa,100x30,0,0{50x30,0,0,7,49x30,51,0,8} ",
		"%unlinked-window-close|@6",
		"%exit|",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestWatchHoldsStdinOpenAndEndsWithTheProcess(t *testing.T) {
	// The fake exits only when its stdin closes; Watch must therefore return
	// through ctx cancellation, never because it let stdin go early.
	script := "#!/bin/sh\necho '%session-changed $0 s'\ncat >/dev/null\n"
	c := &Client{Bin: testutil.WriteFakeTmux(t, script)}
	ctx, cancel := context.WithCancel(context.Background())
	seen := make(chan Notification, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.Watch(ctx, "s", func(n Notification) {
			select {
			case seen <- n:
			default:
			}
		})
	}()
	if n := <-seen; n.Name != "%session-changed" {
		t.Fatalf("first notification = %+v", n)
	}
	select {
	case err := <-done:
		t.Fatalf("Watch returned early: %v", err)
	default:
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Watch after cancel = %v", err)
	}
}

func TestWatchReturnsOnCancelEvenWhenAChildStillHoldsThePipes(t *testing.T) {
	// Cancelling kills only the fake; a child it forked keeps the stderr pipe
	// open. Wait used to block on that pipe forever, which is what hung CI's
	// go test for 10 minutes once the race in the fake above landed that way.
	script := "#!/bin/sh\nsleep 30 &\necho '%session-changed $0 s'\ncat >/dev/null\n"
	c := &Client{Bin: testutil.WriteFakeTmux(t, script)}
	ctx, cancel := context.WithCancel(context.Background())
	seen := make(chan Notification, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.Watch(ctx, "s", func(n Notification) {
			select {
			case seen <- n:
			default:
			}
		})
	}()
	<-seen
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Watch after cancel = %v", err)
	}
}
