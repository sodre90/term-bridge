# Linux host support: tmux behind a `Host` seam, multi-host in the app — Design

Status: Approved 2026-09-14, including the permission reply path (B,
mirror the TUI prompt). Nothing here is implemented.

Bead: cmux-app-pxu (children .1–.6, one per plan phase).
Plan: `docs/superpowers/plans/2026-09-14-linux-tmux-host-plan.md`.

## Decisions

Taken with the owner on 2026-09-14:

1. **Linux target is the home server** (192.168.1.160, where `cmux-relay`
   already runs). It is **headless**: no display, so GUI terminals (kitty,
   WezTerm GUI) are out and the phone is the only UI for anything running
   there.
2. **tmux is the Linux terminal backend.** Nothing else that runs headless
   exposes every terminal-mode flag the app's render grid needs *and* has a
   push event channel. Owner has no existing tmux/kitty/WezTerm preference.
3. **cmux stays on the Mac.** Linux is a second backend behind a new `Host`
   interface in the bridge; the cmux path keeps its behaviour byte-for-byte.
   No "one backend everywhere".
4. **The app gains multi-host.** The Linux box runs its own `cmux-bridge
   agent` as a separate relay tenant; the phone pairs with it independently
   and switches between hosts. Rejected: Mac agent proxying to Linux over
   SSH (Linux would vanish whenever the Mac sleeps); replacing the Mac
   pairing with the Linux one (throwaway).
5. **Every tmux session on the server is visible**, not one dedicated
   session. Workspace create therefore needs a target session.
6. **cmux's "add as tab" has no tmux analog and is hidden**, not faked: the
   bridge advertises capabilities and the app shows only what the host can
   do.
7. **Agents on Linux**: shells, Claude Code, Qwen CLI and possibly other
   harnesses. The structured Inbox (permission/question replies, YOLO) is
   built on Claude Code hooks; every other program gets attention stripes
   from a screen-text classifier only.
8. **Permission prompts are mirrored, not intercepted** (path B in *Feed
   on Linux*): Claude Code draws its own prompt; the phone's answer is typed
   into the pane with `send-keys`. Keeps today's either-side-can-answer
   semantics and leaves allow/deny authority with Claude Code; the cost is a
   keystroke map coupled to Claude Code's TUI, guarded by re-reading the
   prompt before typing.

## Context

The bridge is "the only seam that talks to cmux" in name: `internal/cmux`
shells out to `cmux rpc` / `cmux events`, but `internal/server/*` picks the
method names and parses cmux's JSON itself (`mobile.workspace.list`,
`mobile.terminal.replay`, `pane.list`, `feed.list`, …). There is no
semantic interface a second backend could implement.

Everything the phone does reduces to two buckets:

- **Commodity multiplexer operations**: list workspaces/panes with stable
  ids, snapshot a pane with colours + cursor + modes, send input/paste,
  resize, create/split/close/select/rename by id, an event stream.
- **The agent feed**: structured `permissionRequest` / `question` /
  `exitPlan` items with options and a reply target (`feed.list`,
  `feed.*.reply`, `cmux events --category feed`). This drives the Inbox,
  YOLO mode and push. **No Linux terminal has it.** On Linux it comes from
  the agent itself (Claude Code hooks), which decouples it from the
  terminal choice entirely.

The Android app additionally assumes exactly one host with two transports
(`ConnectionSlot.RELAY|DIRECT`); credentials, e2e sessions, YOLO badges and
push registration are all keyed on that single host.

## Terminal survey (why tmux)

Requirement | tmux 3.6 | kitty | WezTerm | Zellij
--- | --- | --- | --- | ---
Headless | yes | **no** (needs display) | mux-server only | yes
Stable ids | `%N` pane, `@N` window, `$N` session | window id | `pane_id` | plugin API only
Coloured snapshot | `capture-pane -e -p` | `kitten @ get-text --ansi` | `wezterm cli get-text --escapes` | `dump-screen` (no colour)
DECCKM / mouse / bracketed-paste / alt-screen state | `#{keypad_cursor_flag}` `#{mouse_*_flag}` `#{bracket_paste_flag}` `#{alternate_on}` — all verified on the Mac's tmux 3.6b man page | not via CLI | not via CLI | no
Cursor | `#{cursor_x}` `#{cursor_y}` `#{cursor_flag}` | no | no | no
Event push | control mode `tmux -C`: `%output`, `%layout-change`, `%window-add`, `%window-close`, `%window-renamed`, `%exit` | Python watchers (in-process) | none via CLI | plugin
Structured API | line protocol + `-F` formats | JSON over unix socket | CLI / JSON | CLI

The app's `RenderGridDecoder` reads DEC private modes 1, 1000/1002/1003,
1006, 2004 and `active_screen` to drive arrow-key encoding, wheel
forwarding and paste wrapping. Only tmux exposes all of them without the
bridge owning a VT emulator. Control mode is the direct analog of
`cmux rpc` + `cmux events` + the FastPath socket pool.

## Architecture

```
phone ──relay/direct──► cmux-bridge agent (Mac)   ──► internal/host/cmuxhost ──► cmux rpc/events
phone ──relay/direct──► cmux-bridge agent (Linux) ──► internal/host/tmuxhost ──► tmux / tmux -C
                                                  ◄── internal/host/agentfeed ◄── Claude Code hooks
```

### `Host` interface (bridge, `internal/host`)

A semantic interface extracted from what `server/*` does today, so the
cmux implementation is a pure move of existing code:

```go
type Host interface {
    Capabilities() Capabilities            // {Tabs bool, Feed bool, ViewportPerClient bool, ...}
    ListWorkspaces(ctx) ([]Workspace, error) // today: mobile.workspace.list
    Layout(ctx, workspaceID) (Layout, error)  // today: pane.list
    CreateWorkspace(ctx, CreateWorkspaceSpec) (Created, error)
    SplitPane(ctx, surfaceID, Direction) (Created, error)
    CreateTab(ctx, ...) (Created, error)      // ErrUnsupported on tmux
    Select(ctx, surfaceID) error
    Rename(ctx, workspaceID, title) error
    CloseWorkspace / CloseSurface(ctx, id) error
    Replay(ctx, surfaceID, Viewport) (RenderGrid, error)  // cmux.render-grid.v1
    Input / Paste(ctx, surfaceID, text) error
    Resize(ctx, surfaceID, Viewport) error
    Feed(ctx) ([]FeedItem, error)
    FeedReply(ctx, FeedReply) error
    Events(ctx) (<-chan Event, error)       // feed | notification | layout
}
```

`Capabilities` is exposed on the wire (`GET /host` or folded into the
existing status/pairing response) so the app can hide "add as tab" and any
other unsupported affordance per host. `wire.RenderGrid` stays the
untouched mirror of `cmux.render-grid.v1`; tmuxhost becomes a second
*producer* of that format, not a new format.

Server handlers lose every `Rpc(ctx, "mobile.…")` call and every cmux JSON
shape; those move verbatim into `cmuxhost`. Existing server tests keep
passing against a fake `Host`; existing cmux-shape tests move with the
code. Selected by `host = "cmux" | "tmux"` in `agent.toml`, default `cmux`.

### `tmuxhost`

Object mapping:

cmux | tmux | notes
--- | --- | ---
workspace | window (`@N`) | title = `#{window_name}` (rename → `rename-window`); cwd = active pane's `#{pane_current_path}`; `preview` = last non-blank line of `capture-pane` (see *Attention* below — cmux's `preview` is a synthesized agent-status string, tmux has no equivalent)
pane | pane (`%N`) | 1:1
surface (tab) | **= pane** | one surface per pane; `CreateTab` → `ErrUnsupported`
`workspace.create {cwd,title,focus}` | `new-window -d -c cwd -n title -t <session>:` | `-d` = never steals focus (Decision 6 of the 2026-09-12 spec carries over); session chosen by the phone from the list, default: the session of the workspace the phone last viewed
`surface.split {direction}` | `split-window -d -t %N -h|-v [-b]` | `left/up` = `-b`
`workspace.select` | `select-window -t @N` (and `switch-client` for attached clients) | headless → mostly a no-op that matters when someone is SSH-attached
`workspace.close` / `surface.close` | `kill-window -t @N` / `kill-pane -t %N` | confirmed on the phone, as today
`mobile.terminal.input/paste` | `send-keys -t %N -l` (`-H` for control bytes) | paste wrapped in bracketed-paste sequences by the bridge iff `#{bracket_paste_flag}` (the app already conditions on the mode)
`mobile.terminal.viewport` | `resize-window -t @N -x C -y R` while a phone views the window; `set-option -wu -t @N window-size` when the last viewer leaves | see *Resize*
`mobile.terminal.replay` | one invocation: `display -p -t %N -F '…' \; capture-pane -e -p -N -t %N [-S -n]` so cursor/modes/size and text come from the same server turn | see *Render grid*
`cmux events` | one long-lived `tmux -C attach` process with `refresh-client -f no-output` | structural events only (`%sessions-changed`, `%window-*`, `%unlinked-window-*`, `%layout-change`) → `layout` events; `%exit` → reconnect with backoff. Control mode is *session-scoped* (`%output` covers only the attached session), so v1 polls `capture-pane` for content exactly as the Mac path polls replay today; `%output`-driven refresh is a follow-up

**Ids.** tmux ids are unique per server lifetime and are reused after a
server restart. A server restart also kills every process, so "all
workspaces closed" is the correct phone-side reading. Ids on the wire are
therefore `tmux-<epoch>-w3` (window `@3`) / `tmux-<epoch>-p5` (pane `%5`),
where the epoch is the tmux server's `#{start_time}` (listed in the 3.6b
man page; its exact rendering is a verify item). Plain `[A-Za-z0-9-]` only
— tmux's own `%` and `@` sigils would be malformed percent-escapes in
`/sessions/{id}`. Per-workspace phone/bridge state (YOLO mode, sort order)
keyed on an old epoch is dead state and gets garbage-collected on the next
list. **Verify** that the app treats ids as opaque strings everywhere; the
2026-09-12 plan added a "UUID check on every path id" in the bridge — that
check becomes a `Host.ValidID` call.

**Attention.** `classifyAttention` matches "needs your permission" /
"waiting for your input", which are phrases cmux *synthesizes* into
`preview` for agent panes — Claude Code's TUI never prints them, so the
heuristic transfers nothing to `capture-pane` text. For Claude Code panes
attention comes from hooks (below). For everything else v1 ships a small
per-harness screen-text classifier in `tmuxhost`: Qwen CLI's prompt/idle
strings and a generic "shell at prompt, no output for N seconds = idle"
rule, extended as harnesses are added. Scoped as a table of patterns with
tests, not a parser.

**Render grid.** For each replay the bridge runs `capture-pane -e -p -N`
over the visible screen (and `-S -<scrollback_rows>` for the scrollback
block, which the existing delta handshake already only ships when changed)
and parses SGR into the `styles` table + `row_spans`, using `go-runewidth`
for `cell_width`. `display -p` with one format string supplies `columns`,
`rows`, `cursor {row,column,visible}`, `modes` as cmux-shaped
`{"ansi":false,"code":N,"on":bool}` objects for codes 1, 1000, 1002, 1003,
1006, 2004, and `active_screen` from `#{alternate_on}`. `state_seq` is a
bridge counter bumped when the captured bytes change; the existing
"gate on render-grid bytes" comparison keeps working unchanged.
Alternative considered: feed `%output` into a Go VT emulator
(`charmbracelet/x/vt` or similar) for perfect fidelity — rejected for v1
because the bridge would own an emulator and tmux already exposes the state
we read; kept as a fallback if capture-pane fidelity disappoints
(known gaps: `capture-pane -e` does not emit cursor style/blink; underline
colour and curly underline are flattened to `underline`).

**Resize.** The phone's viewport is the window size. `resize-window -x -y`
sets it, and per the man page **automatically switches that window to
`window-size manual`** — so "latest client wins" does not hold once a phone
has resized. Accepted and made explicit: while any phone views a window it
is sized to the phone; when the last viewer leaves, the bridge runs
`set-option -wu -t @N window-size` so the window falls back to the global
option and an SSH-attached human gets their own size back. Headless makes
the "phone reflows the desktop user" concern mostly moot;
`Capabilities.ViewportPerClient=false` lets the app know two phones on one
pane share a size. Two tenants viewing the same pane at different widths
fight; accepted, same as two humans attached today.

### Feed on Linux: Claude Code hooks (`internal/host/agentfeed`)

Terminal-agnostic. Claude Code's hooks carry `session_id`, `cwd`,
`tool_name`, `tool_input`, `tool_use_id`, and the hook process inherits
`$TMUX_PANE` from the pane it runs in — that is the reply target, no
screen scraping.

- **Transport**: a `command` hook running `cmux-bridge hook` (new
  subcommand) that forwards stdin JSON over a unix socket in the agent's
  runtime dir (0600, owner-only). Chosen over Claude Code's `http` hook type
  so the Linux agent keeps the Mac agent's property of **no listening port
  at all**; the socket is the one local IPC surface and its ACL is the
  filesystem.
- **Passthrough rules, whichever reply path is chosen.** The hook helper
  must never stall a Claude session that is not ours: no `$TMUX_PANE` in
  its environment → exit 0 immediately with no decision; bridge socket
  absent or unreachable → exit 0 immediately with no decision; any bridge
  error → no decision. "No decision" always means Claude Code's own TUI
  prompt appears as if no hook existed.
- **`permissionRequest` items: path (B), mirror the TUI prompt** (owner
  decision 2026-09-14; the alternative (A), holding the `PermissionRequest`
  hook until the phone decides, was rejected because it makes the agent look
  hung to an SSH-attached human and moves allow/deny authority into the
  bridge).
  - `PermissionRequest` hook **always returns immediately with no
    decision**; it only records `{tool_name, tool_input, tool_use_id,
    session_id}` against the pane (`$TMUX_PANE`). Claude Code then draws its
    own prompt, exactly as with no hook.
  - `Notification permission_prompt` fires once that prompt is on screen and
    turns the pending record into the feed item (correlated by pane id +
    order; `tool_name` from the notification payload if present — verify
    item 3). No record → a feed item with the pane's screen text as
    context, so a prompt is never silently dropped.
  - **Reply = `send-keys` into the pane**: the keystroke Claude Code's
    prompt accepts for Allow / Always / Deny. The map lives in one table
    (`agentfeed/keymap.go`) keyed by the prompt's on-screen option text,
    which the bridge **re-reads from `capture-pane` immediately before
    typing**: if the expected option text is not on screen the reply is
    refused with `ack.ok=false, reason=prompt_gone` rather than typed into
    whatever is running now. This is the guard against a stale tap and the
    detector for a TUI change.
  - YOLO: `always` → the prompt's "always allow this tool" option; `all` →
    the same option per prompt (there is no "all tools" choice in the
    prompt); `bypass` → same as `all` on Linux, with the badge text making
    the difference explicit (`--dangerously-skip-permissions` can only be
    set at launch, not from a prompt). `deny` → the deny/Esc option.
  - Known cost, accepted: the keymap is coupled to Claude Code's TUI and
    needs a fixture update when the prompt changes; the `prompt_gone`
    refusal surfaces that on the phone instead of typing blind.
  - The feed item clears when the pane's `Notification` stream moves on
    (`idle_prompt`/`agent_completed`) or the prompt text leaves the screen,
    covering the case where the human at the SSH session answered first.
- **`Notification` hook** (`permission_prompt`, `idle_prompt`,
  `agent_needs_input`, `agent_completed`) → attention state for the
  workspace list stripes and push (`needs-permission` / `waiting-input`),
  plus `notification` events on the stream. Non-Claude panes use the
  screen-text classifier described under *Attention*.
- **`question` items**: the docs list `agent_needs_input` but do not show
  a hook exposing AskUserQuestion's options. **Verify live** whether
  `PreToolUse`/`PermissionRequest` fire for `AskUserQuestion` with
  `tool_input.questions`; if yes, they map to `question` items with
  `selections` replies delivered via the hook decision; if not, `question`
  degrades to an attention stripe + open-the-terminal on Linux (a
  `Capabilities.FeedKinds` list tells the app).
- **`exitPlan`**: not wired on the Mac either; out of scope.
- **Installation**: `cmux-bridge hook install` writes the hook entries into
  `~/.claude/settings.json` (idempotent, prints the diff first). Manual
  alternative documented.
- Qwen CLI / other harnesses: attention stripes only in v1. Qwen Code
  inherits Gemini CLI's hook system; a follow-up bead can add an adapter
  once one Linux agent path is live.

### Multi-host in the app

Today `Settings`, `SlotCredentials`, `CryptoSession`, the YOLO badge cache
and FCM registration are keyed by `ConnectionSlot` only. Introduce a
**`HostId`** = fingerprint of the agent's identity public key, which both
slots already receive at pairing as `agent_pubkey` (`wire/pairing.go`) and
which is what makes RELAY and DIRECT "the same Mac" today. Not the relay
tenant id: the DIRECT slot has no tenant. Key everything by
`(HostId, ConnectionSlot)`:

- `Settings.migrateLegacyIfNeeded` gets a second migration: existing
  single-host data becomes host `default` (the Mac), with its display name
  taken from the pairing response's `host_name` (new wire field; the Mac's
  is its hostname, the Linux one `hostname -s`).
- **Host switcher** on the sessions list (top-bar dropdown; the current
  host's name in the title). Sessions list, Inbox and terminal are scoped
  to the selected host. A merged "all hosts" list is deliberately *not* in
  v1 — ids, capabilities and feed semantics differ per host and the
  switcher keeps every screen single-host.
- Connections screen lists hosts; "Pair another host" runs the existing
  pairing flow (QR + SAS fingerprint confirmation, unchanged) and stores
  under the new `HostId`.
- Push: FCM data messages already carry the tenant; the notification tap
  deep-links to the right host + workspace. One FCM token is registered
  with each host (bead cmux-app-8jb's duplicate-push issue is per host and
  unaffected).
- Per-host `Capabilities` gate UI: no "add as tab" on tmux hosts; feed
  kinds per host; "Show on Mac" relabelled from the host name.

Wire changes (all three copies — `model/Dtos.kt`, `internal/wire`,
`internal/relay/relay.go` — in one commit each): `host_name`,
`host_kind`, `capabilities` on the pairing/status response;
`session` (tmux session name) on `CreateWorkspaceRequest` when
`Capabilities.Sessions` is set; a `sessions` list in the workspace list
response for the create dialog.

### Deployment on the home server

Rootless podman is the house convention, but the agent must reach the
user's tmux server socket and `~/.claude`, and spawn `tmux`/`claude` as the
user — a container adds nothing but socket plumbing. Ship a **plain systemd
user unit** (`bridge/deploy/cmux-bridge-agent.service`, `loginctl
enable-linger`), the Linux counterpart of the Mac's launchd plist. The
agent, the tmux server and Claude Code must run as the **same Unix user**
(tmux socket, `~/.claude/settings.json`, hook socket); `agent.toml` gains
`host = "tmux"`, `tmux_bin`, `tmux_socket` (optional `-S`). Relay dial: the
relay binds loopback and expects nginx's edge token, so the co-located
agent dials the **public nginx endpoint** exactly like the Mac (hairpin
through the LAN/Tailscale name) rather than loopback — no special-casing,
same cert bootstrap. Verify the hairpin works from the host itself and
what the relay does with a loopback tunnel that carries no edge token
(should be refused; confirm). Logging: journald via stdout instead of the
launchd-specific file handling in `internal/logging` (already stderr-first;
no darwin-only assumptions found by grep).

## Security

- The Linux agent keeps **no listening port**; hooks arrive over a
  0600 unix socket owned by the same user. Hook payloads (`tool_input` can
  contain typed commands and paths) are feed content: e2e-encrypted to the
  phone like cmux feed items today and **never logged** (invariant 5).
- The bridge never decides a permission itself: the `PermissionRequest`
  hook always returns no decision, and a phone reply is typed into a prompt
  Claude Code is showing. The `prompt_gone` guard (re-read the screen before
  `send-keys`) ensures a reply never lands as input into anything but that
  prompt. Only an authenticated, e2e-verified phone reply or a stored YOLO
  mode triggers typing.
- `send-keys` is shell input, the same capability terminal input already
  grants. `kill-window`/`kill-pane` remain the one destructive class and
  keep the phone-side confirmation.
- Tenant isolation is untouched: each host is a tenant; the relay stays
  blind. `multitenant_test.go` must pass unchanged.
- Workspace-create cwd validation (absolute, `EvalSymlinks`, under `$HOME`,
  is-dir) moves into `Host`-independent server code and applies to tmux
  identically.

## Known limitations (accepted for v1)

- No tabs on tmux; no `exitPlan`; `question` items only if the live probe
  finds a hook carrying options.
- Non-Claude agents: attention stripes from text heuristics only.
- One window size per pane; two viewers fight.
- `capture-pane` flattens cursor style/blink and extended underline styles.
- tmux copy-mode content is not visible to control-mode clients (tmux
  documents this); a pane a human has left in copy mode shows its live
  screen, not the scrolled view.
- Merged multi-host sessions list is out of scope.

## Verify live before the plan is final

1. `PermissionRequest` hook: exact input JSON; that an immediate empty
   response lets the TUI prompt appear unchanged and adds no visible delay.
2. Whether any hook exposes `AskUserQuestion` questions/options.
3. `Notification` input fields for `permission_prompt` / `agent_needs_input`.
4. `$TMUX_PANE` is present in the hook process environment (Claude Code
   may sanitise env for hooks).
5. `capture-pane -e` output for a Claude Code TUI pane round-trips through
   the SGR parser into a grid the app renders identically to the Mac's
   (compare against a cmux render of the same content).
6. The app treats workspace/surface ids as opaque strings on every path.
7. `resize-window` flips the window to `window-size manual` (man page says
   so): confirm, and confirm `set-option -wu window-size` restores the
   global behaviour for an SSH-attached client afterwards.
8. `#{start_time}` rendering, and that `display -p … \; capture-pane …`
   in one invocation returns both outputs in order.
9. The exact option texts and keystrokes Claude Code's permission prompt
   accepts (Allow / Always / Deny), and how `Notification permission_prompt`
   timing relates to the prompt being on screen (fixture for
   `agentfeed/keymap.go`).
10. What `mobile.workspace.list` returns as `preview` for a Claude Code
    pane versus a plain shell, to size the screen-text classifier.

### Verified 2026-09-14 on 192.168.1.160 (Fedora 44, tmux 3.7c)

Items 5–8 plus the control-mode question, all in a scratch session; the
hook items (1–4, 9) and item 10 are still open and gate phase 5 only.

Item | Result
--- | ---
5 | `capture-pane -e -p -N` of Claude Code's trust prompt saved as `bridge/internal/host/tmuxhost/testdata/claude-trust-prompt.capture`. Besides SGR it carries **OSC 8 hyperlinks** (`ESC]8;id=…;url ESC\`), so the parser skips every OSC (ESC\ or BEL terminated), not only CSI.
6 | Pending (phase 3 reads the app's id handling; the bridge side is `Host.ValidID`).
7 | Confirmed: `resize-window -x 60 -y 20` flips `window-size` from `latest` (global) to `manual`; `set-option -wu window-size` unsets it again. With no client attached the window keeps 60x20 after the unset — expected, there is no client size to follow.
8 | `#{start_time}` renders as plain epoch seconds (`1789367814`). `display -p -F … \; capture-pane -e -p -N` returns the format line first, then the screen rows, in one invocation.
control mode | `tmux -C attach -t <s> -f no-output` (stdin must stay open) reports `%unlinked-window-add/-close/-renamed` and `%sessions-changed` for **every** session, `%window-add/-renamed`/`%layout-change` for the attached one, and no `%output`. Enough for a "list changed" signal across all sessions from one client.
hairpin | From the server, `https://sodre-cmux.mywire.org/agent/tunnel` reaches nginx (403 without a client cert); a loopback tunnel to the relay without the edge token is refused (401). The bootstrap vhost (:8444) is **not** exposed on the owner's nginx, so a new tenant is registered by hand: CSR → `POST /tenants/register` on the relay's loopback port with `X-Edge-Token`.
tmux version | 3.7c on the server (the survey was written against the Mac's 3.6b man page; every format used above exists in both).
empty history | `capture-pane -S -240 -E -1` on a pane with `history_size` 0 prints the screen's **first row once** rather than nothing (`-E -1` clamps to row 0); with N>0 history rows it prints exactly min(N, 240). Found in the phase 4 live test (every fresh or `clear`ed pane failed replay); `Replay` drops the echoed row when history is 0.
live test | Phase 4 end-to-end on the emulator paired to the Linux agent, 2026-09-14: list, render (16/256/truecolour, italic, underline, wide chars), input, paste, resize + `window-size` release, split, rename, close pane/window, create window, control-mode refresh, agent restart. Not exercised: `classifyKind` (no agent was run in a pane) and a tmux **server** restart (the stale-epoch path).

Wire deferrals decided while implementing phase 2: host identity and
capabilities ride on `GET /sessions` (`host` object beside `workspaces`),
which the app already polls and which the relay never parses — the pairing
DTO copy and the `session` field on `CreateWorkspaceRequest` move to phase
3, where the switcher and the create dialog first consume them.

## Phasing (each its own branch; detail in the plan)

1. **Bridge: extract `Host`**, `cmuxhost` as a pure move, server on the
   interface. Zero behaviour change; full gate green; deploy to the Mac and
   live-check.
2. **Wire: `host_name` / `host_kind` / `capabilities`** on all three copies;
   app reads them but changes nothing yet.
3. **App: multi-host** — `HostId` keying + migration, host switcher,
   Connections "pair another host", capability-gated UI.
4. **Bridge: `tmuxhost`** — list/layout/replay/input/resize/mutations,
   control-mode events; screen-text attention classifier; fake-tmux tests +
   live test against the home server. Linux deploy unit + docs.
5. **Bridge: `agentfeed`** — hook socket, `cmux-bridge hook` + `hook
   install`, permission items via the chosen path → Inbox + YOLO,
   `Notification` → stripes/push. Live test with Claude Code on the server.

Phases 3 and 4 are independent: 4 can be live-tested by pairing a debug
build or the emulator to the Linux agent as its *only* host, so multi-host
gates daily use, not verification. Run them in either order or in parallel.
6. **Docs**: README (Mac-only wording, security claims), CLAUDE.md
   invariant 1 (cmux black-box wording gains "tmux via its CLI only"),
   improvement guide.
