package cmux

import (
	"context"
	"strings"
	"testing"

	"github.com/sodre90/term-bridge/internal/testutil"
)

func TestRpcReturnsStdoutJSON(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, `#!/bin/sh
# Echo the method ($2) and params ($3) back so the test can assert them.
printf '{"method":"%s","params":%s}' "$2" "${3:-null}"
`)
	c := &Client{Bin: bin}
	out, err := c.Rpc(context.Background(), "mobile.workspace.list", map[string]any{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"method":"mobile.workspace.list"`) {
		t.Fatalf("method not forwarded: %s", out)
	}
	if !strings.Contains(string(out), `"x":1`) {
		t.Fatalf("params not forwarded: %s", out)
	}
}

func TestRpcNilParamsOmitsArg(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, `#!/bin/sh
# With no params arg, $3 is empty -> default to the literal NOARG.
printf '%s' "${3:-NOARG}"
`)
	c := &Client{Bin: bin}
	out, err := c.Rpc(context.Background(), "mobile.host.status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "NOARG" {
		t.Fatalf("expected no params arg, got %q", out)
	}
}

func TestRpcErrorIncludesStderr(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, `#!/bin/sh
echo "boom" >&2
exit 3
`)
	c := &Client{Bin: bin}
	_, err := c.Rpc(context.Background(), "x", nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want error containing stderr, got %v", err)
	}
}

func TestRpcCallsOnReachedOnSuccess(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, "#!/bin/sh\nprintf '{}'\n")
	var reached int
	c := &Client{Bin: bin, OnReached: func() { reached++ }}
	if _, err := c.Rpc(context.Background(), "mobile.host.status", nil); err != nil {
		t.Fatal(err)
	}
	if reached != 1 {
		t.Fatalf("OnReached called %d times, want 1", reached)
	}
}

func TestRpcDoesNotCallOnReachedOnError(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, "#!/bin/sh\nexit 1\n")
	var reached int
	c := &Client{Bin: bin, OnReached: func() { reached++ }}
	if _, err := c.Rpc(context.Background(), "x", nil); err == nil {
		t.Fatal("want error from a failing subprocess")
	}
	if reached != 0 {
		t.Fatalf("OnReached called %d times on a failed call, want 0", reached)
	}
}

func TestRpcNilOnReachedIsSafe(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, "#!/bin/sh\nprintf '{}'\n")
	c := &Client{Bin: bin}
	if _, err := c.Rpc(context.Background(), "mobile.host.status", nil); err != nil {
		t.Fatal(err)
	}
}

func TestEventsStreamsLines(t *testing.T) {
	bin := testutil.WriteFakeCmux(t, `#!/bin/sh
printf '{"seq":1}\n{"seq":2}\n'
`)
	c := &Client{Bin: bin}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, pipe, err := c.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	buf := make([]byte, 64)
	n, _ := pipe.Read(buf)
	if !strings.Contains(string(buf[:n]), `"seq":1`) {
		t.Fatalf("want first frame, got %q", buf[:n])
	}
	_ = cmd.Wait()
}
