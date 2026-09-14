---
type: article
description: What Term Bridge is, how its three components fit together across cmux (Mac) and tmux (Linux) hosts, and its security model — the top-level entry point for the knowledge base.
status: canonical
authored: 2026-07-21
author: sodre90
tags:
  - article
  - canonical
  - overview
---
## Summary

Term Bridge (formerly cmux-app) is a self-hosted system that lets a phone remote-control the terminal agent sessions on your machines — [cmux](https://github.com/manaflow-ai/cmux) on a Mac, [tmux](https://github.com/tmux/tmux) on a headless Linux box, or both: list workspaces, drive a live terminal, and answer agent prompts from anywhere, without opening any inbound port on any host. It has three parts — an Android client that pairs with several hosts and switches between them, a home-server relay daemon, and a bridge agent on each host that fronts cmux or tmux behind one `Host` interface — connected by a mutual-TLS edge and end-to-end encryption between the phone and each host.

## Body

### How it fits together

```
    Android app (Compose)
          │  HTTPS / WSS — bearer token + e2e encryption
          ▼
    nginx (mutual TLS edge, public DNS name)
          │  HTTP / WS — loopback only
          ▼
    term-bridge-relay (home server)
      ▲                        ▲   persistent yamux-over-WSS tunnels
      │                        │   (each agent dials OUT — agent:<tenant-id> client cert)
    term-bridge agent (Mac)   term-bridge agent (Linux)
      │ cmux rpc / events      │ tmux CLI / control mode
      ▼                        ▼
    cmux.app (unchanged)      tmux server (unchanged)
```

When a host is offline the relay returns `503 {"error":"agent_offline"}` for it; when it reconnects (automatic, backoff-capped), everything resumes. Other hosts are unaffected.

1. **`term-bridge-relay`** runs on the home server behind nginx with `ssl_verify_client optional`. It is its own certificate authority — it mints and signs every agent and device cert itself — and serves many independent tenants (one per host) at once: it owns device pairing/tokens, routes requests by client-cert CN, and optionally sends FCM push. It binds loopback only; nginx is the sole public surface.
2. **`term-bridge agent`** runs on each host as the user whose sessions it serves: in the Mac's GUI login session next to cmux, or as a systemd user unit on the Linux box next to the tmux server. The first time it runs it self-registers with the relay to get its own signed cert and tenant ID; from then on it opens one outbound WSS tunnel to the relay and serves the bridge HTTP/WS API over it. No port-forwarding, no inbound exposure anywhere. `host = "cmux"` or `"tmux"` in `agent.toml` picks the backend; the API is the same, plus a `host` block naming the machine, its kind and its capabilities.
3. **The Android app** pairs with each host separately and keeps credentials, keys and workspace order per host (keyed by the agent's identity key). It renders the host's cell grid live — cmux's `render_grid`, or tmux's `capture-pane` parsed into the same grid. Once paired, every request/response body and terminal frame is also end-to-end encrypted between the phone and that host's agent (X25519 + HKDF, derived during pairing) — the relay operator can route traffic but not read it.

### Components

| Directory | What it is | Stack | Details |
|---|---|---|---|
| `android/` | The phone client — host switcher, sessions list, live terminal, agent inbox, optional push | Kotlin · Jetpack Compose · `com.sodre90.cmuxremote` | [android-app](./components/android-app.md) |
| `bridge/` | Two Go binaries: `term-bridge-relay` (home-server rendezvous, auth, push) and `term-bridge agent` (runs on each host, dials the relay; `cmuxhost` / `tmuxhost` behind one `Host` interface) | Go 1.26 | [bridge](./components/bridge.md) |

Both bridge binaries live in `bridge/cmd/`; deployment templates (systemd units, quadlet, launchd plist, nginx vhosts, container file, example configs for Mac and Linux) are in `bridge/deploy/`.

### Quick start order

1. Relay + nginx edge on the home server
2. Agent on each host: Mac (LaunchAgent) and/or Linux + tmux (systemd user unit); both dial the relay
3. Pair the phone with each host (scan a QR code, or enter the server URL + code manually)
4. Install the app and complete pairing on the Pairing screen
5. Push notifications (optional, Firebase)

```bash
# Bridge (Go 1.26+): from bridge/
go build -o term-bridge-relay ./cmd/term-bridge-relay   # home server
go build -o term-bridge       ./cmd/term-bridge         # agent (Mac or Linux; GOOS=linux to cross-build)
go test ./...                                           # no network, no real cmux or tmux

# Android app: from android/
./gradlew :app:assembleDebug                 # debug APK
./gradlew :app:testDebugUnitTest             # JVM unit tests
```

### Security model

Defense in depth, all the way to the host's own socket:

- **Per-tenant isolation** — the relay serves many independent hosts at once; each gets its own client cert (`CN=agent:<tenant-id>`) and its own tunnel slot, and a device's bearer token is scoped to exactly one tenant. Enforced by an adversarial test (`internal/relay/multitenant_test.go`), not just by convention. Two of your own hosts are two tenants: the phone holds one credential set and e2e session per host, and a push meant for one host cannot be decrypted with another's keys.
- **Mutual TLS at the nginx edge for every agent** — the agent presents a client certificate signed by the relay's own CA; only a request nginx independently verified against that cert may open or use that tenant's tunnel. Devices don't have a client certificate at all — they authenticate with a bearer token instead.
- **Per-device bearer token** — minted at self-service pairing, revocable, sent as `Authorization: Bearer …` and resolved to a tenant on every request.
- **End-to-end encryption between phone and agent** — pairing derives a shared secret via X25519 ECDH + HKDF; every HTTP body and terminal WebSocket frame after that is AEAD-encrypted with a replay-protected counter, so the relay (and anyone who compromises the relay host) can route traffic by tenant but never read its contents. See [pairing-e2e-encryption](./features/pairing-e2e-encryption.md).
- **`X-Relay-Token` shared secret** — injected by the relay so the agent only honors relay-originated requests.
- The relay binds loopback only; no agent has a listening port at all (the optional Tailscale direct listener on a Mac is bound to the tailnet address only).
- On Linux the agent, the tmux server and the agents inside it run as the same Unix user; the bridge drives tmux through its CLI exactly as that user could from a shell, and `send-keys` is the same capability terminal input already grants.

### What the app does

- **Host switcher** — the sessions title is the host you're looking at; tap it to switch or to pair another. Names are learned from the host (`hostname`), with a `cmux`/`tmux` badge; each host keeps its own pairings, workspace order and connection status.
- **Sessions list** — workspaces and their terminal panes: cmux workspaces/surfaces on a Mac, tmux windows/panes (across every session) on Linux.
- **Live terminal** — renders the host's cell grid (`cmux.render-grid.v1` shape) with styles, colors, cursor, and scrollback; fit-to-width sizing, pinch-to-zoom, a word-wrap toggle, text selection, a compact D-pad, an Enter key, and DECCKM-aware cursor keys.
- **Agent inbox** — answer blocking prompts (permission requests, questions) via `POST /feed/{id}/reply`. On cmux the feed is cmux's own; on tmux it is Claude Code's hooks (`term-bridge hook install`): the prompt stays Claude Code's in the terminal, the phone mirrors it, and a reply types the matching option's digit into the pane, refused with `409 prompt_gone` if the prompt is no longer on screen ([linux-tmux-host design](../docs/superpowers/specs/2026-09-14-linux-tmux-host-design.md)).
- **Rename a workspace** — long-press a workspace on the phone to set its persistent display title (cmux workspace title / tmux window name).
- **Open, split, show and close** — create a workspace under `$HOME`, split the pane you are viewing (with a placement preview; "add as tab" only where the host has tabs — tmux does not), show the workspace on the host, and close a pane or workspace after confirmation.
- **YOLO mode** — long-press a workspace to set a per-workspace auto-reply mode (Off/Always/All tools/Bypass) for permission prompts; the agent replies on the host's behalf with no phone round-trip. `Bypass` mirrors Claude Code's own `--dangerously-skip-permissions`. cmux hosts only for now.
- **Custom sort order** — drag workspaces into any order; a phone-local, per-host display preference only.
- **Direct (Tailscale) mode** — an optional, additive alternative to the relay: if the phone and a Mac share a Tailscale tailnet, the app can talk straight to that Mac's agent with no relay involved. See [connectivity-tailscale-dual-pairing](./features/connectivity-tailscale-dual-pairing.md).
- **Optional push** — FCM "an agent needs you" notifications, off by default and requiring no Firebase config to build; one phone token is registered with every paired host and a notification opens the app on the host it came from. Attention pushes come from cmux hosts today. See [push-notifications](./features/push-notifications.md).

The bridge performs read methods, terminal input/replay, feed replies (including YOLO mode's automatic ones), workspace rename, and workspace/pane create, select and close, each naming its target by id (a cmux UUID or a tmux `$n`/`%n`) — it never restores workspaces or terminals, and closing is confirmed on the phone first.

### Repository layout

```
android/              Jetpack Compose client (com.sodre90.cmuxremote)
bridge/               Go module: github.com/sodre90/term-bridge
  cmd/term-bridge-relay/   home-server rendezvous daemon
  cmd/term-bridge/         host agent (dials the relay)
  internal/host/           the Host interface, cmuxhost (Mac) and tmuxhost (Linux)
  internal/                server, cmux CLI client, tmux client, relay, tunnel, auth, push, …
  deploy/                  systemd, quadlet, launchd, nginx, container, example configs
docs/                 design specs and implementation plans
THIRD_PARTY_LICENSES/ bundled-asset licenses (e.g. JetBrains Mono)
```

### Relationship to cmux, tmux & licensing

This project is an independent work that communicates with [cmux](https://github.com/manaflow-ai/cmux) over its documented IPC/CLI contract and with [tmux](https://github.com/tmux/tmux) over its command-line interface and control mode. It contains no cmux or tmux source. cmux is GPLv3 and tmux is ISC-licensed; consuming a documented protocol over IPC does not make this a derivative work of either.

### Further reading

- [enhancement-audit](./enhancement-audit.md) — point-in-time code-quality/security audit of the shipped bridge and Android code.
- [roadmap](./roadmap.md) — phased improvement guide (CI, structural cleanup, operability, UX, polish).
- [research/improvement-ideas](../research/improvement-ideas.md) — provisional backlog of improvement ideas that fed into the roadmap.

## References

- [README.md](../README.md)
