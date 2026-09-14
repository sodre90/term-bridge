package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sodre90/term-bridge/internal/config"
)

// agentServer is one server this Mac's own CLI can talk to over the
// agent-facing routes: the relay, or this Mac's direct (Tailscale) listener.
// Both serve identical routes; they differ only in base URL and in how the
// client proves it is the agent.
//
// Extracted from runPairDevice rather than copied for the devices commands:
// a second, independently drifting copy of the relay-vs-direct client setup
// is exactly the kind of duplication that goes stale on one side only.
type agentServer struct {
	// kind is "relay" or "direct" -- operator-facing, so a device listing
	// can say which server a row came from.
	kind    string
	baseURL string
	client  *http.Client
}

// relayServer dials the relay over the agent's mTLS client certificate.
func relayServer(cfg config.AgentConfig, timeout time.Duration) (agentServer, error) {
	if cfg.RelayURL == "" {
		return agentServer{}, fmt.Errorf("relay_url is not set")
	}
	base, err := httpsBaseFromRelayURL(cfg.RelayURL)
	if err != nil {
		return agentServer{}, err
	}
	tlsCfg, err := loadTLS(cfg.ClientCert, cfg.ClientKey, cfg.CACert)
	if err != nil {
		return agentServer{}, fmt.Errorf("tls: %w", err)
	}
	return agentServer{
		kind:    "relay",
		baseURL: base,
		client:  &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}, Timeout: timeout},
	}, nil
}

// directServer dials this Mac's own direct listener by its Tailscale DNS
// name. The listener's cert is a real, publicly-trusted Let's Encrypt cert
// (tailscale cert), so the default transport's system roots already validate
// it and no client cert is involved at all.
func directServer(cfg config.AgentConfig, timeout time.Duration) (agentServer, error) {
	if cfg.DirectListen == "" {
		return agentServer{}, fmt.Errorf("direct_listen is not set")
	}
	st, err := tailscaleSelfStatus(context.Background())
	if err != nil {
		return agentServer{}, fmt.Errorf("tailscale status: %w", err)
	}
	if st.DNSName == "" {
		return agentServer{}, fmt.Errorf("this Mac has no Tailscale DNS name yet -- is Tailscale up?")
	}
	return agentServer{
		kind:    "direct",
		baseURL: "https://" + strings.TrimSuffix(st.DNSName, ".") + cfg.DirectListen,
		client:  &http.Client{Timeout: timeout},
	}, nil
}

// configuredServers returns every server this agent is set up to reach, in
// the order a device listing should show them. Used by the devices commands,
// which act on whichever server happens to hold a given device rather than
// making the operator know which slot it was paired through.
func configuredServers(cfg config.AgentConfig, timeout time.Duration) ([]agentServer, error) {
	var out []agentServer
	var problems []string
	if cfg.RelayURL != "" {
		srv, err := relayServer(cfg, timeout)
		if err != nil {
			problems = append(problems, "relay: "+err.Error())
		} else {
			out = append(out, srv)
		}
	}
	if cfg.DirectListen != "" {
		srv, err := directServer(cfg, timeout)
		if err != nil {
			problems = append(problems, "direct: "+err.Error())
		} else {
			out = append(out, srv)
		}
	}
	if len(out) == 0 {
		if len(problems) > 0 {
			return nil, fmt.Errorf("no reachable server: %s", strings.Join(problems, "; "))
		}
		return nil, fmt.Errorf("neither relay_url nor direct_listen is configured")
	}
	return out, nil
}
