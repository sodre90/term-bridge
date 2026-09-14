package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/sodre90/term-bridge/internal/auth"
	"github.com/sodre90/term-bridge/internal/cli"
	"github.com/sodre90/term-bridge/internal/config"
)

// announceStore names the database every admin answer below came from, on
// stderr so stdout stays parseable. "no paired devices" and "no paired
// devices in the store you meant" look identical otherwise (cmux-app-xdc).
func announceStore(cfg config.Config) {
	fmt.Fprintln(os.Stderr, "store:", cfg.TokenStore)
}

// parseAdminArgs parses the --config flag the admin commands share and
// returns the positional arguments after it.
//
// Go's flag package stops at the first positional, so `devices list --config
// <path>` parses no flags at all and falls back to the DEFAULT config --
// silently reading a different database than the one the operator named.
// That is the same wrong-store trap as cmux-app-xdc, one layer up, so a flag
// stranded behind a subcommand is refused rather than ignored.
func parseAdminArgs(name string, args []string) (cfgPath string, rest []string, ok bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	path := fs.String("config", defaultConfigPath(), "path to config.toml")
	if err := fs.Parse(args); err != nil {
		return "", nil, false
	}
	for _, arg := range fs.Args() {
		if strings.HasPrefix(arg, "-") {
			fmt.Fprintf(os.Stderr, "flags must come before the subcommand: term-bridge-relay %s %s ...\n", name, arg)
			return "", nil, false
		}
	}
	return *path, fs.Args(), true
}

// displayedHashLen is how much of a token hash `devices list` prints. It is
// the operator's only source for the prefix `revoke` takes, so it has to be
// wide enough that what they can see is never ambiguous.
const displayedHashLen = 12

func shortHash(tokenHash string) string {
	if len(tokenHash) <= displayedHashLen {
		return tokenHash
	}
	return tokenHash[:displayedHashLen]
}

// revokeDevice deletes the device arg names. arg is a prefix of the hash
// `devices list` prints, because the raw token this used to require is a
// value only the phone ever holds -- which left the relay operator unable to
// revoke anything they could see (cmux-app-nvt). A raw token still works as
// a fallback for the one case where somebody does have one: a leak.
//
// Resolution refuses rather than guesses, and revocation is scoped to the
// resolved device's own tenant, so a mistyped prefix can only ever fail.
func revokeDevice(store *auth.Store, arg string) int {
	matches := matchingDevices(store.List(), arg)
	switch len(matches) {
	case 1:
		dev := matches[0]
		if err := store.RevokeByHash(dev.TenantID, dev.TokenHash); err != nil {
			fmt.Fprintf(os.Stderr, "revoke: %v\n", err)
			return 1
		}
		fmt.Printf("revoked %s (%s, tenant %s)\n", shortHash(dev.TokenHash), dev.Name, dev.TenantID)
		return 0
	case 0:
		if err := store.Revoke(arg); err != nil {
			if errors.Is(err, auth.ErrNotFound) {
				fmt.Fprintf(os.Stderr, "no device matches %q -- run `devices list` for the prefixes\n", arg)
			} else {
				fmt.Fprintf(os.Stderr, "revoke: %v\n", err)
			}
			return 1
		}
		fmt.Println("revoked by raw token")
		return 0
	default:
		fmt.Fprintf(os.Stderr, "%q matches %d devices, be more specific:\n", arg, len(matches))
		for _, dev := range matches {
			fmt.Fprintf(os.Stderr, "  %s  %s  tenant=%s\n", shortHash(dev.TokenHash), dev.Name, dev.TenantID)
		}
		return 1
	}
}

// matchingDevices returns the devices arg selects: an exact hash on its own,
// otherwise every device the prefix covers.
func matchingDevices(devs []auth.Device, arg string) []auth.Device {
	if arg == "" {
		return nil
	}
	var matches []auth.Device
	for _, dev := range devs {
		if dev.TokenHash == arg {
			return []auth.Device{dev}
		}
		if strings.HasPrefix(dev.TokenHash, arg) {
			matches = append(matches, dev)
		}
	}
	return matches
}

func runDevices(args []string) int {
	cfgPath, rest, ok := parseAdminArgs("devices", args)
	if !ok {
		return 2
	}
	cfg, store, err := cli.OpenExistingStore(cfgPath)
	if err != nil {
		slog.Error("devices: load store", "err", err)
		return 1
	}
	announceStore(cfg)
	switch {
	case len(rest) == 0 || rest[0] == "list":
		devs := store.List()
		if len(devs) == 0 {
			fmt.Println("no paired devices")
			return 0
		}
		for _, d := range devs {
			fmt.Printf("%-16s  device=%s  tenant=%s  fcm=%v  created=%s\n",
				d.Name, shortHash(d.TokenHash), d.TenantID, d.FCM != "", d.Created.Format(time.RFC3339))
		}
		return 0
	case rest[0] == "revoke":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: term-bridge-relay devices revoke <device-prefix>")
			return 2
		}
		return revokeDevice(store, rest[1])
	default:
		fmt.Fprintln(os.Stderr, "usage: term-bridge-relay devices [list|revoke <device-prefix>]")
		return 2
	}
}

func runTenants(args []string) int {
	cfgPath, rest, ok := parseAdminArgs("tenants", args)
	if !ok {
		return 2
	}
	cfg, store, err := cli.OpenExistingStore(cfgPath)
	if err != nil {
		slog.Error("tenants: load store", "err", err)
		return 1
	}
	announceStore(cfg)
	switch {
	case len(rest) == 0 || rest[0] == "list":
		tenants, err := store.ListTenants()
		if err != nil {
			slog.Error("tenants: list", "err", err)
			return 1
		}
		if len(tenants) == 0 {
			fmt.Println("no tenants")
			return 0
		}
		for _, t := range tenants {
			fmt.Printf("%s  created=%s  revoked=%v\n", t.ID, t.CreatedAt.Format(time.RFC3339), t.Revoked)
		}
		return 0
	case rest[0] == "revoke":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: term-bridge-relay tenants revoke <id>")
			return 2
		}
		if store.RevokeTenant(rest[1]) {
			fmt.Println("revoked (this also stops all of that tenant's devices from verifying)")
			return 0
		}
		fmt.Fprintln(os.Stderr, "no such tenant, or already revoked")
		return 1
	default:
		fmt.Fprintln(os.Stderr, "usage: term-bridge-relay tenants [list|revoke <id>]")
		return 2
	}
}
