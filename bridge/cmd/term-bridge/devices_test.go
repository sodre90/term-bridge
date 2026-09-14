package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sodre90/term-bridge/internal/e2e"
	"github.com/sodre90/term-bridge/internal/wire"
)

// fakeAdminServer is the agent-facing half of the device-admin routes,
// standing in for a relay or a direct listener. revoked records what the CLI
// asked it to delete, which is how a test sees whether the server half ran
// at all.
type fakeAdminServer struct {
	*httptest.Server
	devices []wire.AgentDevice
	// revokeStatus, when non-zero, is what the revoke route answers instead
	// of succeeding -- how a test makes the server half fail.
	revokeStatus int32
	revoked      []string
	listCalls    int32
}

func fakeDeviceAdmin(t *testing.T, devices ...wire.AgentDevice) *fakeAdminServer {
	t.Helper()
	srv := &fakeAdminServer{devices: devices}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/devices", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&srv.listCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wire.AgentDeviceListResp{Devices: srv.devices})
	})
	mux.HandleFunc("POST /agent/devices/{tokenHash}/revoke", func(w http.ResponseWriter, r *http.Request) {
		if status := int(atomic.LoadInt32(&srv.revokeStatus)); status != 0 {
			w.WriteHeader(status)
			return
		}
		hash := r.PathValue("tokenHash")
		srv.revoked = append(srv.revoked, hash)
		for i, dev := range srv.devices {
			if dev.TokenHash == hash {
				srv.devices = append(srv.devices[:i], srv.devices[i+1:]...)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("{}"))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv.Server = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeAdminServer) as(kind string) agentServer {
	return agentServer{kind: kind, baseURL: f.URL, client: f.Client()}
}

func newSessions(t *testing.T) *e2e.Store {
	t.Helper()
	sessions, err := e2e.OpenStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return sessions
}

// addSecret gives sessions a shared secret for deviceID. Each device gets a
// freshly generated peer key, because AddDevice refuses to file the same
// secret under two device ids.
func addSecret(t *testing.T, sessions *e2e.Store, deviceID string) {
	t.Helper()
	agentKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	deviceKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := e2e.DeriveSharedSecret(agentKey, deviceKey.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.AddDevice(deviceID, deviceKey.PublicKey(), secret); err != nil {
		t.Fatal(err)
	}
}

func hasSecret(t *testing.T, sessions *e2e.Store, deviceID string) bool {
	t.Helper()
	_, ok := sessions.SharedSecret(deviceID)
	return ok
}

func TestListJoinsServerDevicesToLocalSecrets(t *testing.T) {
	srv := fakeDeviceAdmin(t,
		wire.AgentDevice{Name: "phone-1", TokenHash: "aaaa1111", CreatedAt: "2026-06-29T19:20:42Z"},
		wire.AgentDevice{Name: "phone-2", TokenHash: "bbbb2222", CreatedAt: "2026-06-30T13:37:00Z"},
	)
	sessions := newSessions(t)
	addSecret(t, sessions, "aaaa1111")

	rows, problems, _ := collectDevices([]agentServer{srv.as("relay")}, sessions)
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(rows), rows)
	}
	if !rows[0].hasSecret {
		t.Error("aaaa1111 has a local secret and should be reported as such")
	}
	if rows[1].hasSecret {
		t.Error("bbbb2222 has no local secret and should be reported as such")
	}
}

// A secret with no server-side token is the drift state this command exists
// to clean up; if the listing hid it, nothing could ever reap it.
func TestListReportsSecretsNoServerKnowsAbout(t *testing.T) {
	srv := fakeDeviceAdmin(t, wire.AgentDevice{Name: "phone-1", TokenHash: "aaaa1111"})
	sessions := newSessions(t)
	addSecret(t, sessions, "aaaa1111")
	addSecret(t, sessions, "cccc3333")

	rows, _, _ := collectDevices([]agentServer{srv.as("relay")}, sessions)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(rows), rows)
	}
	orphan := rows[1]
	if orphan.server.kind != localSource || orphan.device.TokenHash != "cccc3333" || !orphan.hasSecret {
		t.Fatalf("stranded secret not reported as local: %+v", orphan)
	}
}

// An unreachable server must not make its devices look like stranded local
// secrets -- reaping one of those would drop a live device's secret.
func TestListDoesNotInventOrphansWhenAServerIsDown(t *testing.T) {
	srv := fakeDeviceAdmin(t, wire.AgentDevice{Name: "phone-1", TokenHash: "aaaa1111"})
	srv.Close()
	sessions := newSessions(t)
	addSecret(t, sessions, "aaaa1111")

	rows, problems, _ := collectDevices([]agentServer{srv.as("relay")}, sessions)
	if len(problems) != 1 {
		t.Fatalf("want 1 problem from the dead server, got %v", problems)
	}
	if len(rows) != 0 {
		t.Fatalf("a dead server must not turn live devices into local orphans: %+v", rows)
	}
}

func TestResolveDevicePrefix(t *testing.T) {
	rows := []deviceRow{
		{server: agentServer{kind: "relay"}, device: wire.AgentDevice{TokenHash: "aaaa1111"}},
		{server: agentServer{kind: "relay"}, device: wire.AgentDevice{TokenHash: "aaaa2222"}},
		{server: agentServer{kind: "direct"}, device: wire.AgentDevice{TokenHash: "bbbb3333"}},
	}
	tests := []struct {
		name    string
		prefix  string
		want    string
		wantErr string
	}{
		{name: "exact", prefix: "aaaa1111", want: "aaaa1111"},
		{name: "unique prefix", prefix: "bbbb", want: "bbbb3333"},
		{name: "ambiguous prefix", prefix: "aaaa", wantErr: "matches 2 devices"},
		{name: "no match", prefix: "zzzz", wantErr: "no device matches"},
		{name: "empty", prefix: "", wantErr: "no device given"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveDevicePrefix(rows, tt.prefix)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want an error containing %q, resolved %q", tt.wantErr, got.device.TokenHash)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.device.TokenHash != tt.want {
				t.Fatalf("resolved %q, want %q", got.device.TokenHash, tt.want)
			}
		})
	}
}

func TestRevokeRemovesBothTheTokenAndTheSecret(t *testing.T) {
	srv := fakeDeviceAdmin(t, wire.AgentDevice{Name: "phone-1", TokenHash: "aaaa1111"})
	sessions := newSessions(t)
	addSecret(t, sessions, "aaaa1111")

	var out bytes.Buffer
	if err := revokeByPrefix(&out, []agentServer{srv.as("relay")}, sessions, "aaaa"); err != nil {
		t.Fatal(err)
	}
	if len(srv.revoked) != 1 || srv.revoked[0] != "aaaa1111" {
		t.Fatalf("server revocations = %v, want [aaaa1111]", srv.revoked)
	}
	if hasSecret(t, sessions, "aaaa1111") {
		t.Fatal("the local shared secret should be gone")
	}
	if !strings.Contains(out.String(), "local secret removed") {
		t.Fatalf("output should report both halves: %q", out.String())
	}
}

// The point of running the local half unconditionally: a device the server
// has already forgotten is exactly the one whose secret is stranded here.
func TestRevokeStillReapsTheSecretWhenTheServerNeverHeardOfTheDevice(t *testing.T) {
	srv := fakeDeviceAdmin(t, wire.AgentDevice{Name: "phone-1", TokenHash: "aaaa1111"})
	sessions := newSessions(t)
	addSecret(t, sessions, "aaaa1111")
	// The device is still in the listing but gone by the time we revoke it --
	// what a device revoked out from under us between the two calls looks like.
	atomic.StoreInt32(&srv.revokeStatus, http.StatusNotFound)

	var out bytes.Buffer
	if err := revokeByPrefix(&out, []agentServer{srv.as("relay")}, sessions, "aaaa1111"); err != nil {
		t.Fatal(err)
	}
	if hasSecret(t, sessions, "aaaa1111") {
		t.Fatal("a stranded secret should still be reaped on a 404")
	}
	if !strings.Contains(out.String(), "had no such token already") {
		t.Fatalf("the two outcomes should be reported separately: %q", out.String())
	}
}

// A revocation that failed on the server must not half-complete: leaving the
// token alive while dropping the secret is strictly worse than doing nothing.
func TestRevokeKeepsTheSecretWhenTheServerFails(t *testing.T) {
	srv := fakeDeviceAdmin(t, wire.AgentDevice{Name: "phone-1", TokenHash: "aaaa1111"})
	sessions := newSessions(t)
	addSecret(t, sessions, "aaaa1111")
	atomic.StoreInt32(&srv.revokeStatus, http.StatusInternalServerError)

	err := revokeByPrefix(&bytes.Buffer{}, []agentServer{srv.as("relay")}, sessions, "aaaa1111")
	if err == nil {
		t.Fatal("a failed server revocation must be reported as an error")
	}
	if !hasSecret(t, sessions, "aaaa1111") {
		t.Fatal("the local secret must survive a failed server revocation")
	}
}

func TestRevokeOfAStrandedSecretNeverCallsAServer(t *testing.T) {
	srv := fakeDeviceAdmin(t)
	sessions := newSessions(t)
	addSecret(t, sessions, "cccc3333")

	var out bytes.Buffer
	if err := revokeByPrefix(&out, []agentServer{srv.as("relay")}, sessions, "cccc"); err != nil {
		t.Fatal(err)
	}
	if len(srv.revoked) != 0 {
		t.Fatalf("no server owns this device, yet %v was revoked remotely", srv.revoked)
	}
	if hasSecret(t, sessions, "cccc3333") {
		t.Fatal("the stranded secret should be gone")
	}
}

// Resolving a prefix against a listing that is missing a server's devices
// could pick the wrong device, so revocation refuses outright.
func TestRevokeRefusesAPartialListing(t *testing.T) {
	live := fakeDeviceAdmin(t, wire.AgentDevice{Name: "phone-1", TokenHash: "aaaa1111"})
	dead := fakeDeviceAdmin(t)
	dead.Close()
	sessions := newSessions(t)
	addSecret(t, sessions, "aaaa1111")

	err := revokeByPrefix(&bytes.Buffer{}, []agentServer{live.as("relay"), dead.as("direct")}, sessions, "aaaa1111")
	if err == nil {
		t.Fatal("want a refusal when one server could not be listed")
	}
	if len(live.revoked) != 0 {
		t.Fatalf("nothing should have been revoked, got %v", live.revoked)
	}
	if !hasSecret(t, sessions, "aaaa1111") {
		t.Fatal("the local secret must survive a refused revocation")
	}
}

func TestPrintDeviceRowsShowsTheDriftColumn(t *testing.T) {
	rows := []deviceRow{
		{server: agentServer{kind: "relay"}, device: wire.AgentDevice{Name: "phone-1", TokenHash: "aaaa1111bbbb2222", CreatedAt: "2026-06-29T19:20:42Z"}, hasSecret: true},
		{server: agentServer{kind: localSource}, device: wire.AgentDevice{TokenHash: "cccc3333dddd4444"}, hasSecret: true},
	}
	var out bytes.Buffer
	printDeviceRows(&out, rows)

	got := out.String()
	for _, want := range []string{"SOURCE", "aaaa1111bbbb", "phone-1", "yes", localSource, "cccc3333dddd"} {
		if !strings.Contains(got, want) {
			t.Fatalf("listing is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "aaaa1111bbbb2222") {
		t.Fatalf("the listing should print a short hash, not the whole one:\n%s", got)
	}
}

func TestReaperRemovesSecretsNoServerKnowsAbout(t *testing.T) {
	srv := fakeDeviceAdmin(t, wire.AgentDevice{Name: "phone-1", TokenHash: "aaaa1111"})
	sessions := newSessions(t)
	addSecret(t, sessions, "aaaa1111")
	addSecret(t, sessions, "cccc3333")

	reaped, _, _, err := reapDriftedCredentials([]agentServer{srv.as("relay")}, sessions, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 1 {
		t.Fatalf("reaped = %d, want 1", reaped)
	}
	if hasSecret(t, sessions, "cccc3333") {
		t.Fatal("the stranded secret should be gone")
	}
	if !hasSecret(t, sessions, "aaaa1111") {
		t.Fatal("a secret the server still lists must survive")
	}
}

// Without this guard a relay outage would unpair every device on the Mac:
// unreachable means the server's devices are unknown, not absent.
func TestReaperTouchesNothingWhenAServerIsUnreachable(t *testing.T) {
	live := fakeDeviceAdmin(t, wire.AgentDevice{Name: "phone-1", TokenHash: "aaaa1111"})
	dead := fakeDeviceAdmin(t)
	dead.Close()
	sessions := newSessions(t)
	addSecret(t, sessions, "aaaa1111")
	addSecret(t, sessions, "cccc3333")

	reaped, _, _, err := reapDriftedCredentials([]agentServer{live.as("relay"), dead.as("direct")}, sessions, time.Now())
	if err == nil {
		t.Fatal("an incomplete round must be reported, not treated as a clean sweep")
	}
	if reaped != 0 {
		t.Fatalf("reaped = %d, want 0", reaped)
	}
	for _, id := range []string{"aaaa1111", "cccc3333"} {
		if !hasSecret(t, sessions, id) {
			t.Fatalf("%s was reaped despite an unreachable server", id)
		}
	}
}

func TestReaperIsANoOpWhenNothingIsStranded(t *testing.T) {
	srv := fakeDeviceAdmin(t, wire.AgentDevice{Name: "phone-1", TokenHash: "aaaa1111"})
	sessions := newSessions(t)
	addSecret(t, sessions, "aaaa1111")

	reaped, _, _, err := reapDriftedCredentials([]agentServer{srv.as("relay")}, sessions, time.Now())
	if err != nil || reaped != 0 {
		t.Fatalf("reaped = %d, err = %v; want 0, nil", reaped, err)
	}
	if !hasSecret(t, sessions, "aaaa1111") {
		t.Fatal("a live device's secret must survive")
	}
}

// aged builds a server device row created age ago, which is what decides
// whether the reaper may treat a missing local secret as drift.
func aged(name, tokenHash string, age time.Duration) wire.AgentDevice {
	return wire.AgentDevice{
		Name:      name,
		TokenHash: tokenHash,
		CreatedAt: time.Now().Add(-age).UTC().Format(time.RFC3339),
	}
}

// cmux-app-2vz: a server row whose shared secret this Mac no longer holds is
// a live bearer token for a device that provably cannot be that device any
// more. Reaping ran in one direction only, so 17 of them piled up.
func TestReaperRevokesTokensWhoseSecretIsGone(t *testing.T) {
	srv := fakeDeviceAdmin(t,
		aged("phone-1", "aaaa1111", 48*time.Hour),
		aged("phone-old", "bbbb2222", 48*time.Hour),
	)
	sessions := newSessions(t)
	addSecret(t, sessions, "aaaa1111")

	secrets, tokens, _, err := reapDriftedCredentials([]agentServer{srv.as("relay")}, sessions, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if secrets != 0 || tokens != 1 {
		t.Fatalf("secrets = %d, tokens = %d; want 0, 1", secrets, tokens)
	}
	if len(srv.revoked) != 1 || srv.revoked[0] != "bbbb2222" {
		t.Fatalf("revoked %v, want just the secretless bbbb2222", srv.revoked)
	}
	if !hasSecret(t, sessions, "aaaa1111") {
		t.Fatal("the live device's secret must survive")
	}
}

// pair-device creates the server row when the phone redeems its code and
// writes the local secret only after a human compares the pairing
// fingerprint. Without the grace period the reaper would revoke whatever
// pairing happened to be waiting on that person.
func TestReaperLeavesAPairingInFlightAlone(t *testing.T) {
	srv := fakeDeviceAdmin(t, aged("phone-new", "bbbb2222", time.Minute))
	sessions := newSessions(t)

	secrets, tokens, _, err := reapDriftedCredentials([]agentServer{srv.as("relay")}, sessions, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if secrets != 0 || tokens != 0 {
		t.Fatalf("secrets = %d, tokens = %d; want the in-flight pairing untouched", secrets, tokens)
	}
	if len(srv.revoked) != 0 {
		t.Fatalf("a pairing still being confirmed was revoked: %v", srv.revoked)
	}
}

// A row whose age cannot be established is not a row whose age is old. The
// safe way to be wrong about a credential is to leave it in place.
func TestReaperLeavesARowWithNoUsableTimestampAlone(t *testing.T) {
	srv := fakeDeviceAdmin(t,
		wire.AgentDevice{Name: "no-date", TokenHash: "bbbb2222"},
		wire.AgentDevice{Name: "bad-date", TokenHash: "cccc3333", CreatedAt: "yesterday"},
	)
	sessions := newSessions(t)

	_, tokens, _, err := reapDriftedCredentials([]agentServer{srv.as("relay")}, sessions, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if tokens != 0 || len(srv.revoked) != 0 {
		t.Fatalf("tokens = %d, revoked = %v; want nothing touched", tokens, srv.revoked)
	}
}

// The unreachable-server guard has to cover the new direction too: an
// incomplete listing must not be read as "the secret for this row is gone".
func TestReaperRevokesNoTokensWhenAServerIsUnreachable(t *testing.T) {
	live := fakeDeviceAdmin(t, aged("phone-old", "bbbb2222", 48*time.Hour))
	dead := fakeDeviceAdmin(t)
	dead.Close()
	sessions := newSessions(t)

	if _, tokens, _, err := reapDriftedCredentials([]agentServer{live.as("relay"), dead.as("direct")}, sessions, time.Now()); err == nil {
		t.Fatal("an incomplete round must be reported")
	} else if tokens != 0 {
		t.Fatalf("tokens = %d, want 0", tokens)
	}
	if len(live.revoked) != 0 {
		t.Fatalf("revoked %v despite an unreachable server", live.revoked)
	}
}

// A device paired on one slot only must not be reaped off it just because
// the other slot has no row for it -- the secret is matched across the whole
// listing, not per server.
func TestReaperKeepsADeviceKnownToOnlyOneServer(t *testing.T) {
	relay := fakeDeviceAdmin(t, aged("phone-1", "aaaa1111", 48*time.Hour))
	direct := fakeDeviceAdmin(t)
	sessions := newSessions(t)
	addSecret(t, sessions, "aaaa1111")

	secrets, tokens, _, err := reapDriftedCredentials([]agentServer{relay.as("relay"), direct.as("direct")}, sessions, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if secrets != 0 || tokens != 0 {
		t.Fatalf("secrets = %d, tokens = %d; want nothing reaped", secrets, tokens)
	}
}

// -- per-slot reachability (cmux-app-t5x)

// The hourly listing is the only end-to-end probe the agent runs against its
// own transports. It knew the direct standby was unreachable for 14 days and
// reported it nowhere but a WARN.
func TestAReapRoundReportsWhichSlotsAnswered(t *testing.T) {
	live := fakeDeviceAdmin(t, wire.AgentDevice{Name: "phone-1", TokenHash: "aaaa1111"})
	dead := fakeDeviceAdmin(t)
	dead.Close()
	sessions := newSessions(t)
	addSecret(t, sessions, "aaaa1111")

	_, _, reached, err := reapDriftedCredentials(
		[]agentServer{live.as("relay"), dead.as("direct")}, sessions, time.Now())

	if err == nil {
		t.Fatal("want the round reported as incomplete")
	}
	if !reached["relay"] {
		t.Error("relay answered and must be recorded as reached")
	}
	if reached["direct"] {
		t.Error("direct did not answer and must not be recorded as reached")
	}
}

// Reachability has to survive the early return: a round abandoned because one
// slot was unreachable is exactly the round whose answer is worth keeping.
func TestAnAbandonedRoundStillReportsReachability(t *testing.T) {
	dead := fakeDeviceAdmin(t)
	dead.Close()
	sessions := newSessions(t)

	_, _, reached, err := reapDriftedCredentials([]agentServer{dead.as("direct")}, sessions, time.Now())

	if err == nil {
		t.Fatal("want an error for an unreachable server")
	}
	if len(reached) != 1 {
		t.Fatalf("reachability was dropped on the abandoned path: %v", reached)
	}
	if reached["direct"] {
		t.Error("direct must be recorded as not reached")
	}
}

func TestEveryConfiguredSlotIsAccountedFor(t *testing.T) {
	relay := fakeDeviceAdmin(t)
	direct := fakeDeviceAdmin(t)
	sessions := newSessions(t)

	_, _, reached, err := reapDriftedCredentials(
		[]agentServer{relay.as("relay"), direct.as("direct")}, sessions, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	for _, slot := range []string{"relay", "direct"} {
		if !reached[slot] {
			t.Errorf("%s answered but is not recorded as reached", slot)
		}
	}
	if len(reached) != 2 {
		t.Fatalf("want exactly the configured slots, got %v", reached)
	}
}
