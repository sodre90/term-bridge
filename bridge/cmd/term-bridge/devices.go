package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sodre90/term-bridge/internal/config"
	"github.com/sodre90/term-bridge/internal/e2e"
	"github.com/sodre90/term-bridge/internal/wire"
)

// deviceAdminTimeout bounds a single device-admin HTTP call. These are small
// SQLite reads and one delete; anything slower than this is a dead server,
// not a busy one.
const deviceAdminTimeout = 10 * time.Second

// displayedHashLen is how much of a token hash the listing prints. It is the
// operator's only source for the prefix `revoke` takes, so it has to be wide
// enough that what they can see is never ambiguous.
const displayedHashLen = 12

// reaperFirstRound delays the first reaper round past agent startup, when
// the relay tunnel is usually still coming up; reaperPeriod paces the rest.
// Nothing here is urgent -- a stranded secret grants no access on its own --
// so this is deliberately slow enough to be invisible.
const (
	reaperFirstRound = 2 * time.Minute
	reaperPeriod     = time.Hour
)

// localSource marks a row that exists only in this Mac's e2e store: a shared
// secret whose server-side token is already gone. Those are unusable and
// invisible from the server side, so the listing has to carry them or
// nothing can ever reap them.
const localSource = "local"

// deviceRow is one paired device as the operator sees it: a server's view of
// it joined to whether this Mac still holds the e2e secret that makes it
// usable. The two stores drift apart with nothing reconciling them
// (cmux-app-vkq), and this join is the report of that drift.
type deviceRow struct {
	server    agentServer
	device    wire.AgentDevice
	hasSecret bool
}

func fetchDevices(srv agentServer) ([]wire.AgentDevice, error) {
	resp, err := srv.client.Get(srv.baseURL + "/agent/devices")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var body wire.AgentDeviceListResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Devices, nil
}

// revokeOnServer deletes the device's bearer token. A device the server has
// never heard of is reported as existed=false rather than as an error: that
// is the orphan case the caller is here to clean up, not a failure.
func revokeOnServer(srv agentServer, tokenHash string) (existed bool, err error) {
	resp, err := srv.client.Post(srv.baseURL+"/agent/devices/"+tokenHash+"/revoke", "application/json", nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
}

// collectDevices joins every configured server's listing to this Mac's e2e
// store. A server that cannot be reached yields a problem string rather than
// aborting the whole listing -- the rows that did come back are still worth
// showing -- so callers that must not act on a partial view check problems
// before doing anything.
// reached maps each server's kind to whether it answered this listing. It is
// the only end-to-end reachability probe the agent runs against its own
// transports, and throwing it away is how the direct standby stayed
// unreachable for fourteen days with nothing but an hourly WARN to show for it
// (cmux-app-t5x).
func collectDevices(servers []agentServer, sessions *e2e.Store) (rows []deviceRow, problems []string, reached map[string]bool) {
	unclaimedSecrets := make(map[string]bool)
	for _, id := range sessions.DeviceIDs() {
		unclaimedSecrets[id] = true
	}
	reached = make(map[string]bool, len(servers))
	for _, srv := range servers {
		devices, err := fetchDevices(srv)
		reached[srv.kind] = err == nil
		if err != nil {
			problems = append(problems, srv.kind+": "+err.Error())
			continue
		}
		for _, dev := range devices {
			// device_id in the e2e store IS the auth store's token hash --
			// AddDevice is keyed by exactly what the server returns here.
			hasSecret := unclaimedSecrets[dev.TokenHash]
			delete(unclaimedSecrets, dev.TokenHash)
			rows = append(rows, deviceRow{server: srv, device: dev, hasSecret: hasSecret})
		}
	}
	// Only once every server has answered is a leftover secret really an
	// orphan; against a partial listing it may just belong to the server that
	// failed, and calling it local would invite reaping a live device.
	if len(problems) == 0 {
		for _, id := range sessions.DeviceIDs() {
			if unclaimedSecrets[id] {
				rows = append(rows, deviceRow{
					server:    agentServer{kind: localSource},
					device:    wire.AgentDevice{TokenHash: id},
					hasSecret: true,
				})
			}
		}
	}
	return rows, problems, reached
}

func printDeviceRows(w io.Writer, rows []deviceRow) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SOURCE\tDEVICE\tNAME\tCREATED\tSECRET")
	for _, row := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			row.server.kind, shortHash(row.device.TokenHash),
			orDash(row.device.Name), orDash(row.device.CreatedAt), yesNo(row.hasSecret))
	}
	_ = tw.Flush()
}

func shortHash(tokenHash string) string {
	if len(tokenHash) <= displayedHashLen {
		return tokenHash
	}
	return tokenHash[:displayedHashLen]
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// resolveDevicePrefix picks the one device prefix names, and refuses rather
// than guesses: revocation is destructive and the operator types these by
// hand off the listing.
func resolveDevicePrefix(rows []deviceRow, prefix string) (deviceRow, error) {
	if prefix == "" {
		return deviceRow{}, fmt.Errorf("no device given")
	}
	var matches []deviceRow
	for _, row := range rows {
		if row.device.TokenHash == prefix {
			return row, nil
		}
		if strings.HasPrefix(row.device.TokenHash, prefix) {
			matches = append(matches, row)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return deviceRow{}, fmt.Errorf("no device matches %q", prefix)
	default:
		var candidates []string
		for _, row := range matches {
			candidates = append(candidates, shortHash(row.device.TokenHash)+" ("+row.server.kind+")")
		}
		return deviceRow{}, fmt.Errorf("%q matches %d devices, be more specific: %s",
			prefix, len(matches), strings.Join(candidates, ", "))
	}
}

// revokeByPrefix takes both halves of a paired device away: the bearer token
// on the server that issued it, then the shared secret this Mac holds for
// it. That order is load-bearing -- dropping the secret first would leave a
// token that authenticates into an agent which cannot decrypt for it, which
// is the exact drift state this command exists to clean up.
func revokeByPrefix(out io.Writer, servers []agentServer, sessions *e2e.Store, prefix string) error {
	rows, problems, _ := collectDevices(servers, sessions)
	if len(problems) > 0 {
		return fmt.Errorf("refusing to revoke from a partial device listing (%s) -- a prefix could resolve to the wrong device",
			strings.Join(problems, "; "))
	}
	row, err := resolveDevicePrefix(rows, prefix)
	if err != nil {
		return err
	}

	existed := false
	if row.server.kind != localSource {
		existed, err = revokeOnServer(row.server, row.device.TokenHash)
		if err != nil {
			return fmt.Errorf("revoke on %s (nothing was removed): %w", row.server.kind, err)
		}
	}
	removed, err := sessions.RemoveDevice(row.device.TokenHash)
	if err != nil {
		return fmt.Errorf("the %s token is revoked, but removing the local secret failed: %w", row.server.kind, err)
	}
	_, _ = fmt.Fprintf(out, "revoked %s: %s\n", shortHash(row.device.TokenHash),
		describeRevocation(row.server.kind, existed, removed))
	return nil
}

// pairingGrace is how long a server device row with no local secret is left
// alone before it counts as drift.
//
// `pair-device` creates the server row when the phone redeems its code, and
// writes the local secret only after a human has compared the pairing
// fingerprint across two screens -- so "row, no secret" is the ordinary state
// of a pairing in flight, for as long as that person takes to look. An hour
// is far longer than any such pause and still well inside the reaper's own
// period.
const pairingGrace = time.Hour

// olderThan reports whether an RFC3339 created_at is at least age old. An
// absent or unparseable timestamp reports false: without a known age the row
// cannot be told apart from a pairing in flight, and leaving a credential in
// place is the safe way to be wrong.
func olderThan(createdAt string, now time.Time, age time.Duration) bool {
	created, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return false
	}
	return now.Sub(created) >= age
}

// reapDriftedCredentials removes both halves of a pairing that has lost the
// other half, in either direction:
//
//   - a shared secret this Mac holds that no server has a device row for.
//     Inert -- no request can authenticate without the token -- so this is
//     hygiene rather than security, which is why it runs on a timer instead
//     of on the critical path of whatever revoked the token (cmux-app-f5y).
//   - a server device row whose shared secret this Mac no longer holds. That
//     one is not inert: it is a live bearer token that still authenticates at
//     the relay and still reaches the agent, for a device that provably
//     cannot be that device any more. 17 of them had accumulated by
//     2026-08-13 because only the first direction was ever reaped
//     (cmux-app-2vz).
//
// Together they are what makes revocation converge from any direction: a
// device revoked through the phone's Forget, through `term-bridge-relay devices
// revoke`, or by an operator here all end the same way instead of leaving the
// two stores to drift apart (cmux-app-vkq).
//
// The whole round is abandoned if any server failed to answer. A server that
// did not reply means its devices are unknown, not absent; without that
// guard a relay outage would unpair every device on this Mac.
func reapDriftedCredentials(servers []agentServer, sessions *e2e.Store, now time.Time) (secrets, tokens int, reached map[string]bool, err error) {
	rows, problems, reached := collectDevices(servers, sessions)
	if len(problems) > 0 {
		return 0, 0, reached, fmt.Errorf("skipped: %s", strings.Join(problems, "; "))
	}
	for _, row := range rows {
		if row.server.kind == localSource {
			removed, err := sessions.RemoveDevice(row.device.TokenHash)
			if err != nil {
				return secrets, tokens, reached, fmt.Errorf("remove stranded secret %s: %w", shortHash(row.device.TokenHash), err)
			}
			if removed {
				secrets++
				slog.Info("devices: reaped a shared secret no server knows about", "device", shortHash(row.device.TokenHash))
			}
			continue
		}
		if row.hasSecret || !olderThan(row.device.CreatedAt, now, pairingGrace) {
			continue
		}
		if _, err := revokeOnServer(row.server, row.device.TokenHash); err != nil {
			return secrets, tokens, reached, fmt.Errorf("revoke drifted %s token %s: %w",
				row.server.kind, shortHash(row.device.TokenHash), err)
		}
		tokens++
		slog.Info("devices: revoked a token whose shared secret this agent no longer holds",
			"device", shortHash(row.device.TokenHash), "source", row.server.kind)
	}
	return secrets, tokens, reached, nil
}

// runReaper reaps drifted credentials on a loop until ctx is done. The first
// round is delayed rather than immediate: at startup the relay tunnel is
// usually still coming up, and a round that cannot reach a server does
// nothing anyway.
// observe, when set, is handed each round's per-slot reachability -- see
// [reaperRound].
func runReaper(ctx context.Context, cfg config.AgentConfig, sessions *e2e.Store, delay, period time.Duration, observe func(map[string]bool)) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		reaperRound(cfg, sessions, observe)
		timer.Reset(period)
	}
}

// reaperRound runs one round and reports which slots answered.
//
// observe is called even when the round was abandoned, because a slot that did
// not answer is the entire reason this is recorded: the hourly listing is the
// only thing that probes each transport end to end, and until cmux-app-t5x it
// reported an unreachable standby nowhere but a WARN line.
func reaperRound(cfg config.AgentConfig, sessions *e2e.Store, observe func(map[string]bool)) {
	servers, err := configuredServers(cfg, deviceAdminTimeout)
	if err != nil {
		slog.Warn("devices: reaper found no reachable server", "err", err)
		return
	}
	secrets, tokens, reached, err := reapDriftedCredentials(servers, sessions, time.Now())
	if observe != nil {
		observe(reached)
	}
	switch {
	case err != nil:
		slog.Warn("devices: reaper round incomplete", "err", err)
	case secrets > 0 || tokens > 0:
		slog.Info("devices: reaper removed drifted credentials", "secrets", secrets, "tokens", tokens)
	}
}

func describeRevocation(source string, existed, secretRemoved bool) string {
	served := source + " token removed"
	switch {
	case source == localSource:
		served = "no server token (this was a stranded local secret)"
	case !existed:
		served = source + " had no such token already"
	}
	if secretRemoved {
		return served + ", local secret removed"
	}
	return served + ", no local secret held"
}

// runDevices implements `term-bridge devices`, the operator's view of who is
// paired and the only way to take a pairing back. Both subcommands act
// across every configured server, so the operator never has to know which
// slot a phone was paired through.
func runDevices(args []string) int {
	if len(args) == 0 {
		devicesUsage()
		return 2
	}
	sub, rest := args[0], args[1:]
	if sub != "list" && sub != "revoke" {
		devicesUsage()
		return 2
	}
	fs := flag.NewFlagSet("devices "+sub, flag.ContinueOnError)
	cfgPath := fs.String("config", defaultAgentConfigPath(), "path to agent.toml")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	cfg, err := config.LoadAgent(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load agent config:", err)
		return 1
	}
	servers, err := configuredServers(cfg, deviceAdminTimeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sessions, err := e2e.OpenStore(cfg.SessionStore)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open session store:", err)
		return 1
	}

	if sub == "list" {
		rows, problems, _ := collectDevices(servers, sessions)
		printDeviceRows(os.Stdout, rows)
		for _, problem := range problems {
			fmt.Fprintln(os.Stderr, "could not list devices on", problem)
		}
		if len(problems) > 0 {
			return 1
		}
		return 0
	}
	if len(fs.Args()) != 1 {
		devicesUsage()
		return 2
	}
	if err := revokeByPrefix(os.Stdout, servers, sessions, fs.Args()[0]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func devicesUsage() {
	fmt.Fprintln(os.Stderr, "usage: term-bridge devices list [flags]")
	fmt.Fprintln(os.Stderr, "       term-bridge devices revoke <device-prefix> [flags]")
}
