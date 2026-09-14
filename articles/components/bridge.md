---
type: article
description: "term-bridge component reference: the term-bridge-relay and term-bridge agent Go binaries — architecture, the Host seam (cmux on a Mac, tmux on Linux), build, deployment, pairing, and API."
status: canonical
authored: 2026-07-21
author: sodre90
tags:
  - article
  - canonical
  - bridge
  - component
---
## Summary

`term-bridge` is two Go binaries that give a phone remote access to the terminal sessions on your hosts — cmux on a Mac, tmux on a Linux box: `term-bridge-relay` (a rendezvous daemon on a home server, behind nginx mTLS) and `term-bridge agent` (runs on each host, dials out to the relay so no host needs inbound ports). The agent speaks to its backend only through the documented CLI (`cmux rpc` / `cmux events`; the `tmux` command and its control mode) — no socket password is stored, no cmux or tmux source is copied. The two backends sit behind one `Host` interface (`internal/host`: `cmuxhost`, `tmuxhost`), so the server, relay, auth, e2e and push code is shared.

## Body

### Architecture (v2 relay topology)

The relay multiplexes every app request as a fresh [yamux](https://github.com/hashicorp/yamux) stream over that tenant's single agent tunnel (a WebSocket, so it traverses nginx on 443). The agent serves its handler verbatim. When a host is not connected, the relay returns `503 {"error":"agent_offline"}` for that tenant only; every host is its own tenant.

Security is layered: mutual TLS at the nginx edge for the agent only (`ssl_verify_client optional` — the agent presents a client cert signed by the relay's own CA and is routed by its verified CN; devices have no client cert) + a per-device bearer token checked by the relay + end-to-end encryption between phone and agent (X25519 ECDH + HKDF derived at pairing, AEAD over every HTTP body and terminal WS frame, replay-protected) + an `X-Relay-Token` shared secret the relay injects so the agent only honors relay-originated requests. The relay binds loopback only; the agent has no listening port at all. See [pairing-e2e-encryption](../features/pairing-e2e-encryption.md) for the crypto detail and [bridge-relay-architecture](../features/bridge-relay-architecture.md) for the original design rationale.

### Build

Requires Go 1.26+.

```bash
cd bridge
go build -o term-bridge-relay  ./cmd/term-bridge-relay     # for the home server
go build -o term-bridge ./cmd/term-bridge    # for a host (agent mode; GOOS=linux GOARCH=amd64 to cross-build)
go test ./...        # all tests run with no network and no real cmux
```

### Relay (home server)

1. Copy the binary to `/usr/local/bin/term-bridge-relay`.
2. Copy `deploy/relay.example.toml` to `/etc/term-bridge-relay/config.toml` and set `relay_token` plus optionally the FCM fields. On first run the relay generates its own CA (`ca_cert`/`ca_key`) and signs every agent and device cert against it. Migrating an existing deployment with its own CA (RSA or ECDSA)? Point `ca_cert`/`ca_key` at those files and the relay reuses it.
3. Install the systemd unit and nginx vhost (`deploy/term-bridge-relay.service`, `deploy/nginx-term-bridge-relay.conf`). If a new agent will self-register, also install the no-mTLS bootstrap vhost (`deploy/nginx-term-bridge-relay-bootstrap.conf`, proxies only `POST /tenants/register` on a separate port).

The relay binds `127.0.0.1:8765`; nginx is the only public surface. nginx must set `X-Client-Cert-CN $ssl_client_s_dn` (never trust an inbound value).

Can also run in a container (podman): `docker-compose.yml` + `deploy/Containerfile` build and run the relay rootless. `podman-compose up -d --build`, then verify with `curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8765/healthz` (expect `200`).

### Agent (Mac)

The agent must run in the GUI login session to reach the per-user cmux socket (`host = "cmux"`, the default).

1. Copy `deploy/agent.example.toml` to `~/.config/term-bridge/agent.toml`; set `relay_url` (`wss://<your-domain>/agent/tunnel`), client-cert paths, server CA, and the same `relay_token` as the relay.
2. Install the LaunchAgent (`deploy/com.sodre90.term-bridge.plist`, `launchctl bootstrap`/`kickstart`).

The agent reconnects automatically (exponential backoff, capped at 30s) if the relay or network drops.

### Agent (Linux, tmux)

The same binary fronts a Linux box with `host = "tmux"` in `agent.toml` (`deploy/agent.linux.example.toml`): every tmux window on the server is a workspace, every pane a terminal, ids are tmux's `$n`/`%n`. The agent, the tmux server and whatever runs inside it must be the same Unix user (tmux socket). It ships as a systemd user unit (`deploy/term-bridge-agent.service`, `loginctl enable-linger`), logs to journald, and dials the relay's public nginx name like any other host — it is a tenant of its own, so the phone pairs with it separately. Differences by design: no "add as tab" (`capabilities.tabs = false`), a viewed window is sized to the phone (`resize-window`) and handed back afterwards, and the Inbox/YOLO/attention pushes cover Claude Code panes only, through its hooks: `term-bridge hook install` adds `term-bridge hook` to `~/.claude/settings.json`, the hook forwards each event over `$XDG_RUNTIME_DIR/term-bridge/hooks.sock` (0600, no port) to `internal/host/agentfeed`, which mirrors the prompt as a feed item and answers a reply by re-reading `capture-pane` and typing the matching option's digit (`409 prompt_gone` when the prompt is gone; the hook itself never returns a decision) — see the [Linux host design](../../docs/superpowers/specs/2026-09-14-linux-tmux-host-design.md).

### Agent client certificate

No host needs a hand-rolled client cert:

1. `bootstrap_url` in `agent.toml` points at the relay's no-mTLS bootstrap vhost.
2. On first run — only while `client_cert` doesn't exist on disk yet — the agent generates a keypair, sends a CSR to `bootstrap_url`, and the relay mints a fresh tenant and signs the cert with CN `agent:<tenant-id>` against its own CA. The agent writes the cert/key and prints the assigned tenant ID.
3. Every run after that skips registration — the cert is already on disk.

Self-registration never touches `ca_cert` — that setting pins the CA that signed nginx's own *server* certificate, unrelated to the relay's internal agent/device-signing CA. Leave it empty for a publicly-trusted nginx server cert (e.g. Let's Encrypt).

### Pair a device

Pairing is self-service, no operator step, no hand-rolled `.p12` client certificate:

```bash
term-bridge pair-device --config ~/.config/term-bridge/agent.toml
```

This asks the relay for a fresh, single-use pairing code, then prints a QR code and the code itself. The QR payload carries a one-time pairing URL, the code, and the agent's public key. The app scans it, generates its own keypair, and calls the relay directly to redeem the code. `pair-device` polls in the background and, once the phone redeems the code, derives a shared secret with the device (X25519 + HKDF).

Because the device public key that redemption hands back came through the relay, a compromised relay could substitute its own key and silently MITM the session. So before saving anything, `pair-device` prints a short SAS fingerprint of both public keys and waits for the operator to confirm it matches the one the phone shows — only on `y`/`yes` does it derive and save the shared secret. See [pairing-e2e-encryption](../features/pairing-e2e-encryption.md) for the full threat model.

No camera handy? The Android app also has a manual-entry form (server URL + the printed code), resolving the agent's public key via the public, unauthenticated `GET /devices/pair-info/{code}` — same handshake, same e2e result, same fingerprint-confirmation step.

`pair-device` never displays a raw device token to the operator. Devices/tenants can be listed/revoked via `term-bridge-relay devices` / `term-bridge-relay tenants`. Revocation is checked live on every connect/request but does not forcibly close an already-connected agent's existing tunnel.

There is no manual-pairing fallback: a phone paired under the old `term-bridge-relay pair` flow loses relay access the moment self-service pairing ships and must be re-paired via `pair-device`.

### Direct (Tailscale) mode

An optional, additive alternative to the relay: if a Mac and the phone share a Tailscale tailnet, the phone can talk straight to that Mac's agent with no relay and no home server in the path. The relay keeps working exactly as before — this is a second listener, not a replacement, and push notifications still require the relay. See [connectivity-tailscale-dual-pairing](../features/connectivity-tailscale-dual-pairing.md) for the full design (MagicDNS + HTTPS cert issuance, `direct_listen` config, dual-pairing automatic fallback).

Switching between relay and direct mode in the original v1 shipped as a manual re-pair — no automatic fallback in that version (later addressed, see [connectivity-tailscale-dual-pairing](../features/connectivity-tailscale-dual-pairing.md)).

### Edge: nginx mutual TLS

See `deploy/nginx-term-bridge-relay.conf`. Point the home-server DNS name at nginx, accept an optional client certificate (`ssl_verify_client optional`), and `proxy_pass` to `http://127.0.0.1:8765`. The `map $http_upgrade $connection_upgrade` block (http context) is required for the agent tunnel and the terminal/event WebSockets.

### Push (optional)

1. Create a Firebase project and a service-account JSON key.
2. Put the key on the home server; set `fcm_project_id` + `fcm_credentials` in the relay config.
3. The app registers its FCM token via `POST /devices/register` with every host it is paired to. The relay opens its own `/events` subscription over each agent tunnel; when an agent raises a blocking prompt it sends a high-priority FCM data message to every device paired to that tenant. The payload carries no host id; the phone tries each paired host's e2e session and only the right one decrypts. Attention pushes come from cmux hosts today.

cmux redacts the actual prompt text in its event stream, so push triggers on the Claude Code hook name (`Notification` covers permission prompts and idle "waiting for input"; `AskUserQuestion` is an explicit blocking choice) rather than structured feed content. The notification body is enriched with the workspace's live title + status preview. Tapping the notification deep-links to that workspace's terminal. See [push-notifications](../features/push-notifications.md) for the later agent-native (direct-mode) push path.

### API

The app's base URL is the relay's public domain. All routes require `Authorization: Bearer <device-token>`. A `503 {"error":"agent_offline"}` means that host's agent is not currently connected to the relay. `GET /sessions` carries a `host` block (`name`, `kind`, `capabilities: {tabs, feed}`) the app gates its UI on.

| Method | Path | Purpose |
|---|---|---|
| GET  | `/sessions` | list workspaces/terminals (normalized) |
| GET  | `/events` (WS) | agent feed + notifications; `needs_attention` flags blocking prompts |
| GET  | `/terminal/{id}` (WS) | replay + live output (down); input/paste/resize (up) |
| GET  | `/feed/pending` | list pending blocking prompts (full question/option structure) |
| POST | `/feed/{id}/reply` | answer a prompt: `{kind, request_id, params}` |
| POST | `/sessions/{id}/rename` | set a workspace's title in cmux: `{title}` |
| POST | `/sessions/{id}/yolo-mode` | set a workspace's auto-reply mode: `{mode}` (`""` \| `always` \| `all` \| `bypass`) |
| POST | `/devices/register` | store this device's FCM token: `{fcm_token}` |
| POST | `/devices/pair` | redeem a pairing code (no bearer token yet): `{code, name, device_pubkey}` |
| GET  | `/devices/pair-info/{code}` | resolve a pairing code's agent pubkey for manual entry (no auth) |

Terminal frames carry cmux's `render_grid` (`format: "cmux.render-grid.v1"`) verbatim. `feed.*.reply`'s params beyond `request_id`: `feed.permission.reply` takes `mode: "once" | "always" | "all" | "bypass" | "deny"`; `feed.exit_plan.reply` takes `mode: "ultraplan" | "manual" | "autoAccept" | "bypassPermissions"`; `feed.question.reply` takes `selections: [string]`.

### Safety

The bridge calls only read methods, terminal input/replay, feed replies, workspace rename (cmux's own `workspace.rename` RPC), and YOLO mode's auto-replies to permission prompts. It never creates, closes, or restores workspaces/terminals. Tests use a fake `cmux` binary and never touch the real socket.

YOLO mode is an opt-in, per-workspace auto-reply for permission prompts, persisted locally on the agent (`~/.config/term-bridge/yolo.db`, keyed by workspace ID, never sent to cmux itself). `bypass` mirrors Claude Code's own `--dangerously-skip-permissions`. Correlating a pending item to a workspace is done by matching cwd, since cmux pending items key on the agent's own session ID, not the cmux workspace ID.

### Licensing

The bridge is an independent work that communicates with cmux over its IPC/CLI. It contains no cmux source. cmux is GPLv3; consuming its documented protocol over IPC does not make this a derivative work.

## References

- [bridge/README.md](../../bridge/README.md)
