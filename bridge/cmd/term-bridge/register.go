package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sodre90/term-bridge/internal/config"
)

type registerResp struct {
	TenantID string `json:"tenant_id"`
	CertPEM  string `json:"cert_pem"`
}

// ensureRegistered generates a keypair and self-registers with the relay's
// bootstrap endpoint if cfg.ClientCert doesn't already exist on disk. It is a
// no-op once registration has happened once — an agent identity, once
// minted, is reused for the agent's lifetime.
func ensureRegistered(cfg config.AgentConfig) error {
	if _, err := os.Stat(cfg.ClientCert); err == nil {
		return nil
	}
	if cfg.BootstrapURL == "" {
		return fmt.Errorf("no client_cert on disk and bootstrap_url is empty — set one in agent.toml")
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate agent key: %w", err)
	}
	csrTmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: "pending"}}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTmpl, key)
	if err != nil {
		return fmt.Errorf("create CSR: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	body, err := json.Marshal(map[string]string{"csr": string(csrPEM)})
	if err != nil {
		return fmt.Errorf("marshal registration request: %w", err)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(cfg.BootstrapURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("register with relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("register with relay: status %d", resp.StatusCode)
	}
	var rr registerResp
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return fmt.Errorf("parse register response: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal agent key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := writeNew(cfg.ClientKey, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write client key: %w", err)
	}
	if err := writeNew(cfg.ClientCert, []byte(rr.CertPEM), 0o644); err != nil {
		return fmt.Errorf("write client cert: %w", err)
	}
	fmt.Printf("agent: registered as tenant %s (cert written to %s)\n", rr.TenantID, cfg.ClientCert)
	return nil
}

func writeNew(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create dir for %s: %w", path, err)
	}
	if err := os.WriteFile(path, data, perm); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
