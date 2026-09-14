package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sodre90/term-bridge/internal/auth"
)

func writeSelfSigned(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mac-agent"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "c.pem")
	keyPath = filepath.Join(dir, "k.pem")
	cf, _ := os.Create(certPath)
	_ = pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	cf.Close()
	kf, _ := os.Create(keyPath)
	_ = pem.Encode(kf, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	kf.Close()
	return certPath, keyPath
}

func TestLoadTLS(t *testing.T) {
	cert, key := writeSelfSigned(t)
	cfg, err := loadTLS(cert, key, cert) // self-signed: cert doubles as its CA
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("want 1 client cert, got %d", len(cfg.Certificates))
	}
	if cfg.RootCAs == nil {
		t.Fatal("want RootCAs set from ca_cert")
	}
}

func TestLoadTLSEmptyCAUsesSystemRoots(t *testing.T) {
	// A Let's Encrypt server cert is publicly trusted, so an empty ca_cert must
	// not error and must leave RootCAs nil (Go falls back to the system roots).
	cert, key := writeSelfSigned(t)
	cfg, err := loadTLS(cert, key, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootCAs != nil {
		t.Fatal("empty ca_cert should leave RootCAs nil (system roots)")
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("want 1 client cert, got %d", len(cfg.Certificates))
	}
}

func TestLoadTLSMissingFileErrors(t *testing.T) {
	if _, err := loadTLS("/no/cert", "/no/key", "/no/ca"); err == nil {
		t.Fatal("want error for missing cert files")
	}
}

func TestRunDirectListenerAutoCreatesTenantOnce(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "direct-auth.db")
	store, err := auth.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	tenantID, err := ensureDirectTenant(store)
	if err != nil {
		t.Fatal(err)
	}
	if tenantID == "" {
		t.Fatal("expected a non-empty tenant id")
	}

	// Calling it again against the SAME store must reuse the existing
	// tenant, not mint a second one -- otherwise toggling direct_listen
	// on/off across restarts would orphan every previously paired device.
	again, err := ensureDirectTenant(store)
	if err != nil {
		t.Fatal(err)
	}
	if again != tenantID {
		t.Fatalf("ensureDirectTenant not idempotent: got %q then %q", tenantID, again)
	}
}

func TestDirectListenPortExtractsPort(t *testing.T) {
	port, err := directListenPort(":8443")
	if err != nil {
		t.Fatal(err)
	}
	if port != "8443" {
		t.Fatalf("port = %q, want 8443", port)
	}
}

func TestDirectListenPortRejectsMalformedAddr(t *testing.T) {
	if _, err := directListenPort("not-a-valid-addr"); err == nil {
		t.Fatal("want error for an address with no port")
	}
}

func TestSelfTailscaleIPv4PrefersIPv4(t *testing.T) {
	st := &tailscaleSelf{
		TailscaleIPs: []netip.Addr{
			netip.MustParseAddr("fd7a:115c:a1e0::1"), // IPv6 tailnet address, listed first
			netip.MustParseAddr("100.64.1.2"),
		},
	}
	ip, err := selfTailscaleIPv4(st)
	if err != nil {
		t.Fatal(err)
	}
	if ip.String() != "100.64.1.2" {
		t.Fatalf("ip = %q, want 100.64.1.2", ip.String())
	}
}

func TestSelfTailscaleIPv4NilStatusErrors(t *testing.T) {
	if _, err := selfTailscaleIPv4(nil); err == nil {
		t.Fatal("want error for a nil status")
	}
}

func TestSelfTailscaleIPv4NoIPv4Errors(t *testing.T) {
	st := &tailscaleSelf{
		TailscaleIPs: []netip.Addr{
			netip.MustParseAddr("fd7a:115c:a1e0::1"), // IPv6-only, e.g. IPv4 not yet assigned
		},
	}
	if _, err := selfTailscaleIPv4(st); err == nil {
		t.Fatal("want error when the node has no Tailscale IPv4 address")
	}
}
