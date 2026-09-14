package main

import (
	"path/filepath"
	"testing"

	"github.com/sodre90/term-bridge/internal/auth"
)

const testPubkey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func newStore(t *testing.T) *auth.Store {
	t.Helper()
	store, err := auth.Open(filepath.Join(t.TempDir(), "devices.db"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// issueDevice pairs a device and returns both halves the CLI has to bridge:
// the raw token only the phone ever sees, and the hash the listing prints.
func issueDevice(t *testing.T, store *auth.Store, tenantID, name string) (token, tokenHash string) {
	t.Helper()
	token, err := store.Issue(tenantID, name, testPubkey)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := store.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	return token, dev.TokenHash
}

func newTenant(t *testing.T, store *auth.Store) string {
	t.Helper()
	id, err := store.CreateTenant()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The whole point of cmux-app-nvt: what `devices list` prints must be enough
// to revoke with.
func TestRevokeResolvesThePrefixTheListingPrints(t *testing.T) {
	store := newStore(t)
	tenant := newTenant(t, store)
	token, hash := issueDevice(t, store, tenant, "phone")

	if code := revokeDevice(store, shortHash(hash)); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, err := store.Verify(token); err == nil {
		t.Fatal("a revoked token must stop verifying")
	}
}

func TestRevokeAcceptsAFullHash(t *testing.T) {
	store := newStore(t)
	tenant := newTenant(t, store)
	token, hash := issueDevice(t, store, tenant, "phone")

	if code := revokeDevice(store, hash); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, err := store.Verify(token); err == nil {
		t.Fatal("a revoked token must stop verifying")
	}
}

// The old raw-token form is the only way to revoke a token that leaked
// without being paired through this relay's listing, so it still has to work.
func TestRevokeStillAcceptsARawToken(t *testing.T) {
	store := newStore(t)
	tenant := newTenant(t, store)
	token, _ := issueDevice(t, store, tenant, "phone")

	if code := revokeDevice(store, token); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, err := store.Verify(token); err == nil {
		t.Fatal("a revoked token must stop verifying")
	}
}

func TestRevokeRefusesAnAmbiguousPrefixWithoutTouchingAnything(t *testing.T) {
	store := newStore(t)
	tenant := newTenant(t, store)
	firstToken, _ := issueDevice(t, store, tenant, "phone-1")
	secondToken, _ := issueDevice(t, store, tenant, "phone-2")

	// The empty prefix covers every device, which is the ambiguity this has
	// to refuse without needing two hashes to collide by luck.
	if code := revokeDevice(store, ""); code == 0 {
		t.Fatal("an ambiguous prefix must not revoke anything")
	}
	for _, token := range []string{firstToken, secondToken} {
		if _, err := store.Verify(token); err != nil {
			t.Fatalf("a refused revoke must leave every device intact: %v", err)
		}
	}
}

func TestRevokeOfAnUnknownPrefixFails(t *testing.T) {
	store := newStore(t)
	tenant := newTenant(t, store)
	token, _ := issueDevice(t, store, tenant, "phone")

	if code := revokeDevice(store, "zzzzzzzz"); code == 0 {
		t.Fatal("an unmatched prefix must fail")
	}
	if _, err := store.Verify(token); err != nil {
		t.Fatalf("a failed revoke must leave the device intact: %v", err)
	}
}

// This CLI is the one cross-tenant caller, so the tenant it passes to
// RevokeByHash has to be the resolved device's own -- not a caller's guess.
func TestRevokeUsesTheResolvedDevicesOwnTenant(t *testing.T) {
	store := newStore(t)
	tenantA, tenantB := newTenant(t, store), newTenant(t, store)
	tokenA, hashA := issueDevice(t, store, tenantA, "phone-a")
	tokenB, _ := issueDevice(t, store, tenantB, "phone-b")

	if code := revokeDevice(store, hashA); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, err := store.Verify(tokenA); err == nil {
		t.Fatal("tenant A's device should be gone")
	}
	if _, err := store.Verify(tokenB); err != nil {
		t.Fatalf("tenant B's device must be untouched: %v", err)
	}
}

func TestMatchingDevicesPrefersAnExactHashOverAPrefix(t *testing.T) {
	devs := []auth.Device{
		{TokenHash: "abcd", Name: "shorter"},
		{TokenHash: "abcd1234", Name: "longer"},
	}
	matches := matchingDevices(devs, "abcd")
	if len(matches) != 1 || matches[0].Name != "shorter" {
		t.Fatalf("exact hash should win outright, got %+v", matches)
	}
}

// A --config stranded behind the subcommand used to parse as nothing at all,
// so the command read the default store instead of the one named -- the same
// wrong-database trap, one layer up from cmux-app-xdc.
func TestAdminArgsRefuseAFlagBehindTheSubcommand(t *testing.T) {
	if _, _, ok := parseAdminArgs("devices", []string{"list", "--config", "/etc/term-bridge-relay/config.toml"}); ok {
		t.Fatal("a flag after the subcommand must be refused, not silently ignored")
	}
}

func TestAdminArgsReadTheConfigFlagBeforeTheSubcommand(t *testing.T) {
	cfgPath, rest, ok := parseAdminArgs("devices", []string{"--config", "/etc/term-bridge-relay/config.toml", "revoke", "abcd"})
	if !ok {
		t.Fatal("the documented argument order must parse")
	}
	if cfgPath != "/etc/term-bridge-relay/config.toml" {
		t.Fatalf("cfgPath = %q", cfgPath)
	}
	if len(rest) != 2 || rest[0] != "revoke" || rest[1] != "abcd" {
		t.Fatalf("rest = %v, want [revoke abcd]", rest)
	}
}

func TestAdminArgsDefaultToTheBareSubcommand(t *testing.T) {
	cfgPath, rest, ok := parseAdminArgs("devices", nil)
	if !ok || cfgPath == "" || len(rest) != 0 {
		t.Fatalf("bare `devices` should parse with the default config: ok=%v cfg=%q rest=%v", ok, cfgPath, rest)
	}
}
