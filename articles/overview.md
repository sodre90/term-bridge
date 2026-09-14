---
type: article
description: What cmux-app is, how its three components fit together, and its security model — the top-level entry point for the knowledge base.
status: canonical
authored: 2026-07-21
author: sodre90
tags:
  - article
  - canonical
  - overview
---
## Summary

cmux-app is a self-hosted system that lets a phone remote-control [cmux](https://github.com/manaflow-ai/cmux) agent sessions running on a Mac: list workspaces, drive a live terminal, and answer agent prompts from anywhere, without opening any inbound port on the Mac. It has three parts — an Android client, a home-server relay daemon, and a Mac-side bridge agent — connected by a mutual-TLS edge and end-to-end encryption between the phone and the Mac.

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
          │  persistent yamux-over-WSS tunnel
          ▲  (the Mac dials OUT — agent:<tenant-id> client cert)
    term-bridge agent (Mac)
          │  cmux rpc / cmux events
          ▼
          cmux.app (unchanged)
```

When the Mac is offline the relay returns `503 {"error":"agent_offline"}`; when it reconnects (automatic, backoff-capped), everything resumes.

1. **`term-bridge-relay`** runs on the home server behind nginx with `ssl_verify_client on`. It is its own certificate authority — it mints and signs every agent and device cert itself — and serves many independent tenants (Mac agents) at once: it owns device pairing/tokens, routes requests by client-cert CN, and optionally sends FCM push. It binds loopback only; nginx is the sole public surface.
2. **`term-bridge agent`** runs in the Mac's GUI login session next to cmux. The first time it runs it self-registers with the relay to get its own signed cert and tenant ID; from then on it opens one outbound WSS tunnel to the relay and serves the bridge HTTP/WS API over it. No port-forwarding, no inbound exposure on the Mac.
3. **The Android app** connects to the relay's public DNS name, authenticating with a per-device bearer token minted at pairing, and renders cmux's `render_grid` cell grid live. Once paired, every request/response body and terminal frame is also end-to-end encrypted between the phone and the Mac agent (X25519 + HKDF, derived during pairing) — the relay operator can route traffic but not read it.

### Components

| Directory | What it is | Stack | Details |
|---|---|---|---|
| `android/` | The phone client — sessions list, live terminal, agent inbox, optional push | Kotlin · Jetpack Compose · `com.sodre90.cmuxremote` | [android-app](./components/android-app.md) |
| `bridge/` | Two Go binaries: `term-bridge-relay` (home-server rendezvous, auth, push) and `term-bridge agent` (runs on the Mac, dials the relay) | Go 1.26 | [bridge](./components/bridge.md) |

Both bridge binaries live in `bridge/cmd/`; deployment templates (systemd unit, launchd plist, nginx vhosts, container files, example configs) are in `bridge/deploy/`.

### Quick start order

1. Relay + nginx edge on the home server
2. Agent on the Mac (LaunchAgent, dials the relay)
3. Pair the phone (scan a QR code, or enter the server URL + code manually)
4. Install the app and complete pairing on the Pairing screen
5. Push notifications (optional, Firebase)

```bash
# Bridge (Go 1.26+): from bridge/
go build -o term-bridge-relay  ./cmd/term-bridge-relay     # home server
go build -o term-bridge ./cmd/term-bridge    # Mac (agent mode)
go test ./...                                # no network, no real cmux

# Android app: from android/
./gradlew :app:assembleDebug                 # debug APK
./gradlew :app:testDebugUnitTest             # JVM unit tests
```

### Security model

Defense in depth, all the way to the cmux socket:

- **Per-tenant isolation** — the relay serves many independent Mac agents at once; each gets its own client cert (`CN=agent:<tenant-id>`) and its own tunnel slot, and a device's bearer token is scoped to exactly one tenant. Enforced by an adversarial test (`internal/relay/multitenant_test.go`), not just by convention.
- **Mutual TLS at the nginx edge for the Mac agent** — the agent presents a client certificate signed by the relay's own CA; only a request nginx independently verified against that cert may open or use that tenant's tunnel. Devices don't have a client certificate at all — they authenticate with a bearer token instead.
- **Per-device bearer token** — minted at self-service pairing, revocable, sent as `Authorization: Bearer …` and resolved to a tenant on every request.
- **End-to-end encryption between phone and Mac agent** — pairing derives a shared secret via X25519 ECDH + HKDF; every HTTP body and terminal WebSocket frame after that is AEAD-encrypted with a replay-protected counter, so the relay (and anyone who compromises the relay host) can route traffic by tenant but never read its contents. See [pairing-e2e-encryption](./features/pairing-e2e-encryption.md).
- **`X-Relay-Token` shared secret** — injected by the relay so the agent only honors relay-originated requests.
- The relay binds loopback only; the Mac agent has no listening port at all.

### What the app does

- **Sessions list** — workspaces and their terminal panes, normalized from cmux.
- **Live terminal** — renders cmux's `cmux.render-grid.v1` grid with styles, colors, cursor, and scrollback; fit-to-width sizing, pinch-to-zoom, a word-wrap toggle, text selection, a compact D-pad, an Enter key, and DECCKM-aware cursor keys.
- **Agent inbox** — answer blocking prompts (permission requests, plan approvals, questions) via `POST /feed/{id}/reply`.
- **Rename a workspace** — long-press a workspace on the phone to set its persistent display title in cmux.
- **YOLO mode** — long-press a workspace to set a per-workspace auto-reply mode (Off/Always/All tools/Bypass) for permission prompts; the Mac agent replies on cmux's behalf with no phone round-trip. `Bypass` mirrors Claude Code's own `--dangerously-skip-permissions`.
- **Custom sort order** — drag workspaces into any order; a phone-local display preference only.
- **Direct (Tailscale) mode** — an optional, additive alternative to the relay: if the phone and Mac share a Tailscale tailnet, the app can talk straight to the Mac agent with no relay involved. See [connectivity-tailscale-dual-pairing](./features/connectivity-tailscale-dual-pairing.md).
- **Optional push** — FCM "an agent needs you" notifications, off by default and requiring no Firebase config to build. See [push-notifications](./features/push-notifications.md).

The bridge performs **only** read methods, terminal input/replay, feed replies (including YOLO mode's automatic ones), and workspace rename — it never creates, closes, or restores workspaces or terminals.

### Repository layout

```
android/              Jetpack Compose client (com.sodre90.cmuxremote)
bridge/               Go module: github.com/sodre90/term-bridge
  cmd/term-bridge-relay/       home-server rendezvous daemon
  cmd/term-bridge/      Mac agent (dials the relay)
  internal/             server, cmux CLI client, relay, tunnel, auth, push, …
  deploy/               systemd, launchd, nginx, container, example configs
docs/                 design specs and implementation plans
THIRD_PARTY_LICENSES/ bundled-asset licenses (e.g. JetBrains Mono)
```

### Relationship to cmux & licensing

This project is an independent work that communicates with [cmux](https://github.com/manaflow-ai/cmux) over its documented IPC/CLI contract. It contains no cmux source. cmux is GPLv3; consuming its documented protocol over IPC does not make this a derivative work.

### Further reading

- [enhancement-audit](./enhancement-audit.md) — point-in-time code-quality/security audit of the shipped bridge and Android code.
- [roadmap](./roadmap.md) — phased improvement guide (CI, structural cleanup, operability, UX, polish).
- [research/improvement-ideas](../research/improvement-ideas.md) — provisional backlog of improvement ideas that fed into the roadmap.

## References

- [README.md](../README.md)
