# Plan: Linux host support — tmux behind a `Host` seam, multi-host in the app

Spec: `docs/superpowers/specs/2026-09-14-linux-tmux-host-design.md`.
Epic: cmux-app-pxu; one child bead per phase below (.1–.6).

Six phases, each its own branch and gated by the full Go and Android gates
(`CLAUDE.md` → *Build & verify*). Phases 3 (app multi-host) and 4 (tmuxhost)
are independent after phase 2 and may run in parallel; phase 4 is
live-tested by pairing the emulator to the Linux agent as its only host.

Hazards for whoever runs this:

- **Phase 1 is a refactor with zero behaviour change.** Do not fold any
  tmux or wire work into it. The Mac must behave byte-for-byte the same
  after deploy; the existing fake-cmux tests are the proof and must stay
  untouched.
- **Never probe tmux mutations against the owner's live sessions on the
  home server.** Do every experiment inside a scratch session
  (`tmux new-session -d -s cmux-app-scratch`) and kill it by name at the
  end. The `cmux-live-state-hazard` rule applies to tmux ids the same way.
- **Hooks touch `~/.claude/settings.json` on the server.** `hook install`
  prints the diff and asks; never write it silently, never in a test.
- The **verify-live list** in the spec (items 1–10) is a prerequisite for
  phases 4 and 5. Record each result in the spec's table before coding the
  part that depends on it.

## Phase 0 — verify live (no commit; results go into the spec)

On the home server, in the scratch session, as the same user that will run
the agent:

- tmux: `#{start_time}` rendering; `display -p -F '…' \; capture-pane -e
  -p -N` returns both in order; which control-mode notifications are
  session-scoped vs global (`%unlinked-window-*`, `%sessions-changed`) and
  whether `refresh-client -B` subscriptions give a cheap per-pane dirty
  signal; `resize-window` flips `window-size` to
  manual and `set-option -wu window-size` restores it for an SSH client;
  `capture-pane -e` of a Claude Code TUI pane — save the bytes as a test
  fixture.
- Claude Code hooks: install a throwaway `command` hook that `tee`s stdin to
  a file for `PermissionRequest` and `Notification`; trigger a permission
  prompt and an `AskUserQuestion`; capture the exact JSON and whether
  `$TMUX_PANE` is in `env`. Return `{}` from `PermissionRequest` at once
  and confirm the TUI prompt appears with no visible delay. Record the
  prompt's exact option texts and the keystroke each accepts, and whether
  `Notification permission_prompt` arrives before or after the prompt is
  drawn (this is the `keymap.go` fixture).
- cmux (Mac): `cmux rpc mobile.workspace.list` — record `preview` for a
  Claude Code pane and a plain shell (spec item 10).
- Relay: from the server, `curl` the public nginx endpoint (hairpin) and
  confirm a loopback tunnel without the edge token is refused.

## Phase 1 — bridge: extract `Host`, `cmuxhost` as a pure move (cmux-app-pxu.1)

`bridge: extract a Host interface; cmux behind it unchanged`

- `internal/host/host.go`: `Host` interface as in the spec (`Capabilities`,
  `ListWorkspaces`, `Layout`, `CreateWorkspace`, `SplitPane`, `CreateTab`,
  `Select`, `Rename`, `CloseWorkspace`, `CloseSurface`, `Replay`, `Input`,
  `Paste`, `Resize`, `Feed`, `FeedReply`, `Events`, `ValidID`);
  `Capabilities{Tabs, Feed, FeedKinds, Sessions, ViewportPerClient}`;
  `ErrUnsupported`; `IsNotFound(err)` re-exported so `cmux.IsNotFound`
  callers move without changing semantics.
- `internal/host/cmuxhost/`: one file per today's server file whose cmux
  parsing moves — `sessions.go` (`mobile.workspace.list` → `wire.Workspace`
  incl. `classifyAttention`, `classifyKind`, `cleanTitle`, `canonicalPath`,
  `parsePanes`), `layout.go` (`pane.list` → `normaliseLayout`),
  `terminal.go` (`mobile.terminal.replay/viewport/input/paste`),
  `feed.go` (`feed.list`, `feed.*.reply`, `feedMethod`), `mutate.go`
  (`workspace.create/select/rename/close`, `surface.split/create/focus/
  close`), `events.go` (`cmux events --category feed --category
  notification --reconnect` → `host.Event`). Moves, not rewrites: `git mv`
  where a whole file goes, then trim. Their tests move with them, still
  driven by `testutil.WriteFakeCmux`.
- `internal/server`: `Server.host host.Host` replaces `Server.cmux`.
  **Keep `New(c *cmux.Client, s *auth.Store)`** as a convenience wrapping
  `cmuxhost.New(c)` so every existing test that builds a fake `cmux` bin
  compiles and passes untouched; add `NewWithHost(h host.Host, s *auth.Store)`
  for phase 4's tests. Handlers become thin: validate → `s.host.X` → encode.
  The delta handshake, style table, `Unchanged` computation, e2e, YOLO
  auto-reply and push stay in `server` — they are bridge protocol, not host.
- `cmd/cmux-bridge/agent.go`: builds `cmuxhost.New(cmuxClient)`; `OnReached`
  wiring unchanged.
- Tests: `go test ./...` green with no test file edited except imports
  and package moves; one new `host` conformance test skeleton
  (`hosttest.Run(t, func() host.Host)`) exercised by `cmuxhost` against the
  fake bin, to be reused by `tmuxhost`.
- Deploy to the Mac, live-check sessions list, terminal, Inbox reply, YOLO,
  create/split/close in a scratch workspace.

## Phase 2 — wire: host identity and capabilities (cmux-app-pxu.2)

`wire: host_name, host_kind and capabilities on pairing and status`

- Go `internal/wire`: `HostInfo{Name, Kind, Capabilities}` on
  `PairingCodeInfoResp` and on the status response the app already polls;
  `Capabilities` mirrors `host.Capabilities` (wire type, not the host type,
  to keep the package boundary).
- Relay `internal/relay/relay.go`: the pairing DTO copy gains the same
  field, passed through blind.
- Kotlin `model/Dtos.kt`: `HostInfo`, `HostCapabilities` with tolerant
  defaults (`tabs = true`, `feed = true`, `feedKinds = all`) so an older
  bridge behaves as today.
- Tests on all three sides: round-trip, defaults on absence.
- App reads and stores `HostInfo` per slot but changes no behaviour yet.

## Phase 3 — app: multi-host (cmux-app-pxu.3)

`app: multi-host — HostId keying, host switcher, pair another host`

- `data/HostId.kt`: value class over the agent public-key fingerprint
  (same derivation as the SAS fingerprint's input, reuse that code).
- `Settings`: every `key(slot, KEY_*)` becomes `key(hostId, slot, KEY_*)`;
  `migrateLegacyIfNeeded` gains step 2: existing slot-keyed data → the
  `HostId` derived **offline** from `CryptoSession`'s persisted
  `KEY_PEER_PUBLIC_KEY` (the agent pubkey is stored per slot at pairing, so
  no network fetch and no `LEGACY` placeholder is needed; a slot with no
  session record has nothing worth migrating). Same for `SlotCredentials`,
  `CryptoSession` stores, the YOLO badge cache, `WorkspaceOrderStore`,
  `TerminalDisplayStore`.
- `AppContainer`: a `HostRegistry` (list of paired hosts + selected host,
  persisted) and per-host `BridgeGateway`/`FallbackBridgeClient`/
  `SocketReconnector` instances created lazily; the selected host's
  gateway is what the view models see today, so screens change minimally.
- UI: sessions list top bar shows the host name with a dropdown when >1
  host; Inbox and terminal scoped to the selected host; Connections screen
  lists hosts, "Pair another host" reuses `PairingViewModel` unchanged and
  stores under the new `HostId`; per-host `HostCapabilities` gate the
  placement sheet's "tab" option and feed kinds.
- Push: FCM registration per host; notification payload already carries
  the tenant — map tenant → host on tap and select it before navigating.
- Tests: `Settings` migration (slot-keyed → fingerprint-keyed, offline), registry
  selection persistence, placement sheet hides "tab" when
  `capabilities.tabs=false`, `ViewModelConstructionTest` still constructs
  everything (watch cmux-app-2jj's flake).
- Live: pair the Samsung to the Mac twice (relay + direct) as before —
  everything works as today — then pair the emulator to the Mac and to a
  second `cmux-bridge agent` instance on the Mac pointing at the same cmux
  (two hosts, same content) to exercise switching without needing Linux.

## Phase 4 — bridge: `tmuxhost` (cmux-app-pxu.4)

`bridge: tmux host — workspaces, panes, render grid, input, events`

- `internal/tmux/`: thin exec client (`tmux [-S sock] …`) plus a
  `Control` type owning **one** `tmux -C attach` process with
  `refresh-client -f no-output`, used for **structural events only**:
  `%sessions-changed`, `%window-add`/`%window-close`/`%window-renamed`,
  `%unlinked-window-add`/`-close` (windows of sessions the control client
  is not attached to — control mode is session-scoped, and Decision 5 wants
  every session), `%layout-change`, `%exit`; `%begin/%end/%error` framing;
  reconnect with `backoff` like `RunEvents`. **Content refresh is polled**
  with `capture-pane`, exactly as the Mac path polls
  `mobile.terminal.replay` today behind the bytes-changed gate;
  `%output`-driven refresh (one control client per session, or
  `refresh-client -B` format subscriptions as a per-pane dirty signal) is a
  follow-up bead, not v1. Unit-tested against recorded transcripts.
- `internal/host/tmuxhost/`:
  - `ids.go`: `tmux-<epoch>-wN` / `-pN` encode/decode, `ValidID`.
  - `sessions.go`: `list-panes -a -F` with one format string → windows →
    `wire.Workspace` (title, cwd, preview = last non-blank captured line,
    `Kind` and the attention-classifier row chosen by
    `#{pane_current_command}` — `claude`, `qwen`, `zsh`… — which is far
    more reliable than the title heuristic `classifyKind` uses on the Mac),
    `sessions` list for the create dialog.
  - `attention.go`: table of `{command, regexp, state}` over the last N
    captured lines + shell-idle rule; tests per pattern; Qwen CLI strings
    from phase 0.
  - `grid.go`: `display -p -F … \; capture-pane -e -p -N [-S -n]` in one
    exec → SGR parser (`sgr.go`, 16/256/truecolor, bold/faint/italic/
    underline/inverse/strike) → style table + `row_spans`/`scrollback_spans`
    with `go-runewidth` cell widths → `wire.RenderGrid` with `modes` as
    `{"ansi":false,"code":N,"on":bool}` for 1/1000/1002/1003/1006/2004,
    `active_screen`, `cursor`, `state_seq` = a bridge counter bumped when the
    captured bytes change. Golden
    tests from the phase 0 fixture, compared against the Mac render of the
    same fixture where possible.
  - `terminal.go`: `send-keys -l` / `-H`; paste wrapped iff
    `bracket_paste_flag`; `Resize` = `resize-window -x -y` + viewer refcount
    per window, `set-option -wu window-size` at zero.
  - `mutate.go`: `new-window -d -c -n -t <session>:`, `split-window -d -h|-v
    [-b]`, `select-window`, `rename-window`, `kill-window`, `kill-pane`;
    `CreateTab` → `host.ErrUnsupported` (server maps to 400
    `unsupported`).
  - `Capabilities{Tabs:false, Feed:false (until phase 5), Sessions:true,
    ViewportPerClient:false}`.
  - Run the `hosttest` conformance suite against a fake `tmux` script
    (like `WriteFakeCmux`) and the recorded control-mode transcript.
- `config/agent.go`: `host`, `tmux_bin`, `tmux_socket`; `agent.go` picks
  the host by `host`.
- `deploy/cmux-bridge-agent.service` (systemd user unit, journald
  logging, `WantedBy=default.target`), `deploy/agent.linux.example.toml`,
  `bridge/README.md` Linux section.
- Live on 192.168.1.160 (scratch session only): sessions list, terminal
  render vs. an SSH-attached view, input/paste into a shell, resize +
  restore, create/split/close, polled refresh + structural events, host restart
  → ids rotate and the app shows the workspaces gone.

## Phase 5 — bridge: `agentfeed` (cmux-app-pxu.5)

`bridge: Claude Code hooks feed permission prompts to the phone on Linux`

Gate: phase 0 results for hook JSON, `$TMUX_PANE`, the prompt's option
texts/keystrokes and `Notification permission_prompt` timing. If the
prompt has no stable on-screen option text to key on, stop and bring the
evidence to the owner before improvising.

- `cmd/cmux-bridge/hook.go`: `cmux-bridge hook` reads stdin JSON, requires
  `TMUX_PANE`, connects to `$XDG_RUNTIME_DIR/cmux-bridge/hooks.sock`
  (0600), forwards, writes the bridge's response to stdout; **any failure →
  exit 0, empty output** (no decision). `cmux-bridge hook install [--dry-run]`
  edits `~/.claude/settings.json` idempotently after printing the diff.
- `internal/host/agentfeed/`: unix-socket listener owned by the agent
  process (mode 0600, parent dir 0700). `PermissionRequest` → respond `{}`
  at once, record `{tool_name, tool_input, tool_use_id}` against the pane.
  `Notification permission_prompt` → promote the record to
  `host.FeedItem{Kind:"permissionRequest", RequestID: tool_use_id, Surface:
  pane id, Tool, Input}` (or a screen-text item if no record). `FeedReply`
  → `keymap.go` resolves mode → expected option text + keystroke, re-reads
  `capture-pane` for that pane, refuses with `prompt_gone` if the text is
  absent, else `send-keys`. Item clears on `idle_prompt`/`agent_completed`
  or when the prompt text leaves the screen. `Notification idle_prompt /
  agent_needs_input / agent_completed` → attention updates and
  `notification` events. `tool_input` is feed content: never logged.
- `tmuxhost` composes `agentfeed`: `Feed`/`FeedReply`/`Events` merge it;
  `Capabilities.Feed=true`, `FeedKinds` per phase 0 (`question` only if a
  hook exposes options).
- Server: no change beyond what phase 1 left; YOLO auto-reply already
  calls `host.FeedReply`.
- Tests: socket round-trip with a fake hook client; `PermissionRequest`
  always answers `{}` immediately; record → item promotion and the
  no-record fallback; keymap against the phase 0 prompt fixture;
  `prompt_gone` refusal when the screen changed; YOLO modes → option
  mapping; item clearing; passthrough on missing pane;
  `multitenant_test.go` unchanged and green.
- Live: Claude Code in the scratch session on the server, phone paired to
  the Linux host: permission prompt appears in the Inbox *and* in the SSH-attached view;
  reply from the phone allows/denies; answering in SSH first clears the
  Inbox item; a stale tap after the prompt is gone is refused with
  `prompt_gone`; YOLO `always` picks the always option; stripes and push
  fire; `hook install` diff reviewed by the owner before applying.

## Phase 6 — docs (cmux-app-pxu.6)

`docs: Linux host — README, CLAUDE.md, improvement guide`

- `README.md`: architecture paragraph gains the Linux agent; "What the app
  does" gains host switching and the tmux limitations list; security
  section: Linux agent has no listening port, hooks socket ACL, the
  allow/deny authority statement from the spec.
- `CLAUDE.md` invariant 1: "cmux is a black box … tmux likewise: only via
  its CLI and control mode, never by reading its source or socket format".
- `docs/improvement-guide.md`: host seam described where §
  `internal/cmux/client.go funnels all cmux RPC` currently is; Linux
  deploy pointers.
- Close cmux-app-pxu with the commit list; file follow-up beads for
  Qwen/Gemini hook adapters, merged multi-host sessions list, and the VT
  emulator fallback if phase 4's golden tests found fidelity gaps.
