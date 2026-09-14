# Term Bridge

Reach the terminal agents running on your machines from your phone — a Mac
running [cmux](https://github.com/manaflow-ai/cmux), a headless Linux box
running [tmux](https://github.com/tmux/tmux), or both. List workspaces, drive
a live terminal, and answer agent prompts from anywhere, **without opening a
single inbound port on any host**.

Each host *dials out* to a small relay on your home server; your phone talks to
the relay behind an mTLS edge and switches between hosts with one tap. Every
host is driven **only through its documented CLI** — `cmux rpc` / `cmux
events` on the Mac, the `tmux` command and its control mode on Linux — no host
source is copied and no socket password is stored.

```
    ┌────────────────────────────┐
    │        Android app         │
    │  (Compose · one or more    │
    │   paired hosts)            │
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
    │     term-bridge-relay      │
    │       (home server)        │
    └────────────────────────────┘
          ▲                   ▲     persistent yamux-over-WSS tunnels
          │                   │     (each agent dials OUT — agent:<tenant-id> client cert)
 ┌──────────────────┐  ┌──────────────────┐
 │ term-bridge agent│  │ term-bridge agent│
 │      (Mac)       │  │  (Linux, tmux)   │
 └──────────────────┘  └──────────────────┘
          │ cmux rpc / events      │ tmux CLI / control mode
          ▼                        ▼
      cmux.app                 tmux server
      (unchanged)              (unchanged)
```

When a host is offline the relay returns `503 {"error":"agent_offline"}` for
it; when it reconnects (automatic, backoff-capped), everything resumes. The
other hosts are unaffected.

## Components

| Directory | What it is | Stack | Guide |
|---|---|---|---|
| [`android/`](android/) | The phone client — host switcher, sessions list, live terminal, agent inbox, optional push | Kotlin · Jetpack Compose · `com.sodre90.cmuxremote` | [android/README.md](android/README.md) |
| [`bridge/`](bridge/) | Two Go binaries: **`term-bridge-relay`** (home-server rendezvous, auth, push) and **`term-bridge agent`** (runs on each host, dials the relay; fronts cmux or tmux behind one `Host` interface) | Go 1.26 | [bridge/README.md](bridge/README.md) |

Both bridge binaries live in `bridge/cmd/`; deployment templates (systemd
units, podman quadlet, launchd plist, nginx vhosts, container file, example
configs for Mac and Linux) are in `bridge/deploy/`.

## How it fits together

1. **`term-bridge-relay`** runs on your home server behind nginx with
   `ssl_verify_client optional` (agents present a client cert; devices
   don't). It's its own certificate authority — it mints and signs every
   agent and device cert itself — and serves many independent tenants (one
   per host) at once: it owns device pairing/tokens, routes requests by
   client-cert CN, and (optionally) sends FCM push. It binds loopback only —
   nginx is the sole public surface.
2. **`term-bridge agent`** runs on each host as the user whose sessions it
   serves: in the Mac's GUI login session next to cmux, or as a systemd user
   unit on the Linux box next to the tmux server. The first time it runs it
   self-registers with the relay (see [bridge/README.md → Agent client
   certificate](bridge/README.md#agent-client-certificate)) to get its own
   signed cert and tenant ID; from then on it opens **one outbound WSS
   tunnel** to the relay and serves the bridge HTTP/WS API over it. No
   port-forwarding, no inbound exposure anywhere. Which backend it fronts is
   one line in `agent.toml` (`host = "cmux"` or `"tmux"`); the API the phone
   sees is the same, plus a `host` block naming the machine, its kind and its
   capabilities.
3. **The Android app** pairs with each host separately (the relay treats
   them as unrelated tenants) and keeps one set of credentials, keys and
   workspace order per host. It renders the host's cell grid live — cmux's
   `render_grid`, or tmux's `capture-pane` output parsed into the same grid.
   Once paired, every request/response body and terminal frame is end-to-end
   encrypted between the phone and that host's agent (X25519 + HKDF, derived
   during pairing) — the relay operator can route traffic but not read it.

## Quick start

Set it up in this order — each step links to the detailed guide:

1. **Relay + nginx edge** on the home server → [bridge/README.md → Relay](bridge/README.md#relay-home-server)
2. **Agent** on each host:
   Mac (LaunchAgent) → [bridge/README.md → Agent (Mac)](bridge/README.md#agent-mac);
   Linux + tmux (systemd user unit) → [bridge/README.md → Agent (Linux, tmux)](bridge/README.md#agent-linux-tmux)
3. **Pair the phone** with each host (self-service: scan a QR code, or enter
   the server URL + code manually) → [bridge/README.md → Pair a device](bridge/README.md#pair-a-device)
4. **Install the app** and complete pairing on the Pairing screen; add more
   hosts later from the host menu → [android/README.md → First-run setup](android/README.md#first-run-setup-pairing-screen)
5. **Push notifications** (optional, Firebase) → both READMEs' *Push* sections

### Build

```bash
# Bridge (Go 1.26+): from bridge/
go build -o term-bridge-relay ./cmd/term-bridge-relay   # home server
go build -o term-bridge       ./cmd/term-bridge         # agent (Mac or Linux)
GOOS=linux GOARCH=amd64 go build -o term-bridge ./cmd/term-bridge   # cross-build for the Linux box
go test ./...                                           # no network, no real cmux or tmux

# Android app: from android/
./gradlew :app:assembleDebug                 # debug APK
./gradlew :app:testDebugUnitTest             # JVM unit tests
```

## Security model

Defense in depth, all the way to the host's own socket:

- **Per-tenant isolation** — the relay serves many independent hosts at
  once; each gets its own client cert (`CN=agent:<tenant-id>`) and its own
  tunnel slot, and a device's bearer token is scoped to exactly one tenant.
  A bug in one tenant's traffic can't spill into another's — enforced by an
  adversarial test (`internal/relay/multitenant_test.go`), not just by
  convention. Two of your own hosts are two tenants: the phone holds one
  credential set and one e2e session per host, and a push meant for one
  host cannot be decrypted with another's keys.
- **Mutual TLS at the nginx edge for every agent** — the agent presents a
  client certificate signed by the relay's own CA (`CN=agent:<tenant-id>`),
  and only a request nginx independently verified against that cert may open
  or use that tenant's tunnel. Devices don't have a client certificate at all
  (nginx's `ssl_verify_client` is `optional`) — they authenticate with a
  bearer token instead (below).
- **Per-device bearer token** — minted at self-service pairing (see [Pair a
  device](bridge/README.md#pair-a-device)), revocable, sent as
  `Authorization: Bearer …` and resolved to a tenant on every request.
- **End-to-end encryption between phone and agent** — pairing also derives
  a shared secret via X25519 ECDH + HKDF; every HTTP body and terminal
  WebSocket frame after that is AEAD-encrypted with a replay-protected
  counter, so the relay (and anyone who compromises the relay host) can route
  traffic by tenant but never read its contents.
- **Fingerprint confirmation at pairing** — the phone and the host each
  display a short fingerprint of the exchanged public keys, and pairing only
  completes once a human confirms they match. Without this the relay, which
  brokers the key exchange, could substitute its own key and read everything.
- **`X-Relay-Token` shared secret** — injected by the relay so the agent only
  honors relay-originated requests.
- The relay binds loopback only; **no agent has a listening port at all**
  (the optional Tailscale direct listener on a Mac is the one exception, and
  it is bound to the tailnet address only).
- On Linux the agent, the tmux server and the agents inside it run as the
  **same Unix user**; the bridge reaches tmux through its CLI exactly as that
  user could from a shell, and `send-keys` is the same capability terminal
  input already grants.
- **The Claude Code hooks socket is the Linux agent's one local IPC surface**:
  `$XDG_RUNTIME_DIR/term-bridge/hooks.sock`, in a `0700` directory, mode
  `0600`, reachable only by that user — no port. `term-bridge hook` is a
  command hook, not an `http` hook, precisely so nothing listens. What
  crosses it is what Claude Code already hands any hook (the gated tool's
  name and input, the prompt's questions); the agent never logs it.
- **Allow/deny authority stays with Claude Code.** The hook returns no
  decision, ever — it only records what is being asked. Approving from the
  phone types the digit of the option Claude Code drew, and the bridge
  re-reads the screen immediately before typing: no prompt on screen, no
  keystroke. Nothing in the bridge can approve a tool call that Claude Code
  is not at that moment asking about.

## What the app does

- **Host switcher** — the sessions title is the host you're looking at; tap
  it to switch or to pair another. Names are learned from the host itself
  (`hostname`), with a `cmux` / `tmux` badge. Each host keeps its own
  pairings, workspace order and connection status; switching recreates every
  screen against the new host.
- **Sessions list** — workspaces and their terminal panes: cmux workspaces
  and surfaces on a Mac; tmux windows and panes (across every session on the
  server) on Linux.
- **Live terminal** — renders the host's cell grid with styles, colors,
  cursor, and scrollback; fit-to-width sizing, pinch-to-zoom, a word-wrap
  toggle, text selection, a compact `←↑↓→` D-pad, an Enter key, and DECCKM-aware
  cursor keys. Input/paste/resize go upstream; replay + live output come down.
  On tmux, resizing sets the window size while the phone is viewing (and
  zooms the viewed pane when the window is split, so it gets the whole
  viewport) and releases both back to the attached client afterwards.
- **Agent inbox** — answer blocking prompts (permission requests and questions)
  via `POST /feed/{id}/reply`. Plan-approval (`exitPlan`) prompts aren't wired
  into the Inbox yet — their reply schema isn't confirmed live. On cmux the
  feed is cmux's own; on tmux it comes from Claude Code's hooks
  (`term-bridge hook install`, see [bridge/README.md → Claude Code
  hooks](bridge/README.md#claude-code-hooks-on-linux)): the prompt stays
  Claude Code's own in the terminal, the phone mirrors it, and a reply types
  the matching option's digit into the pane. A tap on a prompt someone already
  answered at the keyboard is refused (`409 prompt_gone`), never typed blind.
  Multi-select and multi-question prompts are shown but must be answered in
  the terminal on tmux.
- **Rename a workspace** — long-press a workspace on the phone to set its
  persistent display title, via `POST /sessions/{id}/rename` (a cmux
  workspace title or a tmux window name).
- **Open, split, show and close** — create a workspace in a directory under
  your home (`POST /sessions`; a new tmux window in the most recently active
  session), split the pane you are viewing (`POST /sessions/{id}/panes`, with
  a preview of where it lands; "add as tab" only where the host has tabs —
  tmux advertises `tabs: false` and the option disappears), make the host
  show what the phone is looking at (`POST /sessions/{id}/select`), and close
  a pane or a whole workspace after a confirmation (`DELETE`). Nothing created
  from the phone takes focus on the host unless asked.
- **YOLO mode** — long-press a workspace to set a per-workspace auto-reply
  mode (Off/Always/All tools/Bypass) for permission prompts; the agent
  replies on the host's behalf with no phone round-trip, and the mode is shown
  as a badge on that workspace's row and in its terminal pane. `Bypass`
  mirrors Claude Code's own `--dangerously-skip-permissions` on cmux; on
  tmux, where a prompt cannot switch the session's mode, `All tools` and
  `Bypass` both pick each prompt's own "always allow" option.
- **Custom sort order** — drag workspaces into any order via the handle on each
  row; purely a phone-local display preference, kept per host, not synced
  anywhere.
- **Direct (Tailscale) mode** — an optional, additive alternative to the
  relay above: if the phone and a Mac share a Tailscale tailnet, the app can
  talk straight to that Mac's agent with no relay or home server involved. See
  [bridge/README.md → Direct (Tailscale) mode](bridge/README.md#direct-tailscale-mode).
- **Optional push** — FCM "an agent needs you" notifications, off by default and
  requiring no Firebase config to build. One phone token is registered with
  every paired host; a notification names the host it came from and opens
  the app on that host. Attention pushes come from both host kinds (on tmux,
  from the Claude Code hooks); test pushes work on both.

The bridge performs read methods, terminal input/replay, feed replies
(including YOLO mode's automatic ones), workspace rename, and
workspace/pane create, select and close, each naming its target by id (a
cmux UUID or a tmux `$n` / `%n`). It never restores sessions. Creating a
shell from the phone adds no capability terminal input did not already give;
closing is the one destructive action and is confirmed on the phone first.

### tmux host: known limitations

- No tabs (tmux has none). Inbox, YOLO, attention stripes and pushes exist
  only for Claude Code panes with the hook installed; other agents in a
  pane are plain terminals to the phone.
- The Inbox mirrors Claude Code's prompt as drawn: a reply is a keystroke,
  so the option texts the bridge keys on (`Yes`, `Yes, …`, `No`, a question's
  labels) follow Claude Code's TUI and may need updating when it changes —
  the failure mode is a refused reply, not a wrong one.
- One window size per pane: while the phone is viewing a pane the window
  follows the phone's size, and an attached client sees that size too.
- `capture-pane` flattens cursor style/blink and extended underline styles.
- A pane a human has left in copy mode shows its live screen, not the
  scrolled view (tmux does not expose copy-mode content to control clients).
- The sessions list is per host; there is no merged view across hosts.

## Repository layout

```
android/              Jetpack Compose client (com.sodre90.cmuxremote)
bridge/               Go module: github.com/sodre90/term-bridge
  cmd/term-bridge-relay/   home-server rendezvous daemon
  cmd/term-bridge/         host agent (dials the relay): agent, pair-device, devices, status
  internal/host/           the Host interface, cmuxhost (Mac) and tmuxhost (Linux)
  internal/                server, cmux CLI client, tmux client, relay, tunnel, auth, push, …
  deploy/                  systemd, quadlet, launchd, nginx, container, example configs
docs/                 design specs and implementation plans
articles/             longer-form write-ups of features and components
research/             exploratory notes, not authoritative
.beads/               bd issue tracker state
THIRD_PARTY_LICENSES/ bundled-asset licenses (e.g. JetBrains Mono)
CHANGELOG.md          notable changes since the repo went public
CLAUDE.md, AGENTS.md  operating instructions for coding agents
```

## Relationship to cmux, tmux & licensing

This project is an **independent work** that communicates with
[cmux](https://github.com/manaflow-ai/cmux) over its documented IPC/CLI
contract and with [tmux](https://github.com/tmux/tmux) over its command-line
interface and control mode. It contains no cmux or tmux source. cmux is
GPLv3 and tmux is ISC-licensed; consuming a documented protocol over IPC does
not make this a derivative work of either.
