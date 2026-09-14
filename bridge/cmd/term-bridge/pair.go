package main

import (
	"bufio"
	"bytes"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"

	"github.com/sodre90/term-bridge/internal/config"
	"github.com/sodre90/term-bridge/internal/e2e"
)

// pairingRequestTimeout bounds a single pairing-code request/poll HTTP call.
// pairDevice's own deadline only gets checked between poll iterations, so
// without a client-level timeout a single hung relay connection could block
// pairDevice indefinitely instead of retrying or giving up.
const pairingRequestTimeout = 10 * time.Second

// pairingQR is the JSON payload rendered into the QR code. The phone scans
// it, generates its own e2e keypair, and POSTs PairURL directly — it needs
// nothing else to complete self-service pairing.
type pairingQR struct {
	PairURL     string `json:"pair_url"`
	Code        string `json:"code"`
	AgentPubkey string `json:"agent_pubkey"` // base64 X25519 public key
	ExpiresAt   string `json:"expires_at"`
	TenantID    string `json:"tenant_id"` // informational only -- /devices/pair never needs it in the request
}

// httpsBaseFromRelayURL converts the agent's wss:// tunnel URL
// (e.g. "wss://cmux.example.com/agent/tunnel") into the https base the same
// mTLS vhost serves the agent-facing pairing-code endpoints on
// (e.g. "https://cmux.example.com").
func httpsBaseFromRelayURL(relayURL string) (string, error) {
	u, err := url.Parse(relayURL)
	if err != nil {
		return "", fmt.Errorf("parse relay_url: %w", err)
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	default:
		return "", fmt.Errorf("relay_url must be ws:// or wss://, got %q", u.Scheme)
	}
	u.Path = ""
	return u.String(), nil
}

func requestPairingCode(client *http.Client, agentBase, agentPubkeyB64 string) (code, expiresAt, tenantID string, err error) {
	payload, err := json.Marshal(struct {
		AgentPubkey string `json:"agent_pubkey"`
	}{AgentPubkey: agentPubkeyB64})
	if err != nil {
		return "", "", "", err
	}
	resp, err := client.Post(agentBase+"/agent/pairing-code", "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var body struct {
		Code      string `json:"code"`
		ExpiresAt string `json:"expires_at"`
		TenantID  string `json:"tenant_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", "", err
	}
	return body.Code, body.ExpiresAt, body.TenantID, nil
}

func pollPairingCode(client *http.Client, agentBase, code string) (devicePubkey, tokenHash string, redeemed bool, err error) {
	resp, err := client.Get(agentBase + "/agent/pairing-code/" + code)
	if err != nil {
		return "", "", false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", false, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var body struct {
		Redeemed     bool   `json:"redeemed"`
		DevicePubkey string `json:"device_pubkey"`
		TokenHash    string `json:"token_hash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", false, err
	}
	return body.DevicePubkey, body.TokenHash, body.Redeemed, nil
}

// abortPairing destroys whatever the pairing code produced: the device token
// redemption already minted, and the code itself. Reports whether a token
// was actually revoked, so the operator is told plainly that the phone's
// credential is dead rather than merely that the pairing didn't finish.
func abortPairing(client *http.Client, agentBase, code string) (revoked bool, err error) {
	req, err := http.NewRequest(http.MethodDelete, agentBase+"/agent/pairing-code/"+code, nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var body struct {
		Revoked bool `json:"revoked"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, err
	}
	return body.Revoked, nil
}

// confirmPairing records the operator's yes, which is what releases the
// phone: until this lands the phone holds the whole pairing in memory and
// persists nothing (cmux-app-gmo). It is the last step of pairDevice and the
// single commit point of the flow.
func confirmPairing(client *http.Client, agentBase, code string) error {
	req, err := http.NewRequest(http.MethodPost, agentBase+"/agent/pairing-code/"+code+"/confirm", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// abandonPairing rolls back a pairing that got as far as redemption but must
// not stand, and reports what the operator now needs to know. Redemption
// hands the phone a working bearer token before anyone has confirmed
// anything, so leaving it in place would trust a pairing that was refused or
// that failed to complete (cmux-app-af1). A failed rollback is escalated
// rather than logged, because the live credential is then still out there.
func abandonPairing(client *http.Client, agentBase, code, why string, out io.Writer) error {
	revoked, err := abortPairing(client, agentBase, code)
	if err != nil {
		return fmt.Errorf("%s, but revoking the device token the phone already holds FAILED (revoke it by hand): %w", why, err)
	}
	if revoked {
		_, _ = fmt.Fprintf(out, "Revoked the device token this pairing had already issued.\n")
	}
	return fmt.Errorf("%s", why)
}

// pairDevice runs one pairing session: request a fresh pairing code from the
// relay, render it (with the agent's e2e public key and the phone's
// no-cert pairing URL) as a QR code to out, then poll agentBase until the
// code is redeemed or deadline passes. On redemption it derives the shared
// secret with the redeeming device and persists it to sessions, keyed by the
// device's token hash — the same key the relay's proxy Director injects as
// X-Device-ID on every subsequent request from that device.
func pairDevice(client *http.Client, agentBase, devicePairURL string, identity *e2e.Identity, sessions *e2e.Store, out io.Writer, in io.Reader, pollPeriod time.Duration, deadline time.Time) error {
	agentPubkeyB64 := base64.StdEncoding.EncodeToString(identity.PublicKey().Bytes())
	code, expiresAt, tenantID, err := requestPairingCode(client, agentBase, agentPubkeyB64)
	if err != nil {
		return fmt.Errorf("request pairing code: %w", err)
	}

	qr := pairingQR{
		PairURL:     devicePairURL,
		Code:        code,
		AgentPubkey: agentPubkeyB64,
		ExpiresAt:   expiresAt,
		TenantID:    tenantID,
	}
	qrJSON, err := json.Marshal(qr)
	if err != nil {
		return fmt.Errorf("marshal QR payload: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Scan this QR code with the cmux app (code expires %s):\n\n", expiresAt)
	qrterminal.GenerateHalfBlock(string(qrJSON), qrterminal.L, out)
	_, _ = fmt.Fprintf(out, "\nOr enter this code manually: %s\n\n", code)

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for pairing code %s to be redeemed", code)
		}
		devicePubkeyB64, tokenHash, redeemed, err := pollPairingCode(client, agentBase, code)
		if err != nil {
			// Transient relay errors (network blip, brief 5xx) shouldn't abort
			// the whole pairing attempt -- keep polling until the deadline.
			_, _ = fmt.Fprintf(out, "poll error (will retry): %v\n", err)
			time.Sleep(pollPeriod)
			continue
		}
		if !redeemed {
			time.Sleep(pollPeriod)
			continue
		}
		devicePubkeyRaw, err := base64.StdEncoding.DecodeString(devicePubkeyB64)
		if err != nil {
			return fmt.Errorf("decode device pubkey: %w", err)
		}
		devicePub, err := ecdh.X25519().NewPublicKey(devicePubkeyRaw)
		if err != nil {
			return fmt.Errorf("parse device pubkey: %w", err)
		}
		secret, err := e2e.DeriveSharedSecret(identity.Priv, devicePub)
		if err != nil {
			return fmt.Errorf("derive shared secret: %w", err)
		}

		// The redeemed device_pubkey above came back through the same
		// relay-mediated poll on every pairing, QR or manual -- the relay
		// operator could have fabricated it (see
		// docs/superpowers/specs/2026-07-10-pairing-mitm-fingerprint-design.md).
		// A human comparing this fingerprint against the phone's own
		// confirmation screen is the only channel the relay can't forge.
		fingerprint := e2e.PairingFingerprint(identity.PublicKey().Bytes(), devicePub.Bytes())
		_, _ = fmt.Fprintf(out, "\nVerify this code matches the phone's confirmation screen: %s\n", fingerprint)
		confirmed, err := confirmFingerprint(in, out)
		if err != nil {
			return fmt.Errorf("read confirmation: %w", err)
		}
		if !confirmed {
			return abandonPairing(client, agentBase, code,
				fmt.Sprintf("pairing aborted: fingerprint not confirmed for code %s", code), out)
		}

		if err := sessions.AddDevice(tokenHash, devicePub, secret); err != nil {
			// Half a pairing is worse than none: the phone would keep a token
			// that authenticates but decrypts nothing.
			return abandonPairing(client, agentBase, code,
				fmt.Sprintf("persist paired device: %v", err), out)
		}
		// Last, because it is what the phone is waiting on: anything that
		// fails before here has to reach the phone as a refusal rather than
		// leave it waiting out its own timeout.
		if err := confirmPairing(client, agentBase, code); err != nil {
			return abandonPairing(client, agentBase, code,
				fmt.Sprintf("confirm pairing (the phone will report it as refused): %v", err), out)
		}
		_, _ = fmt.Fprintf(out, "Device paired successfully.\n")
		return nil
	}
}

// confirmFingerprint prompts on out and reads one line from in. Only an
// explicit "y"/"yes" (case-insensitive) counts as confirmed -- EOF, a
// blank line, or anything else fails closed (not confirmed, no error),
// since an aborted pairing is ordinary control flow here, not exceptional.
func confirmFingerprint(in io.Reader, out io.Writer) (bool, error) {
	_, _ = fmt.Fprint(out, "Confirm? [y/N]: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return false, nil
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func runPairDevice(args []string) int {
	fs := flag.NewFlagSet("pair-device", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultAgentConfigPath(), "path to agent.toml")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the phone to scan and redeem the code")
	direct := fs.Bool("direct", false, "pair against this Mac's own direct (Tailscale) listener instead of the relay")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.LoadAgent(*cfgPath)
	if err != nil {
		slog.Error("pair-device: load config", "err", err)
		return 1
	}

	var srv agentServer
	if *direct {
		srv, err = directServer(cfg, pairingRequestTimeout)
	} else {
		srv, err = relayServer(cfg, pairingRequestTimeout)
	}
	if err != nil {
		slog.Error("pair-device: server", "direct", *direct, "err", err)
		return 1
	}
	agentBase, client := srv.baseURL, srv.client
	// /devices/pair is public on the same vhost as the agent-facing
	// pairing-code endpoints in both modes.
	devicePairURL := agentBase + "/devices/pair"

	identity, err := e2e.LoadOrCreateIdentity(cfg.IdentityKey)
	if err != nil {
		slog.Error("pair-device: e2e identity", "err", err)
		return 1
	}
	sessions, err := e2e.OpenStore(cfg.SessionStore)
	if err != nil {
		slog.Error("pair-device: open session store", "err", err)
		return 1
	}

	if err := pairDevice(client, agentBase, devicePairURL, identity, sessions, os.Stdout, os.Stdin, 2*time.Second, time.Now().Add(*timeout)); err != nil {
		slog.Error("pair-device: pair", "err", err)
		return 1
	}
	return 0
}
