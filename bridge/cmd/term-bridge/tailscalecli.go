package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os/exec"
	"time"
)

// tailscaleBin is the CLI binary invoked by tailscaleSelfStatus/tailscaleCert.
// Overridden in tests to point at a fake script.
var tailscaleBin = "tailscale"

// tailscaleSelf is the subset of `tailscale status --json`'s "Self" object
// this package needs.
type tailscaleSelf struct {
	DNSName      string
	TailscaleIPs []netip.Addr
}

// tailscaleSelfStatus shells out to `tailscale status --json` for this
// node's own status. Both serveDirect (bind address + cert domain) and
// pair-device --direct (pairing base URL) need this.
//
// This shells out to the tailscale CLI binary rather than using
// tailscale.com/client/tailscale's in-process LocalClient because on
// macOS's "macsys" (standalone, system-extension) install, LocalClient
// reads local-API credentials from a file under /Library/Tailscale that is
// readable only by root and the macOS `admin` group -- a group the account
// running term-bridge need not belong to. The `tailscale` CLI binary is
// recognized by the OS and fetches those credentials over XPC instead, so
// it works for any user able to run `tailscale status` at all.
func tailscaleSelfStatus(ctx context.Context) (*tailscaleSelf, error) {
	out, err := exec.CommandContext(ctx, tailscaleBin, "status", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w", stderrOrErr(err))
	}
	var parsed struct {
		Self struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("tailscale status: parse output: %w", err)
	}
	self := &tailscaleSelf{DNSName: parsed.Self.DNSName}
	for _, s := range parsed.Self.TailscaleIPs {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			continue
		}
		self.TailscaleIPs = append(self.TailscaleIPs, ip)
	}
	return self, nil
}

// tailscaleCert shells out to `tailscale cert` to (re)provision a real,
// publicly-trusted Let's Encrypt certificate for domain, writing it to
// certFile/keyFile. minValidity (if nonzero) asks the CLI to renew only if
// the current cert has less than that much validity left, making repeated
// calls a cheap no-op otherwise.
func tailscaleCert(ctx context.Context, domain, certFile, keyFile string, minValidity time.Duration) error {
	args := []string{"cert", "--cert-file", certFile, "--key-file", keyFile}
	if minValidity > 0 {
		args = append(args, "--min-validity", minValidity.String())
	}
	args = append(args, domain)
	out, err := exec.CommandContext(ctx, tailscaleBin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tailscale cert %s: %w: %s", domain, err, bytes.TrimSpace(out))
	}
	return nil
}

// stderrOrErr surfaces a command's captured stderr (from *exec.ExitError,
// which (Output) populates but the bare error doesn't print) when present,
// so failures show the CLI's actual message instead of just "exit status 1".
func stderrOrErr(err error) error {
	if ee, ok := err.(*exec.ExitError); ok && len(bytes.TrimSpace(ee.Stderr)) > 0 {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(ee.Stderr))
	}
	return err
}
