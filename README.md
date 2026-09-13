# cmux remote

Reach your Mac's [cmux](https://github.com/manaflow-ai/cmux) agent sessions from
your phone — list workspaces, drive a live terminal, and answer agent prompts
from anywhere, **without opening a single inbound port on the Mac**.

The Mac *dials out* to a small relay on your home server; your phone talks to the
relay behind an mTLS edge. Everything speaks to cmux **only through its
documented CLI** (`cmux rpc` / `cmux events`) — no cmux source is copied and no
cmux socket password is stored.

```
    ┌────────────────────────────┐
    │        Android app         │
    │         (Compose)          │
    └────────────────────────────┘
                   │  HTTPS / WSS — bearer token + e2e encryption
                   ▼
    ┌────────────────────────────┐
    │  nginx (mutual TLS edge)   │
    │      public DNS name       │
    └────────────────────────────┘
                   │  HTTP / WS — loopback only
                   ▼
    ┌────────────────────────────┐
    │         cmux-relay         │
    │       (home server)        │
    └────────────────────────────┘
                   │  persistent yamux-over-WSS tunnel
                   ▲  (the Mac dials OUT — agent:<tenant-id> client cert)
    ┌────────────────────────────┐
    │  cmux-bridge agent (Mac)   │
    └────────────────────────────┘
                   │  cmux rpc / cmux events
                   ▼
                   cmux.app  (unchanged)
```

When the Mac is offline the relay returns `503 {"error":"agent_offline"}`; when it
reconnects (automatic, backoff-capped), everything resumes.

## Components

| Directory | What it is | Stack | Guide |
|---|---|---|---|
| [`android/`](android/) | The phone client — sessions list, live terminal, agent inbox, optional push | Kotlin · Jetpack Compose · `com.sodre90.cmuxremote` | [android/README.md](android/README.md) |
| [`bridge/`](bridge/) | Two Go binaries: **`cmux-relay`** (home-server rendezvous, auth, push) and **`cmux-bridge agent`** (runs on the Mac, dials the relay) | Go 1.26 | [bridge/README.md](bridge/README.md) |

Both bridge binaries live in `bridge/cmd/`; deployment templates (systemd unit,
podman quadlet, launchd plist, nginx vhosts, container files, example configs)
are in `bridge/deploy/`.

## How it fits together

1. **`cmux-relay`** runs on your home server behind nginx with `ssl_verify_client
   optional` (the agent presents a client cert; devices don't). It's its own
   certificate authority — it mints and signs every agent and device cert
   itself — and serves many independent tenants (Mac agents) at
   once: it owns device pairing/tokens, routes requests by client-cert CN, and
   (optionally) sends FCM push. It binds loopback only — nginx is the sole public
   surface.
2. **`cmux-bridge agent`** runs in your Mac's GUI login session next to cmux.
   The first time it runs it self-registers with the relay (see
   [bridge/README.md → Agent client certificate](bridge/README.md#agent-client-certificate))
   to get its own signed cert and tenant ID; from then on it opens **one
   outbound WSS tunnel** to the relay and serves the bridge HTTP/WS API over
   it. No port-forwarding, no inbound exposure on the Mac.
3. **The Android app** connects to the relay's public DNS name, authenticating
   with a per-device bearer token minted at pairing, and renders cmux's
   `render_grid` cell grid live. Once paired, every request/response body and
   terminal frame is also end-to-end encrypted between the phone and the Mac
   agent (X25519 + HKDF, derived during pairing) — the relay operator can
   route traffic but not read it.

## Quick start

Set it up in this order — each step links to the detailed guide:

1. **Relay + nginx edge** on the home server → [bridge/README.md → Relay](bridge/README.md#relay-home-server)
2. **Agent** on the Mac (LaunchAgent, dials the relay) → [bridge/README.md → Agent](bridge/README.md#agent-mac)
3. **Pair the phone** (self-service: scan a QR code, or enter the server URL + code manually) → [bridge/README.md → Pair a device](bridge/README.md#pair-a-device)
4. **Install the app** and complete pairing on the Pairing screen → [android/README.md → First-run setup](android/README.md#first-run-setup-pairing-screen)
5. **Push notifications** (optional, Firebase) → both READMEs' *Push* sections

### Build

```bash
# Bridge (Go 1.26+): from bridge/
go build -o cmux-relay  ./cmd/cmux-relay     # home server
go build -o cmux-bridge ./cmd/cmux-bridge    # Mac (agent mode)
go test ./...                                # no network, no real cmux

# Android app: from android/
./gradlew :app:assembleDebug                 # debug APK
./gradlew :app:testDebugUnitTest             # JVM unit tests
```

## Security model

Defense in depth, all the way to the cmux socket:

- **Per-tenant isolation** — the relay serves many independent Mac agents at
  once; each gets its own client cert (`CN=agent:<tenant-id>`) and its own
  tunnel slot, and a device's bearer token is scoped to exactly one tenant.
  A bug in one tenant's traffic can't spill into another's — enforced by an
  adversarial test (`internal/relay/multitenant_test.go`), not just by
  convention.
- **Mutual TLS at the nginx edge for the Mac agent** — the agent presents a
  client certificate signed by the relay's own CA (`CN=agent:<tenant-id>`),
  and only a request nginx independently verified against that cert may open
  or use that tenant's tunnel. Devices don't have a client certificate at all
  (nginx's `ssl_verify_client` is `optional`) — they authenticate with a
  bearer token instead (below).
- **Per-device bearer token** — minted at self-service pairing (see [Pair a
  device](bridge/README.md#pair-a-device)), revocable, sent as
  `Authorization: Bearer …` and resolved to a tenant on every request.
- **End-to-end encryption between phone and Mac agent** — pairing also derives
  a shared secret via X25519 ECDH + HKDF; every HTTP body and terminal
  WebSocket frame after that is AEAD-encrypted with a replay-protected
  counter, so the relay (and anyone who compromises the relay host) can route
  traffic by tenant but never read its contents.
- **Fingerprint confirmation at pairing** — the phone and the Mac each display
  a short fingerprint of the exchanged public keys, and pairing only completes
  once a human confirms they match. Without this the relay, which brokers the
  key exchange, could substitute its own key and read everything.
- **`X-Relay-Token` shared secret** — injected by the relay so the agent only
  honors relay-originated requests.
- The relay binds loopback only; **the Mac agent has no listening port at all.**

## What the app does

- **Sessions list** — workspaces and their terminal panes, normalized from cmux.
- **Live terminal** — renders cmux's `cmux.render-grid.v1` grid with styles,
  colors, cursor, and scrollback; fit-to-width sizing, pinch-to-zoom, a word-wrap
  toggle, text selection, a compact `←↑↓→` D-pad, an Enter key, and DECCKM-aware
  cursor keys. Input/paste/resize go upstream; replay + live output come down.
- **Agent inbox** — answer blocking prompts (permission requests and questions)
  via `POST /feed/{id}/reply`. Plan-approval (`exitPlan`) prompts aren't wired
  into the Inbox yet — their reply schema isn't confirmed live.
- **Rename a workspace** — long-press a workspace on the phone to set its
  persistent display title in cmux, via `POST /sessions/{id}/rename`.
- **Open, split, show and close** — create a workspace in a directory under
  your home (`POST /sessions`), split the pane you are viewing or add a tab
  to it (`POST /sessions/{id}/panes`, with a preview of where it lands), make
  the Mac show what the phone is looking at (`POST /sessions/{id}/select`),
  and close a pane or a whole workspace after a confirmation (`DELETE`).
  Nothing created from the phone takes focus on the Mac unless asked.
- **YOLO mode** — long-press a workspace to set a per-workspace auto-reply
  mode (Off/Always/All tools/Bypass) for permission prompts; the Mac agent
  replies on cmux's behalf with no phone round-trip, and the mode is shown as
  a badge on that workspace's row and in its terminal pane. `Bypass` mirrors
  Claude Code's own `--dangerously-skip-permissions`.
- **Custom sort order** — drag workspaces into any order via the handle on each
  row; purely a phone-local display preference, not synced anywhere.
- **Direct (Tailscale) mode** — an optional, additive alternative to the
  relay above: if the phone and Mac share a Tailscale tailnet, the app can
  talk straight to the Mac agent with no relay or home server involved. See
  [bridge/README.md → Direct (Tailscale) mode](bridge/README.md#direct-tailscale-mode).
- **Optional push** — FCM "an agent needs you" notifications, off by default and
  requiring no Firebase config to build.

The bridge performs read methods, terminal input/replay, feed replies
(including YOLO mode's automatic ones), workspace rename, and — since
2026-09-12 — workspace/pane create, select and close, each naming its target
by UUID. It never restores sessions. Creating a shell from the phone adds no
capability terminal input did not already give; closing is the one
destructive action and is confirmed on the phone first.

## Repository layout

```
android/              Jetpack Compose client (com.sodre90.cmuxremote)
bridge/               Go module: github.com/sodre90/cmux-bridge
  cmd/cmux-relay/       home-server rendezvous daemon
  cmd/cmux-bridge/      Mac agent (dials the relay)
  internal/             server, cmux CLI client, relay, tunnel, auth, push, …
  deploy/               systemd, quadlet, launchd, nginx, container, example configs
docs/                 design specs and implementation plans
articles/             longer-form write-ups of features and components
research/             exploratory notes, not authoritative
.beads/               bd issue tracker state
THIRD_PARTY_LICENSES/ bundled-asset licenses (e.g. JetBrains Mono)
CHANGELOG.md          notable changes since the repo went public
CLAUDE.md, AGENTS.md  operating instructions for coding agents
```

## Relationship to cmux & licensing

This project is an **independent work** that communicates with
[cmux](https://github.com/manaflow-ai/cmux) over its documented IPC/CLI contract.
It contains no cmux source. cmux is GPLv3; consuming its documented protocol over
IPC does not make this a derivative work.
