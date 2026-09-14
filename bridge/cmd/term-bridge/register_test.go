package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sodre90/term-bridge/internal/config"
)

func TestEnsureRegisteredCallsBootstrapAndWritesFiles(t *testing.T) {
	dir := t.TempDir()
	var gotCSR string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			CSR string `json:"csr"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotCSR = body.CSR
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"tenant_id": "abc123",
			"cert_pem":  "FAKE CERT",
			"ca_pem":    "FAKE CA",
		})
	}))
	defer srv.Close()

	cfg := config.AgentConfig{
		ClientCert:   filepath.Join(dir, "agent.crt"),
		ClientKey:    filepath.Join(dir, "agent.key"),
		CACert:       filepath.Join(dir, "ca.crt"),
		BootstrapURL: srv.URL,
	}
	if err := ensureRegistered(cfg); err != nil {
		t.Fatal(err)
	}
	if gotCSR == "" {
		t.Fatal("bootstrap server should have received a non-empty CSR")
	}
	cert, err := os.ReadFile(cfg.ClientCert)
	if err != nil || string(cert) != "FAKE CERT" {
		t.Fatalf("client cert not written correctly: %v %q", err, cert)
	}
	if _, err := os.ReadFile(cfg.ClientKey); err != nil {
		t.Fatalf("client key not written: %v", err)
	}
	if _, err := os.Stat(cfg.CACert); !os.IsNotExist(err) {
		t.Fatalf("ca_cert must not be written by self-registration, got err=%v", err)
	}
}

func TestEnsureRegisteredSkipsIfAlreadyRegistered(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "agent.crt")
	if err := os.WriteFile(certPath, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	cfg := config.AgentConfig{ClientCert: certPath, BootstrapURL: srv.URL}
	if err := ensureRegistered(cfg); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("bootstrap server must not be called when a cert already exists")
	}
}

func TestEnsureRegisteredErrorsWithNoBootstrapURL(t *testing.T) {
	dir := t.TempDir()
	cfg := config.AgentConfig{ClientCert: filepath.Join(dir, "agent.crt")}
	if err := ensureRegistered(cfg); err == nil {
		t.Fatal("want an error when no cert exists and bootstrap_url is empty")
	}
}
