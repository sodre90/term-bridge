package cmux

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sodre90/term-bridge/internal/testutil"
)

const fakeSocketPassword = "test-password"

// shortSocketDir returns a fresh temp dir short enough to hold a Unix socket
// path under sockaddr_un's ~104-byte sun_path limit -- t.TempDir() embeds the
// (often long) test name and reliably blows past that on macOS.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cmuxsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// startFakeCmuxSocket runs a minimal stand-in for cmux's control socket:
// requires "auth <password>\n" first, then answers each JSON request line
// with respond's result. Points CMUX_SOCKET_PATH/CMUX_SOCKET_PASSWORD at it
// for the duration of the test.
func startFakeCmuxSocket(t *testing.T, password string, respond func(method string, params json.RawMessage) (result json.RawMessage, errCode, errMsg string)) {
	t.Helper()
	path := filepath.Join(shortSocketDir(t), "cmux.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				authLine, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimSpace(authLine) != "auth "+password {
					conn.Write([]byte("ERROR: Invalid password\n"))
					return
				}
				conn.Write([]byte("OK: Authenticated\n"))
				for {
					reqLine, err := r.ReadString('\n')
					if err != nil {
						return
					}
					var req struct {
						ID     string          `json:"id"`
						Method string          `json:"method"`
						Params json.RawMessage `json:"params"`
					}
					if err := json.Unmarshal([]byte(reqLine), &req); err != nil {
						return
					}
					result, errCode, errMsg := respond(req.Method, req.Params)
					var resp map[string]any
					if errCode != "" {
						resp = map[string]any{"id": req.ID, "ok": false, "error": map[string]string{"code": errCode, "message": errMsg}}
					} else {
						resp = map[string]any{"id": req.ID, "ok": true, "result": result}
					}
					b, _ := json.Marshal(resp)
					conn.Write(append(b, '\n'))
				}
			}()
		}
	}()
	t.Setenv("CMUX_SOCKET_PATH", path)
	t.Setenv("CMUX_SOCKET_PASSWORD", password)
}

func TestFastPathUsedWhenSocketAvailable(t *testing.T) {
	startFakeCmuxSocket(t, fakeSocketPassword, func(method string, params json.RawMessage) (json.RawMessage, string, string) {
		return json.RawMessage(`{"via":"socket","method":"` + method + `"}`), "", ""
	})
	bin := testutil.WriteFakeCmux(t, `#!/bin/sh
echo "subprocess should not have been used" >&2
exit 1
`)
	c := &Client{Bin: bin, FastPath: true}
	out, err := c.Rpc(context.Background(), "mobile.workspace.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"via":"socket"`) {
		t.Fatalf("want response via socket fast path, got %s", out)
	}
}

func TestFastPathFallsBackWhenSocketUnreachable(t *testing.T) {
	t.Setenv("CMUX_SOCKET_PATH", filepath.Join(t.TempDir(), "does-not-exist.sock"))
	bin := testutil.WriteFakeCmux(t, `#!/bin/sh
printf '{"via":"subprocess"}'
`)
	c := &Client{Bin: bin, FastPath: true}
	out, err := c.Rpc(context.Background(), "mobile.workspace.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"via":"subprocess"`) {
		t.Fatalf("want fallback to subprocess, got %s", out)
	}
}

func TestFastPathFallsBackWhenAuthRejected(t *testing.T) {
	startFakeCmuxSocket(t, fakeSocketPassword, func(method string, params json.RawMessage) (json.RawMessage, string, string) {
		return json.RawMessage(`{}`), "", ""
	})
	// Override the password the client will actually send so the fake server
	// rejects it -- exercises the auth-failure branch of connectLocked.
	t.Setenv("CMUX_SOCKET_PASSWORD", "wrong-password")
	bin := testutil.WriteFakeCmux(t, `#!/bin/sh
printf '{"via":"subprocess"}'
`)
	c := &Client{Bin: bin, FastPath: true}
	out, err := c.Rpc(context.Background(), "mobile.workspace.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"via":"subprocess"`) {
		t.Fatalf("want fallback to subprocess, got %s", out)
	}
}

// TestFastPathCommittedFailureDoesNotFallBack is the safety-critical case:
// once a request has actually been written to cmux's socket, a subsequent
// failure (here, the fake server closes the connection without responding at
// all) must be reported as-is, never retried via the subprocess CLI --
// retrying an ambiguous send through a second transport risks
// double-executing non-idempotent input (e.g. typed keystrokes).
func TestFastPathCommittedFailureDoesNotFallBack(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "cmux.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		r := bufio.NewReader(conn)
		authLine, _ := r.ReadString('\n')
		if strings.TrimSpace(authLine) != "auth "+fakeSocketPassword {
			conn.Close()
			return
		}
		conn.Write([]byte("OK: Authenticated\n"))
		// Read (and discard) the RPC request, proving it was sent, then hang
		// up without a response -- simulates cmux crashing mid-call.
		r.ReadString('\n')
		conn.Close()
	}()
	t.Setenv("CMUX_SOCKET_PATH", path)
	t.Setenv("CMUX_SOCKET_PASSWORD", fakeSocketPassword)

	bin := testutil.WriteFakeCmux(t, `#!/bin/sh
printf '{"via":"subprocess"}'
`)
	c := &Client{Bin: bin, FastPath: true}
	_, err = c.Rpc(context.Background(), "mobile.terminal.input", map[string]any{"text": "rm -rf /"})
	if err == nil {
		t.Fatal("want an error (the connection was dropped mid-call), got nil")
	}
	if strings.Contains(err.Error(), "subprocess") {
		t.Fatalf("must not have fallen back to the subprocess CLI after a committed send, got %v", err)
	}
}

func TestFastPathReconnectsAfterConnectionDrop(t *testing.T) {
	var calls atomic.Int32
	startFakeCmuxSocket(t, fakeSocketPassword, func(method string, params json.RawMessage) (json.RawMessage, string, string) {
		calls.Add(1)
		return json.RawMessage(`{"via":"socket"}`), "", ""
	})
	bin := testutil.WriteFakeCmux(t, `#!/bin/sh
echo "subprocess should not have been used" >&2
exit 1
`)
	c := &Client{Bin: bin, FastPath: true}

	if _, err := c.Rpc(context.Background(), "mobile.workspace.list", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Forcibly break every pooled connection (simulates cmux restarting,
	// which drops every open connection at once, not just the one this
	// particular call happened to use) and confirm the very next call
	// transparently reconnects rather than staying stuck on a broken one.
	c.fastPathPool().closeAll()
	time.Sleep(20 * time.Millisecond)

	out, err := c.Rpc(context.Background(), "mobile.workspace.list", nil)
	if err != nil {
		t.Fatalf("second call after drop: %v", err)
	}
	if !strings.Contains(string(out), `"via":"socket"`) {
		t.Fatalf("want reconnected socket response, got %s", out)
	}
	if calls.Load() != 2 {
		t.Fatalf("want 2 socket-side calls, got %d", calls.Load())
	}
}

// TestFastPathRetriesOnceWhenReusedConnectionWentStale reproduces the bug
// behind the app's intermittent "bridge HTTP 502: cmux unavailable" that
// cleared itself on manual refresh: cmux restarts (or the Mac sleeps/wakes)
// between two calls that land on the same pooled socketConn. The client's
// cached sc.conn is still non-nil going into the later call -- unlike
// TestFastPathReconnectsAfterConnectionDrop, nothing ever called
// pool.closeAll() -- so the bug was in treating a write failure on that
// stale-but-still-cached connection as "committed," which skipped any retry
// and surfaced the error straight to the caller.
//
// The pool round-robins fastPathPoolSize connections FIFO, so issuing exactly
// fastPathPoolSize calls first (each landing on a distinct, freshly-dialed
// connection that the fake server closes right after answering) guarantees
// the next call cycles back to the very first connection -- reused from the
// client's point of view, but already dead on the server side.
func TestFastPathRetriesOnceWhenReusedConnectionWentStale(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "cmux.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var calls atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				authLine, err := r.ReadString('\n')
				if err != nil || strings.TrimSpace(authLine) != "auth "+fakeSocketPassword {
					return
				}
				conn.Write([]byte("OK: Authenticated\n"))
				reqLine, err := r.ReadString('\n')
				if err != nil {
					return
				}
				calls.Add(1)
				var req struct {
					ID string `json:"id"`
				}
				json.Unmarshal([]byte(reqLine), &req)
				resp, _ := json.Marshal(map[string]any{"id": req.ID, "ok": true, "result": json.RawMessage(`{"via":"socket"}`)})
				conn.Write(append(resp, '\n'))
				// Answer exactly one request, then hang up from the SERVER
				// side -- simulates cmux restarting while the bridge's
				// client-side pool still holds this connection open (its
				// sc.conn stays non-nil; nothing here touches the client).
			}()
		}
	}()
	t.Setenv("CMUX_SOCKET_PATH", path)
	t.Setenv("CMUX_SOCKET_PASSWORD", fakeSocketPassword)

	bin := testutil.WriteFakeCmux(t, `#!/bin/sh
echo "subprocess should not have been used" >&2
exit 1
`)
	c := &Client{Bin: bin, FastPath: true}

	for i := 0; i < fastPathPoolSize; i++ {
		if _, err := c.Rpc(context.Background(), "mobile.workspace.list", nil); err != nil {
			t.Fatalf("seed call %d: %v", i, err)
		}
	}
	time.Sleep(20 * time.Millisecond)

	out, err := c.Rpc(context.Background(), "mobile.workspace.list", nil)
	if err != nil {
		t.Fatalf("call on a since-gone-stale cached connection should transparently reconnect and retry, got: %v", err)
	}
	if !strings.Contains(string(out), `"via":"socket"`) {
		t.Fatalf("want reconnected socket response, got %s", out)
	}
	if calls.Load() != fastPathPoolSize+1 {
		t.Fatalf("want %d socket-side calls (one per seed connection, plus the retried one), got %d", fastPathPoolSize+1, calls.Load())
	}
}

// TestFastPathPoolSlowCallDoesNotBlockConcurrentFastCall is the accept
// criterion for the pool: before this pool existed, every fast-path call
// funneled through one shared socketConn whose mutex was held for the whole
// round trip, so a slow call (e.g. a big replay poll) head-of-line-blocked
// any unrelated fast call (e.g. an input ack) issued concurrently. Confirmed
// against the pre-pool code: a fast call issued 20ms after a 200ms-sleeping
// slow call still took ~180ms to complete. With a multi-connection pool the
// fast call should land on a different, idle connection and complete close
// to its own server-side latency instead of waiting out the slow one.
func TestFastPathPoolSlowCallDoesNotBlockConcurrentFastCall(t *testing.T) {
	release := make(chan struct{})
	startFakeCmuxSocket(t, fakeSocketPassword, func(method string, params json.RawMessage) (json.RawMessage, string, string) {
		if method == "slow" {
			<-release
		}
		return json.RawMessage(`{}`), "", ""
	})
	c := &Client{FastPath: true}

	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		if _, err := c.Rpc(context.Background(), "slow", nil); err != nil {
			t.Error(err)
		}
	}()
	// Give the slow call time to be checked out and blocked server-side
	// before starting the fast one, so this deterministically exercises "an
	// unrelated call started while a slow one is in flight," not a race.
	time.Sleep(20 * time.Millisecond)

	fastStart := time.Now()
	if _, err := c.Rpc(context.Background(), "fast", nil); err != nil {
		t.Fatal(err)
	}
	fastElapsed := time.Since(fastStart)
	close(release)
	<-slowDone

	if fastElapsed > 50*time.Millisecond {
		t.Fatalf("fast call took %v while a slow call was in flight on another pooled connection; want it to complete promptly, not queue behind the slow one", fastElapsed)
	}
}

// withSocketIOTimeout shortens the default budget for one test, so a test
// about deadlines doesn't have to sleep out a real five seconds.
func withSocketIOTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := socketIOTimeout
	socketIOTimeout = d
	t.Cleanup(func() { socketIOTimeout = prev })
}

// cmux-app-69y. The default budget is sized for calls like
// mobile.workspace.list; mobile.terminal.replay is an order of magnitude
// heavier and has to be able to ask for more. The old clamp let a caller's
// deadline shorten the default only, so this call died at the default no
// matter what deadline it was given.
func TestCallerDeadlineCanExceedTheDefaultBudget(t *testing.T) {
	withSocketIOTimeout(t, 50*time.Millisecond)
	startFakeCmuxSocket(t, fakeSocketPassword, func(method string, params json.RawMessage) (json.RawMessage, string, string) {
		time.Sleep(300 * time.Millisecond)
		return json.RawMessage(`{"via":"socket"}`), "", ""
	})
	bin := testutil.WriteFakeCmux(t, `#!/bin/sh
echo "subprocess should not have been used" >&2
exit 1
`)
	c := &Client{Bin: bin, FastPath: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := c.Rpc(ctx, "mobile.terminal.replay", nil)
	if err != nil {
		t.Fatalf("a call slower than the default budget but inside its own deadline must succeed: %v", err)
	}
	if !strings.Contains(string(out), `"via":"socket"`) {
		t.Fatalf("want response via socket fast path, got %s", out)
	}
}

// The deadline is authoritative in both directions -- a caller that wants
// less than the default still gets less.
func TestCallerDeadlineShorterThanTheDefaultStillWins(t *testing.T) {
	withSocketIOTimeout(t, 10*time.Second)
	startFakeCmuxSocket(t, fakeSocketPassword, func(method string, params json.RawMessage) (json.RawMessage, string, string) {
		time.Sleep(300 * time.Millisecond)
		return json.RawMessage(`{}`), "", ""
	})
	bin := testutil.WriteFakeCmux(t, `#!/bin/sh
echo "subprocess should not have been used" >&2
exit 1
`)
	c := &Client{Bin: bin, FastPath: true}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.Rpc(ctx, "mobile.terminal.replay", nil); err == nil {
		t.Fatal("want the caller's shorter deadline to cut the call off")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waited %s, so the short deadline was ignored", elapsed)
	}
}

// A cmux that accepted the request and went quiet is a different problem
// from a cmux that isn't there, and the two want opposite responses. They
// used to be one undifferentiated "i/o timeout" inside a read error.
func TestSlowCmuxIsReportedDistinctlyFromUnreachableCmux(t *testing.T) {
	withSocketIOTimeout(t, 50*time.Millisecond)
	startFakeCmuxSocket(t, fakeSocketPassword, func(method string, params json.RawMessage) (json.RawMessage, string, string) {
		time.Sleep(2 * time.Second)
		return json.RawMessage(`{}`), "", ""
	})
	bin := testutil.WriteFakeCmux(t, `#!/bin/sh
echo "subprocess should not have been used" >&2
exit 1
`)
	c := &Client{Bin: bin, FastPath: true}

	_, err := c.Rpc(context.Background(), "mobile.terminal.replay", nil)
	if !errors.Is(err, ErrCmuxTooSlow) {
		t.Fatalf("a request cmux never answered must report ErrCmuxTooSlow, got %v", err)
	}

	// The other failure: nothing listening at all. Must NOT look like slowness.
	t.Setenv("CMUX_SOCKET_PATH", filepath.Join(t.TempDir(), "does-not-exist.sock"))
	unreachable := &Client{Bin: testutil.WriteFakeCmux(t, `#!/bin/sh
echo "cmux not running" >&2
exit 1
`), FastPath: true}
	if _, err := unreachable.Rpc(context.Background(), "mobile.terminal.replay", nil); errors.Is(err, ErrCmuxTooSlow) {
		t.Fatalf("an unreachable cmux must not be reported as a slow one: %v", err)
	}
}

// cmux-app-34c: the phone reconnected to a surface cmux no longer had, every
// 5s, because "the object is gone" was indistinguishable from "the call
// failed". The code was on the response all along and fmt.Errorf flattened it.
func TestANotFoundResponseIsTypedAsSuch(t *testing.T) {
	startFakeCmuxSocket(t, fakeSocketPassword, func(string, json.RawMessage) (json.RawMessage, string, string) {
		return nil, "not_found", "Terminal surface not found"
	})
	c := &Client{Bin: testutil.WriteFakeCmux(t, "#!/bin/sh\nexit 1\n"), FastPath: true}

	_, err := c.Rpc(context.Background(), "mobile.terminal.replay", nil)

	if !IsNotFound(err) {
		t.Fatalf("IsNotFound(%v) = false, want true", err)
	}
}

// Every other refusal must stay retryable: treating one as terminal would
// strand a live pane on an error screen.
func TestOtherRefusalsAreNotNotFound(t *testing.T) {
	for _, code := range []string{"internal", "invalid_params", "unauthorized", "unknown"} {
		t.Run(code, func(t *testing.T) {
			startFakeCmuxSocket(t, fakeSocketPassword, func(string, json.RawMessage) (json.RawMessage, string, string) {
				return nil, code, "nope"
			})
			c := &Client{Bin: testutil.WriteFakeCmux(t, "#!/bin/sh\nexit 1\n"), FastPath: true}

			_, err := c.Rpc(context.Background(), "mobile.terminal.replay", nil)

			if err == nil {
				t.Fatal("want an error")
			}
			if IsNotFound(err) {
				t.Fatalf("IsNotFound(%v) = true, want false", err)
			}
		})
	}
}

// The wording is what appears in the agent log and in existing assertions;
// typing the error must not change a byte of it.
func TestTypingTheErrorDidNotChangeItsText(t *testing.T) {
	err := &RPCError{Method: "mobile.terminal.replay", Code: "not_found", Message: "Terminal surface not found"}

	const want = "cmux rpc mobile.terminal.replay: not_found: Terminal surface not found"
	if err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
}

// A known gap, asserted so it is a decision and not a surprise: the `cmux rpc`
// subprocess fallback has only the CLI's stderr, with no code in it. Guessing
// at that text would be exactly the brittle matching the typed error exists to
// avoid, so a surface that goes missing while the fast path is unavailable
// keeps today's retry behaviour.
func TestTheSubprocessFallbackCannotTypeItsErrors(t *testing.T) {
	t.Setenv("CMUX_SOCKET_PATH", filepath.Join(t.TempDir(), "does-not-exist.sock"))
	c := &Client{Bin: testutil.WriteFakeCmux(t, `#!/bin/sh
echo "not_found: Terminal surface not found" >&2
exit 1
`), FastPath: true}

	_, err := c.Rpc(context.Background(), "mobile.terminal.replay", nil)

	if err == nil {
		t.Fatal("want an error")
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		t.Fatal("the subprocess path produced a typed error; if that is now possible, teach IsNotFound about it")
	}
}
