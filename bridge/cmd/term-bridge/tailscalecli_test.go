package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFakeTailscaleBin writes an executable shell script standing in for
// the `tailscale` CLI, points tailscaleBin at it for the duration of the
// test, and restores the original on cleanup.
func writeFakeTailscaleBin(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-tailscale")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := tailscaleBin
	tailscaleBin = path
	t.Cleanup(func() { tailscaleBin = orig })
}

func TestTailscaleSelfStatusParsesDNSNameAndIPs(t *testing.T) {
	writeFakeTailscaleBin(t, `
if [ "$1" = "status" ] && [ "$2" = "--json" ]; then
  cat <<'EOF'
{"Self":{"DNSName":"macbook.example.ts.net.","TailscaleIPs":["fd7a:115c:a1e0::1","100.64.1.2"]}}
EOF
  exit 0
fi
exit 1
`)
	st, err := tailscaleSelfStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.DNSName != "macbook.example.ts.net." {
		t.Fatalf("DNSName = %q", st.DNSName)
	}
	if len(st.TailscaleIPs) != 2 {
		t.Fatalf("want 2 parsed IPs, got %d: %v", len(st.TailscaleIPs), st.TailscaleIPs)
	}
	if st.TailscaleIPs[1].String() != "100.64.1.2" {
		t.Fatalf("TailscaleIPs[1] = %q, want 100.64.1.2", st.TailscaleIPs[1])
	}
}

func TestTailscaleSelfStatusSkipsUnparseableIPs(t *testing.T) {
	writeFakeTailscaleBin(t, `cat <<'EOF'
{"Self":{"DNSName":"x.ts.net.","TailscaleIPs":["not-an-ip","100.64.1.2"]}}
EOF
`)
	st, err := tailscaleSelfStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.TailscaleIPs) != 1 || st.TailscaleIPs[0].String() != "100.64.1.2" {
		t.Fatalf("TailscaleIPs = %v, want just [100.64.1.2]", st.TailscaleIPs)
	}
}

func TestTailscaleSelfStatusCommandFailureErrors(t *testing.T) {
	writeFakeTailscaleBin(t, `echo "permission denied reading local API" >&2
exit 1
`)
	_, err := tailscaleSelfStatus(context.Background())
	if err == nil {
		t.Fatal("want error when the tailscale binary exits nonzero")
	}
	if !strings.Contains(err.Error(), "permission denied reading local API") {
		t.Fatalf("error %q should surface the command's stderr", err)
	}
}

func TestTailscaleSelfStatusBinaryNotFoundErrors(t *testing.T) {
	orig := tailscaleBin
	tailscaleBin = filepath.Join(t.TempDir(), "no-such-binary")
	t.Cleanup(func() { tailscaleBin = orig })
	if _, err := tailscaleSelfStatus(context.Background()); err == nil {
		t.Fatal("want error when the tailscale binary doesn't exist")
	}
}

func TestTailscaleCertWritesFilesAndPassesArgs(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "c.pem")
	keyFile := filepath.Join(dir, "k.pem")
	argsFile := filepath.Join(dir, "args.txt")
	writeFakeTailscaleBin(t, fmt.Sprintf(`echo "$@" > %s
# emulate `+"`tailscale cert`"+` actually writing the requested files
for a in "$@"; do :; done
i=1
prev=""
for a in "$@"; do
  if [ "$prev" = "--cert-file" ]; then echo fake-cert > "$a"; fi
  if [ "$prev" = "--key-file" ]; then echo fake-key > "$a"; fi
  prev="$a"
done
`, argsFile))

	err := tailscaleCert(context.Background(), "macbook.example.ts.net", certFile, keyFile, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	gotArgs, readErr := os.ReadFile(argsFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	args := strings.TrimSpace(string(gotArgs))
	for _, want := range []string{"cert", "--cert-file", certFile, "--key-file", keyFile, "--min-validity", "720h0m0s", "macbook.example.ts.net"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args %q missing %q", args, want)
		}
	}
	if got, err := os.ReadFile(certFile); err != nil || strings.TrimSpace(string(got)) != "fake-cert" {
		t.Fatalf("cert file not written as expected: %q, err=%v", got, err)
	}
	if got, err := os.ReadFile(keyFile); err != nil || strings.TrimSpace(string(got)) != "fake-key" {
		t.Fatalf("key file not written as expected: %q, err=%v", got, err)
	}
}

func TestTailscaleCertOmitsMinValidityWhenZero(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	writeFakeTailscaleBin(t, fmt.Sprintf(`echo "$@" > %s`, argsFile))

	if err := tailscaleCert(context.Background(), "d.ts.net", filepath.Join(dir, "c"), filepath.Join(dir, "k"), 0); err != nil {
		t.Fatal(err)
	}
	gotArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(gotArgs), "--min-validity") {
		t.Fatalf("args %q should omit --min-validity when minValidity is 0", gotArgs)
	}
}

func TestTailscaleCertCommandFailureErrors(t *testing.T) {
	writeFakeTailscaleBin(t, `echo "acme: rate limited" >&2
exit 1
`)
	err := tailscaleCert(context.Background(), "d.ts.net", "/tmp/c", "/tmp/k", 0)
	if err == nil {
		t.Fatal("want error when tailscale cert exits nonzero")
	}
	if !strings.Contains(err.Error(), "acme: rate limited") {
		t.Fatalf("error %q should surface the command's output", err)
	}
}
