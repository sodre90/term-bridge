# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
This file starts now that the repo is public; changes from before this point
aren't retroactively itemized commit-by-commit here — see `git log` for the
full history. The 0.1.0 section is therefore a one-time snapshot of where the
project stood at first release, grouped by capability rather than by commit;
every section after it itemizes changes individually. Purely internal refactors
(string extraction, renames, test-only changes) are deliberately omitted.

## [Unreleased]

The phone now reaches a headless Linux box running tmux, next to the Mac
running cmux, and switches between them.

### Added

- **Linux host: tmux behind a `Host` seam.** The agent fronts either cmux
  or tmux (`host = "tmux"` in `agent.toml`; `bridge/internal/host`,
  `cmuxhost`, `tmuxhost`). On tmux every window is a workspace and every
  pane a terminal, across all sessions on the server; list, render
  (`capture-pane -e` parsed into the same cell grid the app already draws,
  incl. 16/256/truecolour, italic, underline, wide chars), input, paste,
  resize (window sized to the phone while viewed, released afterwards),
  split, rename, create, close, and structural change events from a
  control-mode client. Ids are tmux's `$n`/`%n`; every command targets one
  explicitly. Ships as a systemd user unit (`deploy/term-bridge-agent.service`,
  `deploy/agent.linux.example.toml`). The cmux path is a pure move behind
  the seam and unchanged in behaviour.
- **Host identity and capabilities on the wire.** `GET /sessions` carries a
  `host` block: `name` (short hostname), `kind` (`cmux`/`tmux`) and
  `capabilities` (`tabs`, `feed`). Older agents leave it out and the app
  assumes cmux with everything on.
- **Multi-host in the app.** Pair any number of hosts ("Pair another
  host…" from the sessions title or Connections) and switch between them;
  the title shows the selected host's name and kind. Every pairing, e2e
  session, credential and workspace order is stored per host, keyed by the
  agent's identity key, so a machine's relay and Tailscale slots land under
  one host. Existing pairings migrate in place on first launch. Push: the
  FCM token is registered with every paired host, a notification names its
  host and opens the app on it, and a push is decrypted by trying each
  host's session. Connections shows one card per host; forgetting a host's
  last slot removes it.
- **Capability-gated UI and host-neutral copy.** "Add as tab" disappears
  where the host has no tabs; the Inbox, YOLO mode and the pending-count
  poll are hidden on hosts without a feed (tmux, until its Claude Code
  hooks land). Every "Mac" in the app's copy now names the host ("Show on
  home-server") or is neutral.

### Changed

- **Renamed to Term Bridge.** With tmux hosts alongside cmux, the binaries,
  module and app carry a host-neutral name: `cmux-bridge` → `term-bridge`,
  `cmux-relay` → `term-bridge-relay`, `github.com/sodre90/cmux-bridge` →
  `github.com/sodre90/term-bridge`, the app is "Term Bridge". Config
  directories move with them (`~/.config/term-bridge`,
  `~/.config/term-bridge-relay`); a machine still holding the old directory
  and not the new one is refused at startup with the `mv` to run, so an
  agent can never mint a fresh identity and orphan its paired phones. The
  launchd job (`com.sodre90.term-bridge`), systemd units
  (`term-bridge-agent`, `term-bridge-relay`) and the quadlet follow.
  Unchanged on purpose: the Android applicationId
  (`com.sodre90.cmuxremote` -- a new id would be a new app), the e2e HKDF
  label and the relay CA's name (both bind existing pairings).

## [0.8.0] - 2026-09-13

The phone can now change what is on the Mac, not just look at it: attach a
photo to a pane, open workspaces, split panes with a preview of where the
new one lands, and close either. The bridge also stops cmux's style-id
renumbering from making unchanged scrollback look new on every frame.

### Added

- Attach a photo to a terminal pane from the phone. A Photo button next to
  Paste offers the gallery (Android's photo picker, no storage permission)
  or an image on the clipboard, shows what will be sent -- a thumbnail, the
  size of the copy, the dimensions -- and sends on confirmation. The copy is
  scaled so its longest side is 1568 px, turned upright from its EXIF
  orientation, and re-encoded as JPEG, which drops the camera data; that is
  the size Claude's API resizes to anyway, and turns a 3-8 MB photo into a
  few hundred KB. A switch sends the original instead, camera data and all
  (the dialog says so), when it is under 10 MB. The bridge lands it as a
  file and pastes the path into the pane, which Claude Code turns into an
  attached image; the delivery line under the key bar says whether that
  worked, and why not when the bridge refused it. A photo in flight shows as
  sending for as long as an upload its size plausibly takes on mobile data,
  rather than as delayed after the 1.5 s a keystroke gets. (cmux-app-ej0)
- The terminal socket accepts an `attach` frame carrying an image. The bridge
  writes it to `~/.config/cmux-bridge/attachments/` (configurable as
  `attachments_dir`; private to the user, kept seven days) under a name it
  chooses from a timestamp and the image's own bytes -- JPEG, PNG, WebP, GIF
  or HEIC, anything else is refused -- and pastes the path into the pane,
  which Claude Code turns into an attached image. This is the bridge half of
  attaching a photo from the phone. A refused attachment's ack now says why
  (`reason`: too large, not an image, attachments off, bad encoding), which
  the app shows. Along the way, a text paste now triggers an immediate replay
  the way a keystroke already did. (cmux-app-ej0)
- The terminal socket now bounds how large a message it will read from the
  phone (the attachment cap plus encoding overhead, about 14 MB). Before,
  it buffered whatever a paired device chose to send before looking at it;
  that was a gap from the start, only worth closing once a multi-megabyte
  frame became a legitimate thing to send. A message over the limit ends
  the socket like a decrypt failure does. (cmux-app-ej0)
- Create, show and close workspaces and panes from the phone. A "+" on the
  sessions list opens a new workspace in a directory picked from those the
  listed workspaces already use, or typed; the app goes straight into its
  terminal. A workspace's menu gains "Show on Mac" (brings it to the front
  there) and "Close workspace…", which names the workspace, counts its
  panes and warns when an agent in it is waiting or YOLO is on, before
  closing it -- the only confirmation is on the phone. The terminal's
  overflow menu gains "New tab in this pane" (opens and switches to it),
  "Show on Mac" and "Close this pane…". Nothing created takes focus on the
  Mac, and a bridge that predates these routes is reported as too old
  rather than as a mystery failure. (cmux-app-9ll)
- Split a pane from the phone, and see where the new one will land before
  it exists. "Split…" in the terminal's menu, and "New pane…" in a
  workspace's menu and at the foot of its expanded pane list, open a sheet
  with a miniature of the workspace's real pane layout: the pane being
  split shrinks to half and a ghost pane slides in from the chosen side --
  ← ↑ ↓ →, or Tab, which grows an extra tab on the pane instead. Tap
  another pane in the miniature to split that one. The button says what
  will happen ("Split right", "Add tab"); on success the terminal opens the
  new pane. A workspace never yet shown on the Mac has no real geometry, so
  its panes draw dashed as equal columns with a note. A refused split keeps
  the sheet up with the reason. (cmux-app-9ll)
- The bridge can create a workspace (`POST /sessions`, directory under
  `$HOME`), split the viewed pane or add a tab to it
  (`POST /sessions/{id}/panes`), report where a workspace's panes sit
  (`GET /sessions/{id}/layout`), show a workspace and surface on the Mac
  (`POST /sessions/{id}/select`), and close a surface or a workspace
  (`DELETE`). This lifts the never-create/close rule by owner decision; see
  `docs/superpowers/specs/2026-09-12-workspace-layout-control-design.md`.
  Every call names its target by UUID and nothing created takes focus on
  the Mac. (cmux-app-9ll)

### Compatibility

Wire-format change: a new `attach` terminal up-frame and a `reason` field on
the ack. An old app against a new bridge is unaffected -- it never sends an
attach and ignores the field. A new app against an old bridge sends an attach
the bridge does not recognise and, as with any unknown frame type, drops
without acking; the app shows the photo as delayed and never reports an
outcome. Update the bridge first. The attachment directory is created on
first use; no config, pairing or permission change.

Six new HTTP routes for workspace and pane control. An old app never calls
them; a new app against an old bridge gets 404s and says the bridge is too
old. No new config.

### Changed

- The bridge now gives style ids a stable identity per terminal socket. cmux
  renumbers its style table on every replay in the order it first meets each
  style, so scrolling one row moved almost every id even though the styles
  themselves were unchanged -- and since every span carries an id, that
  renumbering made the ~1400 scrollback spans and ~600 visible spans look
  changed on every frame, defeating both the sticky-block delta and the shared
  compression window. The bridge now numbers styles by content and rewrites
  the ids in `row_spans` and `scrollback_spans` to match, so a scrollback that
  did not scroll is byte-identical again and gets omitted. Id 0 stays the
  default style, which the app depends on. Replayed over 20 captured live
  frames, the scrollback block was omitted on 18 of 19 transitions (the
  remaining one was a real scroll) at 3.5-4.3 ms per 308 KB frame; the
  bandwidth this saves on the wire has not yet been measured live. Any grid
  shape the bridge does not recognise -- a changed epoch, a resize, cleared
  rows, a panned viewport, the alternate screen, a span or style missing a
  field -- goes out exactly as cmux produced it. (cmux-app-bly)

## [0.7.0] - 2026-09-12

Terminal frames now compress against a window shared across the whole socket,
which is worth 5-6x on small frames and nothing on large ones -- the large-frame
cost turned out to be a different problem entirely, now specced.

### Changed

- Terminal frames are now compressed against every frame sent before them on
  the same socket, rather than each one from scratch. Consecutive frames from a
  pane are nearly identical, and a compressor restarted per frame cannot see
  that at all; one kept open for the life of the socket can.

  How much it saves depends entirely on frame size, because DEFLATE's sliding
  window is 32KB. A frame smaller than that is compressed against its
  predecessor and costs a fraction of what it did: measured 607 bytes down to
  61, and 5-6x on synthetic frames up to ~14KB. A frame LARGER than the window
  gets nothing -- by the time the encoder reaches the end of a 140KB frame, the
  matching bytes of the previous one have already fallen out of the window, and
  the result is within 2% of compressing each frame on its own. Measured live
  on a busy pane sending 200KB frames: 6.1x, the same as before.

  So this helps settled and small panes and does nothing for large ones. The
  large-frame case turns out not to be a compression problem at all: cmux
  renumbers its style ids whenever a pane scrolls, which changes the bytes of
  every span in both the scrollback and the visible rows even though the text
  is identical. That is specced separately under
  `docs/superpowers/specs/2026-09-12-style-id-churn-design.md`.

  Negotiated separately from the existing per-frame compression (`?stream=1`,
  confirmed with `X-Cmux-Deflate-Stream`), so every combination of app and
  bridge version keeps working: an older bridge ignores the request and an
  older app never makes it.

  The trade-off is that frames no longer stand alone. A frame that arrives
  corrupt or not at all leaves the app unable to read anything after it, so it
  now closes the socket and resyncs from a fresh replay instead of skipping the
  frame -- the same bargain delta frames already made, and visible as a
  reconnect rather than as a wrong pane.

### Fixed

- A corrupt compressed frame could spin the terminal socket's reader thread
  instead of dropping the frame, if the decompressor stalled without either
  finishing or asking for more input.

### Compatibility

Wire-format change, and both halves stay interchangeable in both directions.
The shared window is negotiated twice like the capabilities before it -- the app
asks with `?stream=1`, the bridge confirms with `X-Cmux-Deflate-Stream` on the
101 -- and is deliberately separate from `?deflate=1` rather than folded into
it, because it is a stronger promise: a chunk of a stream can only be read in
order, by a decoder that saw every chunk before it. An old app against a new
bridge therefore keeps getting standalone frames, and a new app against an old
bridge gets no confirmation and streams nothing. Verified live on both a phone
and an emulator against the new bridge.

No config, pairing or permission change.

## [0.6.0] - 2026-09-12

Terminal streaming used to cost a measured 146 KB/s on a busy pane and roughly
the same on an idle one. It is now ~18 KB/s busy and nothing at all idle, with
a settings dial to go lower still.

### Added

- Two refresh intervals for terminal panes, one for Wi-Fi and one for mobile
  data, under Connections. The phone tells the bridge how often to re-read an
  open pane (`?poll_ms=`, clamped by the bridge to 250ms-10s); the default is
  250ms on Wi-Fi and 1s on mobile, so the saving applies on mobile without
  anyone changing a setting. Which interval is used is decided per socket by
  whether the current link is metered -- a hotspot marked metered, or tethering
  off another phone, counts as mobile -- so the reconnect that follows a
  Wi-Fi/mobile switch picks up the other value on its own. Typing is unaffected
  at every setting: input is answered immediately rather than on the next tick,
  so this trades only how quickly output you did not type appears.

### Changed

- An idle pane costs nothing. The poll loop compared whole grids to decide
  whether anything had changed, but cmux advances `render_revision` and
  `terminal_theme_revision` on its own clock, so the comparison never matched
  and a pane sitting at a shell prompt re-sent its entire ~187KB grid four
  times a second to deliver two incrementing integers. Those two counters are
  now excluded from the comparison.
- Terminal frames are compressed inside the e2e envelope, negotiated with
  `?deflate=1` and confirmed by the bridge on the 101. Compression has to
  happen before encryption -- ciphertext does not compress -- so this could not
  come from the transport.
- Frames leave out render-grid blocks the socket has already sent unchanged and
  name them in `unchanged`, rather than repeating them every tick. Scrollback
  alone is 143KB of a 187KB frame; `styles` and `modes` are a further 28% of
  what remains once it is gone. A block is omitted only while its bytes are
  identical to the last ones sent, so a changed block is always re-sent.

### Compatibility

Wire-format change, and both halves stay interchangeable in both directions.
Compression and delta frames are each negotiated twice -- the app asks, the
bridge confirms on the 101 -- so an old app against a new bridge keeps whole
uncompressed frames, and a new app against an old bridge gets no confirmation
and sends none of its own assumptions. Verified live in the old-app/new-bridge
direction. `?poll_ms=` needs no confirmation, since frames decode identically
at any interval; an older bridge ignores it and keeps its own rate.

The app gains `ACCESS_NETWORK_STATE`, a normal permission: granted at install,
never prompted for, and used only to ask whether the current link is metered.

## [0.5.1] - 2026-09-11

### Fixed

- Panes on the alternate screen can be panned again. 0.5.0 routed swipes there
  to the pane itself, but did it by claiming the drag before the grid's own
  scrolling could start -- and the grid is 79 rows against a phone viewport, so
  only whichever rows it happened to sit at were reachable, on Claude, `less`
  and `vim` alike. A swipe now pans the grid first and pages the pane only with
  the part the grid could not absorb. Found on a pane where that local panning
  was the only scrolling that could have worked at all: an idle shell stranded
  on the alternate screen after an agent exited without restoring the primary
  buffer, which pages on nothing, so the pane read as completely frozen.
- Mouse-reporting panes on the primary screen page the same way, so they now
  pan their grid before they page. This is a behaviour change on those panes
  (opencode among them), which used to page from the first swipe.

### Compatibility

No wire-format, config or pairing change; nothing on the bridge moved. Phone
only, and the two halves stay interchangeable in both directions across this
release.

## [0.5.0] - 2026-09-10

### Added

- Push is configured on the bridge instead of being baked into the app. The
  bridge now carries the client half of the Firebase configuration --
  `fcm_app_id`, `fcm_api_key` and `fcm_sender_id`, alongside the
  `fcm_project_id` it already had -- and hands it to a phone on the pairing
  response, which builds its own `FirebaseOptions` from it. Push used to be a
  build-time decision: an APK built without a `google-services.json` could
  never register an FCM token however the bridge was configured, so anyone
  handed the APK needed an Android toolchain to turn push on at all. None of
  those four values is a secret -- Firebase ships all of them in the clear
  inside every push-enabled APK, and none authorises sending, which still
  needs the service-account key in `fcm_credentials` that never leaves the
  machine.
- Both binaries warn at startup when they find a half-filled client config, or
  credentials with no client half. The config is withheld unless all four
  fields are set, and withholding is silent on the wire -- a phone has no way
  to report what it never received, so a relay would otherwise log that push
  is enabled while every phone paired against it goes on receiving nothing.

### Fixed

- Panes on the alternate screen scroll again. Such a pane has no scrollback of
  its own, so a swipe had nothing local to move and the pane read as frozen;
  swipes there now route to the pane itself. Two things had to be fixed for
  that to work: the drag never reached the handler (a descendant consumed it
  on the Main pass), and a step sized at half the viewport was longer than a
  thumb travels, so no step was ever emitted.
- A swipe that starts on a key-bar button no longer sends that key. Compose
  ends a tap only when something consumes the movement, and a vertical drag
  over the horizontally-scrolling key row was consumed by nothing -- so a
  scroll that began on Esc still fired it on lift-off. In a Claude pane one
  Escape interrupts a running agent and two open the rewind overlay, which is
  what made rewind keep appearing while scrolling.
- A pairing that turns push on now asks for the notification permission. With
  the config arriving at pairing, the startup prompt runs before there is
  anything to prompt about; the phone then registered a token and the bridge
  started sending into a device that had never been asked, which on API 33+
  drops every notification silently until the app is relaunched.

### Compatibility

No breaking change, in either direction. A bridge with push unconfigured sends
exactly the pairing response it sent before this field existed, so an older app
decodes it unchanged; a 0.4.0 app against a push-configured 0.5.0 bridge
ignores the field it does not know.

The `fcm_app_id`, `fcm_api_key` and `fcm_sender_id` settings are optional and
new. An agent or relay that does not set them behaves exactly as it did on
0.4.0, and phones paired against it receive no push unless their APK has a
`google-services.json` compiled in.

A phone running a release APK must re-pair to receive push. The config is
delivered at pairing and nothing else carries it, so a phone paired before
0.5.0 has none stored and upgrading the app alone does not bring push up.

A phone whose APK has a `google-services.json` compiled in is unaffected and
need not re-pair: `FirebaseInitProvider` creates the default app before any of
this runs, and that baked-in config is left alone as the more specific of the
two sources. If it and a bridge-supplied one name different projects, the phone
registers against one while the bridge sends from the other and push fails
silently, so use the same project for both.

## [0.4.0] - 2026-09-09

### Added

- `GET /version` on the agent, and an About card at the bottom of the
  Connections screen showing the app's version and the agent's. The two halves
  ship separately -- the app from a release APK, the agent from a binary on the
  Mac -- so "what am I running" has two answers, and neither was visible on the
  phone. The route sits inside the authenticated route set: a build number tells
  an unauthenticated caller which fixes an agent is missing.

### Compatibility

No breaking change. An app on 0.4.0 talking to a 0.3.0 agent shows the bridge
version as "unknown" (the route 404s) and is otherwise unaffected; a 0.3.0 app
against a 0.4.0 agent simply never calls the route.


## [0.3.0] - 2026-09-08

A reliability and observability release. Every issue listed as *Known* in 0.2.0
is fixed here. The theme is transports and terminals that recover on their own
instead of failing quietly: a standby listener that retries, a terminal socket
that survives a slow backend, push registration that keeps trying, and a status
command that can finally answer "is the other transport actually working?".

### Added

- `cmux-bridge status` now prints the agent's counters (pushes sent and failed,
  pairing codes issued/redeemed/expired, e2e decrypt failures, terminal replay
  failures). The agent serves no `/debug/vars`, so these had been incrementing
  where nothing could read them.
- `cmux-bridge status` also reports, per transport slot, when that slot was last
  reached end to end by the hourly device round -- and carries the value across
  agent restarts, so it can show an outage that began before the current
  process. A slot that has never answered reads differently from one that was
  never configured.
- The agent writes to a size-bounded log file of its own, so a sustained outage
  can no longer fill the disk.
- Pastes are sent bracketed (`ESC[200~`/`ESC[201~`) when the pane has bracketed
  paste enabled, so a multi-line paste arrives as one paste instead of running
  line by line as it lands.
- The terminal falls back to a font that has the box-drawing and block glyphs
  agent output uses, instead of drawing them as missing-glyph boxes.
- The key bar shows where it continues off-screen, and asks for confirmation
  before pasting a script into a shell.
- When the relay refuses the agent's tunnel upgrade, the log now names the HTTP
  status the relay actually answered with, which distinguishes an auth refusal
  from a proxy misconfiguration from a dead backend.
- Relay binaries built in the container are stamped with their version.

### Changed

- Unread state is no longer conveyed by the workspace colour, and expandable
  cards carry a chevron, so "has unread" and "which workspace" stop competing
  for the same signal.
- The Connections screen can be exited, reports zoom honestly, and no longer
  nags about re-pairing.
- The jump-to-bottom button is translucent, so the terminal rows underneath it
  stay readable.
- A retry loop that is already failing logs once per outage plus a periodic
  reminder, instead of one line per attempt.
- An agent whose session store predates the SQLite layout is imported in place
  rather than ignored.

### Fixed

- A slow `mobile.terminal.replay` no longer tears down a live terminal socket.
  One failure used to end the handler, and the phone's reconnect issued another
  full replay against the backend that was already too slow to serve one. The
  pane is now held on its last grid and recovers on the next successful poll,
  bounded so a backend that never returns cannot hold the socket forever
  (`cmux-app-8a0`).
- The app no longer reconnects forever to a terminal surface cmux no longer
  has. The agent closes such a socket with a distinct code and the app reports
  it instead of retrying every 5s behind a spinner (`cmux-app-34c`).
- The direct (Tailscale) listener retries instead of giving up on its first
  failure. A single "bind: can't assign requested address" at startup, before
  Tailscale had assigned its address, previously ended the standby transport for
  the life of the process -- and nothing noticed, because the relay was fine the
  whole time (`cmux-app-t5x`).
- `status.json`'s direct-mode health no longer counts the agent's own probe as a
  served request, which had made the field advance hourly whether or not a phone
  had ever reached the standby (`cmux-app-8d3`).
- FCM registration is retried until a slot accepts it, instead of failing
  silently after an app update until the next launch (`cmux-app-2cm`).
- A registration token FCM reports as `UNREGISTERED` is now dropped, so dead
  tokens stop being indistinguishable from live ones (`cmux-app-6u7`).
- Drifted credentials are reaped in both directions, so a relay device row can
  no longer outlive the shared secret it depends on (`cmux-app-2vz`).
- Aborting an already-confirmed pairing no longer drives the phone-facing state
  backwards from confirmed to refused (`cmux-app-05w`).
- A repeat push no longer re-alerts a notification already on screen
  (`cmux-app-17r`, `cmux-app-3ud`).
- Both Settings "Done" paths guard the back stack instead of leaving it
  inconsistent.

### Compatibility

No wire-format change. The only protocol addition is a WebSocket close code
(4404) that an older app treats as an ordinary close, so either side can be
upgraded alone. Upgrading both is still recommended: the two terminal fixes
above are one agent-side and one app-side, and they complement each other.

### Known issues

Carried into this release and tracked in `.beads/`: a poisoned replay window can
make a slot permanently undecryptable, and re-pairing does not recover it
(`cmux-app-a3g`); a direct-slot credential can die silently, producing a 401
lockout the moment the relay goes down (`cmux-app-hr1`); replay still exceeds
even the 20s budget on roughly 1.25% of terminal opens, which tracks the cmux
process's age rather than anything in this repo (`cmux-app-8a0`); attention
pushes are sent twice to a phone paired on both slots (`cmux-app-8jb`);
subprocess cmux RPC errors are untyped, so `not_found` handling only works on
the fast path (`cmux-app-aft`); opening an agent pane costs a burst of heavy
frames, and watching a running agent costs a sustained one (`cmux-app-dfl`); the
relay's `/healthz` can answer 200 while the agent's own tunnel handshake still
fails (`cmux-app-hxn`).

## [0.2.0] - 2026-09-05

A security release. Every credential in the system now has a way to be taken
back: a paired device can be revoked from either end, revocation terminates the
sessions already running on it, and each pairing gets its own keypair so one
device's key can never speak for another's. Pairing also gained an explicit
operator-approval step. Upgrade both sides together — see *Compatibility*.

### Added

- `cmux-bridge devices` — list and revoke the devices paired with an agent, so
  a lost or retired phone can actually be cut off. `cmux-relay devices
  list|revoke` gains the tenant-scoped equivalent, keyed by token hash, and
  `devices revoke` accepts the identifier `devices list` prints.
- `POST /devices/self-revoke`, letting a device retire its own token — this is
  what makes the app's **Forget** action revoke server-side instead of only
  clearing local state.
- An operator-approval step in pairing: the agent now signals that the person
  at the Mac said yes, and the phone waits for that answer rather than assuming
  it. A pairing has a distinct state between *redeemed* and *finished*.
- Per-slot credential health: the app records which slots a server has rejected,
  reports what each slot said when registering the device, and surfaces a
  rejected credential *before* it is the only one left rather than after
  connectivity is already gone.
- Each workspace's cmux-picked color renders as an identifying dot on its card.
- Vertical swipes page through TUIs that own their own scrolling (opencode and
  friends, which enable DEC mouse reporting and keep their PTY scrollback
  empty); they become PgUp/PgDn, which those parsers actually consume.

### Changed

- **Each pairing now mints its own keypair** instead of reusing one persistent
  device identity, so compromise of one pairing cannot decrypt another's
  traffic. The agent rejects a shared secret already paired to a different
  device.
- Re-pairing retires the credential it replaces, and a refused pairing revokes
  the token it had already minted, instead of leaving either valid forever.
- The app pauses its streaming sockets and event-driven refetches while it is
  backgrounded — the single biggest lever on idle cellular usage, since the
  subscriptions previously ran with the screen off. Push still covers attention
  while paused. The Inbox badge also refetches only on feed frames, so terminal
  output churn no longer costs a second full request per burst.
- The terminal refreshes immediately after input rather than waiting out the
  remaining poll tick, which made remote scrolling feel seconds behind the
  finger.
- Connection status reports the transport actually in use rather than the one
  preferred, and the direct listener reports what it is doing rather than only
  that it bound.
- The FCM token is registered on every configured slot, not just the first
  reachable one — previously a failover left push registered against the slot
  that was up at launch.
- A failed tunnel dial reports every address it tried.
- `detekt` is now part of the commit gate it had been missing from; it enforces
  import ordering and declaration spacing nothing else catches.

### Fixed

- A duplicate push could downgrade an already-delivered notification to a
  placeholder, replacing the real prompt text with a generic body.
- The terminal could replay a line that no longer existed. Terminal replay also
  gets its own deadline instead of inheriting the request's.
- Auto-scroll fought the user: on an actively streaming pane, the
  stick-to-bottom effect restarted every frame and snapped back before an
  upward swipe could take, making the pane feel unscrollable while output
  flowed.
- A socket is returned to the relay once the relay recovers, instead of staying
  on the fallback transport for the rest of the session.
- The relay admin CLI answered from a store it had just created (so it reported
  nothing), and accepted a `--config` stranded behind the subcommand.
- A relay 401 is no longer read as more than it can actually claim; a slot is
  marked rejected on any 401, not only on the launch probe.
- Attention push bodies carry the real pending prompt from `feed.list` instead
  of cmux's general last-activity preview, which could surface an unrelated
  system banner as the apparent reason an agent needed you.
- Relay → Tailscale failover is visible in the UI instead of silent.
- Answered prompts leave the Inbox immediately rather than lingering until the
  next feed update.

### Security

- Per-pairing key separation (see *Changed*) — the headline of this release.
- Revocation now terminates live state, not just future auth: unpairing or
  revoking a device closes its relay connections and its open sockets, and
  replacing a slot's credentials ends the sockets still running on the old
  ones. Previously a revoked device kept its established sessions.
- Shared secrets that no server has a device row for are reaped, so a secret
  cannot outlive the device it was minted for. (The converse — device rows
  outliving their shared secret, `cmux-app-2vz` — is still open.)
- The Android receive path performs validate/decrypt/commit as one atomic step,
  and a replay-window rejection is reported distinctly from a decrypt failure
  so the two stop being conflated in diagnostics.

### Compatibility

Per-pairing keys and the new pairing-confirmation state change the pairing
protocol. Update the app, `cmux-bridge` and `cmux-relay` together; existing
pairings continue to work, but pairing a 0.2.0 app against a 0.1.0 agent (or the
reverse) is not supported. Re-pair after upgrading both ends.

### Known issues

Carried into this release and tracked in `.beads/`: FCM token re-registration
fails silently after an app update until the next launch (`cmux-app-2cm`); push
titles are generic rather than naming the workspace (`cmux-app-17r`); dead FCM
tokens are indistinguishable from live ones (`cmux-app-6u7`);
`status.json`'s `direct_last_served_at` is stamped by the reaper's own probe and
so cannot report standby health (`cmux-app-8d3`); relay device rows can outlive
their shared secret (`cmux-app-2vz`); aborting an already-confirmed pairing
drives the phone-facing state backwards from confirmed to refused
(`cmux-app-05w`); the relay tunnel flaps on IPv6 handshake failures
(`cmux-app-to8`); a sustained relay outage floods the agent log, which has no
rotation (`cmux-app-5v1`).

## [0.1.0] - 2026-07-20

### Added

- Android client (Kotlin/Compose): workspaces list, a live terminal that
  renders cmux's `render-grid` cell grid (styles, colors, cursor,
  scrollback), an agent inbox for replying to blocking prompts, workspace
  rename, drag-to-reorder, and per-workspace YOLO auto-reply modes
  (Off/Always/All tools/Bypass).
- `cmux-relay` (home-server daemon) and `cmux-bridge agent` (Mac) Go
  binaries, with self-service QR/manual device pairing and a multi-tenant
  relay serving many independent Mac agents behind one mTLS edge.
- End-to-end encryption between phone and Mac agent (X25519 + HKDF-derived
  shared secret, AEAD on every HTTP body and terminal WebSocket frame,
  replay-protected counters) — the relay routes traffic by tenant but
  cannot read its contents, including push-notification bodies.
- Optional FCM push notifications ("an agent needs you"), off by default
  and requiring no Firebase config to build.
- Direct (Tailscale) connection mode as an additive alternative to the
  relay, with automatic dual-pairing fallback between relay and direct
  slots.
- Terminal UX: fit-to-width sizing, pinch-to-zoom, a word-wrap toggle, text
  selection, a compact D-pad, an Enter key, DECCKM-aware cursor keys, a
  latching Ctrl modifier chip, and TalkBack accessibility semantics.
- Operability: structured logging (`log/slog`), `expvar` metrics at the
  relay/pairing/push/e2e choke points, a `cmux-bridge status` subcommand,
  rate limiting on device pairing, GitHub Actions CI (bridge + android),
  `golangci-lint`/`detekt`, and Dependabot.
- Permission-request prompts render their content in the Inbox and can be
  answered there; previously only `AskUserQuestion` items had anything to
  show and a `permissionRequest` rendered an empty card.
- A terminal-pane picker when an attention notification can't resolve a
  single pane — cmux reports no per-pane id on the events that raise a push,
  so multi-pane workspaces previously dumped you on the sessions list.
- A persistent terminal font-size setting.

### Changed

- Migrated the agent-local `internal/e2e` and `internal/yolo` stores from
  a JSON file to SQLite, closing a cross-process clobber and a
  full-file-rewrite-per-terminal-frame cost.
- Migrated `internal/auth`'s device/token store to a multi-tenant
  SQLite-backed store with hashed tokens.
- Extracted the app-facing wire contract into a shared `internal/wire` Go
  package and deduplicated the pairing handlers previously duplicated
  between the relay and direct-mode server.
- Android toolchain moved to compileSdk 36, AGP 8.13, Kotlin 2.3, Compose BOM
  2026.06 and Gradle 8.14. Unit tests now require a JDK 21+ runtime (app code
  still targets JVM 17).
- Removed the "N workspaces on autopilot" banner — each workspace card
  already badges its own YOLO mode.

### Fixed

- A set of correctness and security findings from an internal audit (see
  `docs/enhancement-audit.md` and `docs/enhancement-audit-validation.md`):
  a replay-counter validate/commit race, unbounded reads on decrypt input,
  a cross-tenant push-registration leak, and related hardening.
- A stale pooled connection to cmux's control socket (after a cmux restart or
  a sleep/wake) surfaced as a spurious `cmux unavailable` 502 that only
  cleared on a manual refresh; the request is now retried.
- Inbox items are matched to their workspace by cwd, so `/sessions` and
  `/feed/pending` now canonicalize it — on macOS cmux reports the same
  location as both `/tmp/foo` and `/private/tmp/foo`, which broke the match.
- The Inbox badge counted workspaces with cmux's `has_unread` flag, which
  fires on any new output, so it could show a count while the Inbox was
  empty. It now counts actual pending items.
- Inbox "open terminal" routed to the wrong workspace when several shared a
  cwd prefix.
- Attention notifications are cancelled on open, repeat taps deep-link
  correctly, and notification-tap navigation no longer stacks duplicate
  screens on the back stack.
- The Connections screen scrolls, so its lower fields are reachable on
  shorter screens.
- Test pushes (`type=test`) no longer fail the client-side type check.

### Security

- Attention push-notification content is now end-to-end encrypted, so the
  relay operator cannot read a push's title/body even in relay-routed mode.
- Pairing now requires confirming a short fingerprint (SAS) of the exchanged
  public keys on both the phone and the Mac before either side trusts the
  other's key. The relay brokers that exchange, so without this step a
  malicious or compromised relay could substitute its own key and read all
  traffic for a newly paired device. The fingerprint stays visible after
  pairing completes so it can be re-checked.
- The Android e2e send/receive counters are now persisted durably *before*
  use, closing an AEAD nonce-reuse risk if the app process died between
  encrypting a frame and recording the counter.
