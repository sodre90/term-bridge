# term-bridge

Remote access to the terminal sessions on your machines — a Mac running
[cmux](https://github.com/manaflow-ai/cmux), a Linux box running
[tmux](https://github.com/tmux/tmux) — from anywhere, via two small Go
binaries:

- **`term-bridge-relay`** — a rendezvous daemon on your home server, behind nginx mTLS
  on a public DNS name. It owns device auth, pairing, and FCM push.
- **`term-bridge agent`** — runs on each host next to cmux or tmux. It **dials
  out** to the relay (so the host needs no inbound ports / port-forwarding)
  and serves the same HTTP/WebSocket API over the tunnel, whichever backend
  it fronts.

The agent speaks to its backend **only through the documented CLI**: `cmux rpc`
and `cmux events` on the Mac, the `tmux` command plus a control-mode client
(`tmux -C`) on Linux. It stores no socket password and copies no cmux or tmux
source — an independent work that consumes each program's CLI contract.
Inside the agent the two backends sit behind one `Host` interface
(`internal/host`; `cmuxhost`, `tmuxhost`), so the server, relay, auth, e2e
and push code is shared byte-for-byte.

## Architecture (v2 relay topology)

```
    ┌────────────────────────────┐
    │        Android app         │
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
          ▲                   ▲     yamux streams — routed by client-cert CN
          │                   │     (each agent dials OUT — agent:<tenant-id> client cert)
 ┌──────────────────┐  ┌──────────────────┐
 │ term-bridge agent│  │ term-bridge agent│
 │      (Mac)       │  │  (Linux, tmux)   │
 └──────────────────┘  └──────────────────┘
          │ cmux rpc / events      │ tmux CLI / tmux -C
          ▼                        ▼
      cmux.app                 tmux server
      (unchanged)              (unchanged)
```

The relay multiplexes every app request as a fresh [yamux](https://github.com/hashicorp/yamux)
stream over that tenant's single agent tunnel (a WebSocket, so it traverses
nginx on 443). The agent serves its handler verbatim. When a host is not
connected, the relay returns `503 {"error":"agent_offline"}` for that tenant
only. Every host is its own tenant — the relay does not know or care that two
of them belong to the same person.

Security is layered: **mutual TLS at the nginx edge for the agent only**
(`ssl_verify_client optional` — the agent presents a client cert signed by
the relay's own CA and is routed by its verified CN; devices have no client
cert) + a **per-device bearer token** checked by the relay + **end-to-end
encryption** between phone and agent (X25519 ECDH + HKDF derived at pairing,
AEAD over every HTTP body and terminal WS frame, replay-protected — the relay
can route it but not read it) + an `X-Relay-Token` shared secret the relay
injects so the agent only honors relay-originated requests. The relay binds
loopback only; no agent has a listening port at all.

## Build

Requires Go 1.26+.

```bash
cd bridge
go build -o term-bridge-relay ./cmd/term-bridge-relay   # for the home server
go build -o term-bridge       ./cmd/term-bridge         # for a host (agent mode)
GOOS=linux GOARCH=amd64 go build -o term-bridge ./cmd/term-bridge   # cross-build for a Linux box (no cgo)
go test ./...        # all tests run with no network and no real cmux or tmux
```

## Relay (home server)

1. Copy the binary to `/usr/local/bin/term-bridge-relay`.
2. Copy `deploy/relay.example.toml` to `/etc/term-bridge-relay/config.toml` and set
   `relay_token` (a long random secret) and optionally the FCM fields. On
   first run the relay generates its own CA (`ca_cert`/`ca_key`) and signs
   every agent and device cert against it — there's no separate hand-rolled
   CA to create any more. Migrating an existing deployment that already has a
   CA (RSA or ECDSA)? Point `ca_cert`/`ca_key` at those files instead and the
   relay loads and reuses it rather than minting a new one — nginx's trust
   bundle and any already-issued device certs need no changes.
   `ca_cert`/`ca_key` default to `~/.config/term-bridge-relay/ca.crt` / `ca.key` when
   unset, but the example file sets them under `/var/lib/term-bridge-relay/` instead
   — see the next step, `deploy/term-bridge-relay.service`'s `ProtectSystem=strict` +
   `StateDirectory=term-bridge-relay` only allow writes under `/var/lib/term-bridge-relay`,
   so the `~/.config` default would fail to create the CA there on first run.
3. Install the systemd unit and nginx vhost:

   ```bash
   cp deploy/term-bridge-relay.service /etc/systemd/system/
   systemctl enable --now term-bridge-relay
   cp deploy/nginx-term-bridge-relay.conf /etc/nginx/sites-available/cmux
   # enable the site + add the `map $http_upgrade $connection_upgrade` block, reload nginx
   ```

   The vhost also needs a `map $http_upgrade $connection_upgrade` block and a
   `limit_req_zone ... zone=term_bridge_device_pair` block in the `http` context —
   both are documented in the conf's header comment.

   If a new agent will self-register (see [Agent client
   certificate](#agent-client-certificate) below), also install the no-mTLS
   bootstrap vhost — `deploy/nginx-term-bridge-relay-bootstrap.conf` proxies only
   `POST /tenants/register`, on a separate port (8444 in the example). The
   main vhost above keeps `ssl_verify_client optional`, unchanged, for the
   agent tunnel and all device traffic.

The relay binds `127.0.0.1:8765`; nginx is the only public surface. nginx must
**set** both `X-Client-Cert-CN $ssl_client_s_dn` and `X-Client-Cert-Verify
$ssl_client_verify` (never trust inbound values) — the relay refuses to trust
a CN unless the verify header is `SUCCESS`, so an agent tunnel behind a vhost
that sets only the CN is rejected every time.

### Run in a container (podman)

Two supported ways to run the same container image, both built from
`deploy/Containerfile`. Pick by what your host's podman supports:

- **Quadlet** (`deploy/term-bridge-relay.container`) — systemd manages the container
  directly, no long-running compose process. Needs **podman 4.4+**.
- **Compose** (`docker-compose.yml`) — works on older podman and on Docker.

Either way, first drop a `config.toml` on the host (from
`deploy/relay.example.toml`, with a real `relay_token`,
`token_store = "/data/store.db"`, and `ca_cert`/`ca_key` under `/data/` too —
the example file's `/var/lib/term-bridge-relay/` paths are for the native systemd
unit and aren't part of the container's persisted data volume). The image
builds on the host; don't ship it across architectures.

#### Quadlet

```bash
podman build -t localhost/term-bridge-relay:latest -f deploy/Containerfile .
mkdir -p ~/.config/containers/systemd
cp deploy/term-bridge-relay.container ~/.config/containers/systemd/
# edit the config.toml path in that file to point at your copy
systemctl --user daemon-reload
systemctl --user start term-bridge-relay
loginctl enable-linger $USER    # survive logout, start at boot
```

Quadlet generates the `.service` unit from the `.container` file — edit the
`.container` and re-run `daemon-reload`; never edit or `enable` the generated
unit. Drop the file in `/etc/containers/systemd/` and omit `--user` for a
system-wide unit instead.

#### Compose

```bash
podman-compose up -d --build
```

Drop `config.toml` beside the compose file for this path.

#### Either way

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8765/healthz   # 200
```

The port is published on loopback only; the device store persists in a named
volume. Note the two paths use **different volume names** — compose creates
`term-bridge-relay_relay-data`, the quadlet template uses `term-bridge-relay-data` — so when
migrating from compose to quadlet, point the quadlet's `Volume=` at the
existing compose volume or the relay starts up with no paired devices.

Pair devices by running `term-bridge pair-device` on the host (see [Pair
a device](#pair-a-device) below) — the relay side needs no manual step.

## Agent (Mac)

The agent must run **in your GUI login session** to reach the per-user cmux
socket. Configure and install:

1. Copy `deploy/agent.example.toml` to `~/.config/term-bridge/agent.toml`; set
   `relay_url` (`wss://<your-domain>/agent/tunnel`), the client-cert paths, the
   server CA, and the same `relay_token` as the relay.
2. Install the LaunchAgent (it runs `term-bridge agent`):

   ```bash
   cp deploy/com.sodre90.term-bridge.plist ~/Library/LaunchAgents/   # edit REPLACE_ME paths
   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.sodre90.term-bridge.plist
   launchctl kickstart -k gui/$(id -u)/com.sodre90.term-bridge
   tail -f ~/Library/Logs/term-bridge.log
   ```

The agent reconnects automatically (exponential backoff, capped at 30s) if the
relay or network drops.

> **S5 note:** validate that the agent survives logout/login and sleep/wake — it
> lives in the GUI session and `KeepAlive` restarts it, but confirm on your
> machine after install.

## Agent (Linux, tmux)

The same binary fronts a Linux box with `host = "tmux"` in `agent.toml`:
every tmux window on the server is a workspace, every pane a terminal. The
agent, the tmux server and whatever runs inside it (Claude Code, a shell)
must be the **same Unix user** so the agent can reach the tmux socket. There
is no listening port; the agent dials the relay exactly as the Mac does and
is a tenant of its own, so the phone pairs with it separately and switches
between hosts.

1. Build for the box (`GOOS=linux GOARCH=amd64 go build ./cmd/term-bridge`
   cross-compiles cleanly: no cgo) and copy it to `~/bin/term-bridge`.
2. Copy `deploy/agent.linux.example.toml` to `~/.config/term-bridge/agent.toml`
   and fill in the relay URL, token and bootstrap URL as for the Mac.
3. Install the systemd user unit (see the comments in
   `deploy/term-bridge-agent.service` for the exact commands); enable linger
   so it runs without a login session. Logs go to `journalctl --user -u
   term-bridge-agent`.
4. Pair a phone with `term-bridge pair-device` on the box, as below.
5. Run `term-bridge hook install` once (as the same user) so Claude Code
   sessions in tmux feed the Inbox -- see the next section.

What differs from cmux, by design: a pane is its own only surface, so there
is no "add as tab" (the app hides it from the host's advertised
capabilities); a window a phone is viewing is sized to the phone
(`resize-window`), a viewed pane in a split window is zoomed (`resize-pane
-Z`) so it gets the whole viewport rather than a share of it -- zooming
also makes it the window's active pane, so an SSH-attached human's keys
land there while a phone views it -- and both are handed back to attached
clients a few seconds after the phone leaves; and
the Inbox, YOLO mode, attention stripes and pushes cover
Claude Code panes only, through its hooks, since tmux itself has no notion
of an agent prompt (the design: `docs/superpowers/specs/2026-09-14-linux-tmux-host-design.md`).

### Claude Code hooks on Linux

The agent listens on a unix socket, `$XDG_RUNTIME_DIR/term-bridge/hooks.sock`
(directory `0700`, socket `0600`; with no `XDG_RUNTIME_DIR` the feed is off
and the agent says so at startup). `term-bridge hook install` adds a command
hook running `<path>/term-bridge hook` to `~/.claude/settings.json` for
`PreToolUse`, `PermissionRequest`, `PostToolUse`, `Notification`, `Stop`,
`UserPromptSubmit` and `SessionEnd`, leaving everything else in the file as
it was; it prints the before/after first, `--dry-run` stops there, and a
second run is a no-op. Running Claude Code sessions pick it up on restart.

`term-bridge hook` reads the hook's stdin, forwards it with `$TMUX_PANE` and
the `$TMUX` socket path to the agent, waits for the agent to acknowledge,
and exits 0 with no output -- always. Outside tmux, with no agent running,
or on any error it does the same, which to Claude Code means "no decision":
its own prompt appears exactly as with no hook. Exit code 2 would block the
tool call, so the hook is dispatched before every other startup check in the
binary and never takes that path.

What the agent does with the hooks:

- `PreToolUse` records the tool call (this is the hook carrying
  `tool_use_id`); `PermissionRequest`, ~16 ms later, turns it into a pending
  item -- a `permissionRequest` with `tool_name`/`tool_input`, or a
  `question` with the `questions[]` structure for `AskUserQuestion` -- and
  raises the attention frame that becomes a push. A hook from a pane on some
  other tmux server (`%N` is reused per server) is refused.
- A reply types the digit of the option Claude Code drew: `once` → `Yes`;
  `always`/`all`/`bypass` → the prompt's qualified yes (`Yes, allow …`,
  `Yes, and always …`) except "switch to auto mode", falling back to `Yes`
  when the prompt has none; `deny` → `No`, or Esc when there is none; a
  question → the option whose label was chosen. The agent re-reads
  `capture-pane` right before typing (polling up to two seconds, since
  Claude draws the prompt only after the hook returns) and answers
  `409 prompt_gone` if the numbered list is not on screen. Only
  single-select, single-question prompts are answered this way; the rest
  are shown and answered in the terminal.
- The item clears on `PostToolUse`, `Stop`, `UserPromptSubmit`,
  `SessionEnd`, or when a `/feed/pending` read finds the list gone from the
  screen (a human answered at the keyboard). `Stop` marks the workspace
  waiting for input (amber stripe, `last_assistant_message` as its preview);
  a `Notification` of type `idle_prompt` raises the "waiting for your
  input" push, and `permission_prompt` only fills in for a missed
  `PermissionRequest`. A pane whose foreground process is no longer an agent
  carries no status whatever the feed remembers.

The hook payloads are the user's own content (the gated command, the
question) and are never logged. The keymap is coupled to Claude Code's TUI
wording; a wording change surfaces as refused replies, never as a wrong
keystroke.

The agent is co-located with the relay in the reference deployment; it still
dials the relay's **public** nginx name like any other host (hairpin), so
there is no loopback special case and the same cert bootstrap applies.

## Agent client certificate

No host needs a hand-rolled client cert. The relay generates its own CA the
first time it starts, and a new agent (Mac or Linux alike) registers itself
against it the first time *it* starts:

1. Point `bootstrap_url` in `agent.toml` at the relay's no-mTLS bootstrap
   vhost, e.g. `https://term-bridge.example.com:8444/tenants/register` — that's
   `deploy/nginx-term-bridge-relay-bootstrap.conf`, which proxies only that one path.
   A brand-new agent has no client cert yet, so it can't reach the main mTLS
   vhost at all; this separate surface is how it gets one.
2. On first run — only while `client_cert` doesn't exist on disk yet — the
   agent generates a keypair, sends a CSR to `bootstrap_url`, and the relay
   mints a fresh tenant and signs the cert with CN `agent:<tenant-id>`
   against its own CA. The agent writes the returned cert and key to the
   paths `client_cert` / `client_key` point at, and prints the tenant ID it
   was assigned, for example:

   ```
   agent: registered as tenant 9f3a2c1e4b7d0a6f... (cert written to /Users/you/.config/term-bridge/agent.crt)
   ```

   Self-registration never touches `ca_cert` — that setting is unrelated: it
   pins the CA that signed nginx's own *server* certificate, not the relay's
   internal agent/device-signing CA. Leave it empty (the default) if nginx
   presents a publicly-trusted server cert (e.g. Let's Encrypt), which is the
   common case. Only set it if you've deliberately given nginx a self-signed
   or private-CA server cert.

3. Every run after that skips registration — the cert is already on disk. Hand
   the printed tenant ID to whoever will pair phones for this agent (see
   [Pair a device](#pair-a-device) below).

## Pair a device

Pairing is self-service now — no operator step, and no hand-rolled `.p12`
client certificate. Run this on the **host you are pairing** (the Mac or the
Linux box), once its agent has registered (see [Agent client
certificate](#agent-client-certificate) above) — it uses that agent's own e2e
identity and session store, so running it anywhere else mints a second
identity on the wrong host and the phone's traffic will never decrypt. The
phone identifies a host by that identity: pair the same machine's relay and
Tailscale slots and they land under one host in the app; pair a second
machine and it appears as a second host to switch to:

```bash
term-bridge pair-device --config ~/.config/term-bridge/agent.toml
```

This asks the relay for a fresh, single-use pairing code, then prints a QR
code (and the code itself, for manual entry) to the terminal:

```
Scan this QR code with the Term Bridge app (code expires 2026-07-02T15:32:00Z):

█▀▀▀▀▀█ ▀▄█▀▀▄██ █▀▀▀▀▀█
█ ███ █ █▀▄ ▀▀▄█ █ ███ █
█ ▀▀▀ █ █▄▄▀▀▄▀█ █ ▀▀▀ █
▀▀▀▀▀▀▀ █▄▀ ▀ █▄▀ ▀▀▀▀▀▀▀

Or enter this code manually: 7F3K9QRT
```

The QR payload carries a one-time pairing URL, the code, and the agent's
public key. The app (see the separate Android QR-scanning work) scans it,
generates its own keypair, and calls the relay directly to redeem the code —
no client certificate needed for that call. `pair-device` polls in the
background and, once the phone redeems the code, derives a shared secret
with the device (X25519 + HKDF).

The device public key that redemption hands back came through the relay —
a compromised relay could substitute its own key there and silently MITM
the whole session. So before saving anything, `pair-device` prints a short
verification code (the SAS fingerprint of both public keys) and waits for
you to confirm it matches the one the phone is now showing:

```
Verify this code matches the phone's confirmation screen: 9F3A-B02C-77E1
Confirm? [y/N]:
```

Only on `y`/`yes` does it derive and save the shared secret; anything else
(including a blank line or Ctrl-D) aborts the pairing. Once confirmed,
content sent between this agent and that device is end-to-end encrypted, so
the relay operator (or anyone who compromises the relay host) can route
messages but not read them or forge a peer.

No camera handy, or pairing a phone remotely (e.g. over SSH)? The Android app
also has a manual-entry form: enter the server URL (the same `https://` base
the QR's `pair_url` uses) and the printed code. It resolves the agent's
public key via `GET /devices/pair-info/{code}` — a public, unauthenticated
endpoint that hands back exactly what the QR carries (`agent_pubkey`,
`expires_at`, `tenant_id`), scoped to the code alone since the phone doesn't
know its tenant yet. Same single-use code, same handshake, same e2e result,
same fingerprint-confirmation step on both ends — just without a scan.

`pair-device` never displays a raw device token to the operator — only the
phone that scanned the QR code ever sees it. List/revoke devices and tenants
exactly as before:

```bash
# --config must match the running relay's; without it these default to
# ~/.config/term-bridge-relay/config.toml and silently operate on an empty store.
term-bridge-relay devices --config /etc/term-bridge-relay/config.toml            # list devices (tokens redacted)
term-bridge-relay devices revoke <token> --config /etc/term-bridge-relay/config.toml
term-bridge-relay tenants list --config /etc/term-bridge-relay/config.toml       # created/revoked per tenant
term-bridge-relay tenants revoke <id> --config /etc/term-bridge-relay/config.toml
                                   # devices stop authenticating immediately;
                                   # the agent is refused on its next reconnect
```

Note: revocation is checked live on every connect/request, so new agent-tunnel
connects and all device authentication are blocked immediately. It does not,
however, forcibly close an agent that is already connected — that agent's
existing tunnel and its push-monitor goroutine keep running until the
connection ends on its own (a network blip, the agent process restarting, or
the relay itself restarting).

There is no manual-pairing fallback: `auth.Issue` always requires a device
public key, and the old `term-bridge-relay pair` subcommand is gone. Any phone still
paired under that flow lost relay access and must be re-paired via
`pair-device`.

## Direct (Tailscale) mode

An optional, additive alternative to the relay above: if your Mac and phone
are both on the same [Tailscale](https://tailscale.com) tailnet, the phone
can talk straight to the Mac agent with no relay and no home server in the
path. The relay keeps working exactly as before — this is a second listener,
not a replacement. Direct mode has its own optional push, configured with
`fcm_project_id` + `fcm_credentials` in `agent.toml`, plus the client half
(`fcm_app_id`, `fcm_api_key`, `fcm_sender_id`) the agent hands a phone at
pairing (same Firebase project as the relay's, but set separately because the
agent keeps its own device store); leave them empty to disable it. The client
half is independent of sending: an agent that has it but no `fcm_credentials`
still bootstraps a phone for push the relay will send. If you pair both slots,
set all four in **both** configs -- pairing a slot whose bridge sends no config
clears nothing, but only the slot that supplied one can replace it.

1. Install Tailscale on the Mac (Mac App Store, or `brew install --cask
   tailscale`) and run `tailscale up`.
2. In the [Tailscale admin console](https://login.tailscale.com/admin/dns),
   enable **MagicDNS** and, in the same DNS page's **HTTPS Certificates**
   section, enable HTTPS certificates for the tailnet.
3. Install the official Tailscale app from the Play Store on the phone and
   sign in to the same tailnet.
4. Confirm cert issuance works: `sudo tailscale cert
   $(tailscale status --json | jq -r .Self.DNSName)`.
5. Add to `agent.toml`: `direct_listen = ":8443"` (any free port), restart
   the agent.
6. Run `term-bridge pair-device --config ~/.config/term-bridge/agent.toml
   --direct`, then complete pairing on the phone (Settings → Enter server
   URL and code manually) using the printed
   `https://<mac>.<tailnet>.ts.net:8443` URL and code.

Relay and direct are two independent pairing slots on the app's Connections
screen, and you can fill both. With both paired the app tries the relay first
and transparently fails over to direct when it's unreachable (remembering
relay health so it stops re-trying a dead relay on every call), so pairing
both is the recommended setup rather than switching between them.

## Edge: nginx mutual TLS

See `deploy/nginx-term-bridge-relay.conf`. Point your home-server DNS name at nginx,
accept an optional client certificate (`ssl_verify_client optional` — agents
present one, self-service-paired devices don't; the relay tells them apart by
CN), and `proxy_pass` to `http://127.0.0.1:8765`. The `map $http_upgrade
$connection_upgrade` block (http context) is required for the agent tunnel
and the terminal/event WebSockets.

## Push (optional)

To get "agent needs you" notifications:

1. Create a Firebase project and a service-account JSON key.
2. Put the key on the **home server** and set `fcm_project_id` +
   `fcm_credentials` in the relay config.
3. Set the client half in the same config -- `fcm_app_id`, `fcm_api_key`,
   `fcm_sender_id` (see `deploy/relay.example.toml` for where to read each one
   out of a `google-services.json`). The relay hands these to a phone on the
   pairing response so the app can initialise Firebase itself, instead of
   needing a `google-services.json` compiled into the APK. All four counting
   `fcm_project_id` must be set or the block is withheld entirely -- Firebase
   rejects a partial set, and the relay warns at startup when it finds one.
   They are not secrets; every push-enabled APK ships them in the clear.
   Sending still needs `fcm_credentials`, which never leaves the home server.
   The config is delivered by pairing and only by pairing, so phones paired
   before these were set must re-pair.
4. The app registers its FCM token via `POST /devices/register` -- with
   **every** host it is paired to, since each is a separate tenant. The relay
   opens its own `/events` subscription over each agent tunnel; when an agent
   raises a blocking prompt it sends a high-priority FCM data message to every
   device paired to that tenant. The payload carries no host id: the phone
   tries each paired host's e2e session and only the right one decrypts.

Attention pushes come from both host kinds -- on tmux from the Claude Code
hooks above, one per prompt (`PermissionRequest` / `AskUserQuestion`) plus
`idle_prompt`. Test pushes (`POST /devices/test-push`) work on both.

cmux redacts the actual prompt text in its event stream, so push triggers on
the Claude Code hook name (`Notification` covers permission prompts and idle
"waiting for input"; `AskUserQuestion` is an explicit blocking choice) rather
than on structured feed content. The body, however, comes from `feed.list`,
which does carry the real prompt text: the newest pending item for that
workspace's cwd, either the question prompt verbatim or "Wants to run
<tool>: <command>" for a permission request. It falls back to a recognized
agent-status line and then to a generic phrase — an unrecognized cmux preview
is dropped rather than shown, so cmux's own system banners can't masquerade
as the reason an agent needs you. The payload is e2e-encrypted per device;
the relay fans it out without being able to read it.

Tapping the notification deep-links to that workspace's terminal (its one
pane directly, or the sessions list when it has several) — cmux never reports
which pane raised the prompt, so pane-exact linking isn't possible.

## API

The app's base URL is the relay's public domain (`https://<your-domain>`). All
routes require `Authorization: Bearer <device-token>`. A `503 {"error":
"agent_offline"}` means that host's agent is not currently connected to the
relay.

| Method | Path | Purpose |
|---|---|---|
| GET  | `/sessions` | list workspaces/terminals (normalized), plus a `host` block: `{name, kind: "cmux" \| "tmux", capabilities: {tabs, feed}}` the app gates its UI on |
| GET  | `/events` (WS) | agent feed + notifications; `needs_attention` flags blocking prompts |
| GET  | `/terminal/{id}` (WS) | replay + live output (down); input/paste/resize (up) |
| GET  | `/feed/pending` | list pending blocking prompts (full question/option structure) |
| POST | `/feed/{id}/reply` | answer a prompt: `{kind, request_id, params}` |
| POST | `/sessions/{id}/rename` | set a workspace's title (cmux workspace title / tmux window name): `{title}` |
| POST | `/sessions/{id}/yolo-mode` | set a workspace's auto-reply mode for permission prompts: `{mode}` (`""` \| `always` \| `all` \| `bypass`) |
| POST | `/sessions` | create a workspace: `{cwd, title?}`; `cwd` must exist, be a directory and lie under `$HOME` → `{workspace_id, surface_id}` |
| GET  | `/sessions/{id}/layout` | where the workspace's panes sit, as fractions of their bounding box: `{estimated, panes:[{id,x,y,w,h,focused,surface_ids,selected_surface_id}]}` |
| POST | `/sessions/{id}/panes` | new terminal relative to a viewed surface: `{surface_id, placement}` (`left` \| `right` \| `up` \| `down` \| `tab`; `tab` is refused where `capabilities.tabs` is false) → `{surface_id, pane_id}` |
| POST | `/sessions/{id}/select` | show the workspace on the host, focusing `{surface_id?}` |
| DELETE | `/sessions/{id}` | close a workspace |
| DELETE | `/sessions/{id}/panes/{surfaceId}` | close one terminal surface |
| POST | `/devices/register` | store this device's FCM token: `{fcm_token}` |
| POST | `/devices/test-push` | send a test notification to this device |
| POST | `/devices/pair` | redeem a pairing code (no bearer token yet): `{code, name, device_pubkey}` |
| GET  | `/devices/pair-info/{code}` | resolve a pairing code's agent pubkey for manual entry (no auth) |
| GET  | `/healthz` | relay liveness check (no auth) |

Terminal frames carry a cell grid (`format: "cmux.render-grid.v1"`): cmux's
`render_grid` verbatim on a Mac, and on a tmux host the same shape built from
`capture-pane -e` (SGR parsed into per-cell styles). The client renders both
identically. Ids are opaque to the app: cmux UUIDs, or tmux's `$n` window
and `%n` pane ids.

`/feed/pending`, `/feed/{id}/reply` and `/sessions/{id}/yolo-mode` answer
on cmux hosts and on tmux hosts with the hook feed (`capabilities.feed`);
a tmux agent started without `XDG_RUNTIME_DIR` advertises `feed = false`
and the app does not call them. On tmux `/feed/{id}/reply` answers
`409 {"error":"prompt_gone"}` when the prompt is no longer on screen.

`feed.*.reply`'s params beyond `request_id` (confirmed against cmux's own RPC
contract strings, not guessed): `feed.permission.reply` takes
`mode: "once" | "always" | "all" | "bypass" | "deny"`; `feed.exit_plan.reply`
takes `mode: "ultraplan" | "manual" | "autoAccept" | "bypassPermissions"`;
`feed.question.reply` takes `selections: [string]`.

## Safety

The bridge calls read methods, terminal input/replay, feed replies,
workspace rename (cmux's own documented `workspace.rename` RPC, the same
one behind `cmux rename-workspace` / Cmd+Shift+R), YOLO mode's
auto-replies to permission prompts (`feed.permission.reply`, the same RPC a
phone tap on Allow/Bypass in Feed sends — see below), and the workspace and
pane mutations behind `cmux new-workspace` / `new-split` / `new-surface` /
`select-workspace` / `close-workspace` / `close-surface`
(`workspace.create`, `surface.split`, `surface.create`, `workspace.select`,
`surface.focus`, `workspace.close`, `surface.close`). Every mutation names
its target by UUID and is refused with a 400 before any RPC otherwise --
cmux's create methods default to whatever is focused on the Mac when given
no target. Nothing created this way takes focus (`focus:false`); a new
workspace's directory must exist, be a directory and lie under `$HOME`.
It never restores sessions. Tests use a fake `cmux` binary and never touch
the real socket.

On a tmux host the same routes map onto `list-windows`/`list-panes`,
`capture-pane`, `send-keys`/`paste-buffer`, `resize-window`, `resize-pane -Z`, `new-window`,
`split-window`, `rename-window`, `select-window` and `kill-window`/`kill-pane`,
always targeting a window or pane by id (`$n`/`%n`), never "the current
one". A control-mode client (`tmux -C attach -f no-output`) supplies the
structural change events. Tests run against a fake `tmux` script that also
plays back recorded control-mode notification bursts; the SGR parser has a
`capture-pane -e` fixture of a real Claude Code prompt.

**YOLO mode** is an opt-in, per-workspace auto-reply for permission prompts,
enabled via `POST /sessions/{id}/yolo-mode`. The mode (`always`/`all`/
`bypass`) is persisted locally on the agent (a SQLite store at
`~/.config/term-bridge/yolo.db`, overridable with `yolo_store`, keyed by
workspace ID — never sent to cmux itself). When a workspace with a
mode set gets a pending `permissionRequest`-kind feed item, the agent replies to it
with that mode automatically, with no phone round-trip. `bypass` mirrors
Claude Code's own `--dangerously-skip-permissions`: cmux's wrapper already
launches Claude with `--allow-dangerously-skip-permissions`, so a single
`bypass` reply switches that session into `bypassPermissions` for good.
Correlating a pending item to a workspace is done by matching cwd — cmux
pending items key on the agent's own session ID (`workstream_id`, e.g.
`"claude-<uuid>"`), not the cmux workspace ID, so cwd is the only field both
share (confirmed live; see `internal/server/yolo.go`).

## Licensing

The bridge is an independent work that communicates with cmux over its
IPC/CLI and with tmux over its command-line interface and control mode. It
contains no cmux or tmux source. cmux is GPLv3 and tmux is ISC-licensed;
consuming a documented protocol over IPC does not make this a derivative work
of either.
