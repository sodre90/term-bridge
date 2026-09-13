# Open, split, show and close workspaces and panes from the phone — Design

Status: Approved 2026-09-12. Lifts the "never create/close/restore"
rule the repo has carried since day one; see *Decisions* and *Security*.

Bead: cmux-app-9ll (children .1/.2/.3, one per plan commit).

## Decisions

Taken with the owner on 2026-09-12:

1. **Scope**: create a workspace; create a pane in an existing workspace as a
   split (left/right/up/down) or as a tab; make the Mac show the workspace or
   pane the phone is looking at; close a pane or a workspace.
2. **The rule is lifted fully.** CLAUDE.md invariant 1, improvement-guide §1
   and §9 and the README paragraph change in the first commit that adds a
   mutation, not later. "cmux is a black box" stays: everything below goes
   through documented `cmux rpc` methods, exactly as rename does.
3. **Directory for a new workspace**: the distinct current directories of the
   existing workspaces are offered as a list, plus a typed path. The bridge
   checks a typed path exists, is a directory and lies under `$HOME`. No
   directory browser in v1 (cmux advertises `workspace.directory_browse.v1`
   but exposes no public method for it; a bridge-side browser is a follow-up
   bead).
4. **Entry points on the phone**: the workspace long-press menu, a "+" on the
   sessions list, the pane picker dialog, and the terminal screen's overflow.
5. **Close is guarded on the phone only**: a confirmation names what is about
   to close; the bridge does what it is told and logs ids.
6. **Nothing created from the phone takes focus on the Mac** unless the
   phone asks for it with "Show on Mac".
7. **The preview**: choosing where a pane goes shows a miniature of the
   workspace's real pane layout with a ghost pane animating into place.

## Context

The bridge is read + terminal I/O + feed replies + rename. Every workspace or
pane the phone can look at had to be made on the Mac first. From the phone
you cannot start a second agent in a project, put a shell beside a running
agent, or clean up a finished workspace -- and when you do open something
on the phone, the Mac's screen stays wherever it was.

The rule against creating and closing was written when the phone client was
new and the bridge's blast radius was the concern. Terminal input has been
shell access all along, so *creating* a shell from the phone adds no
capability an attacker with the device did not already have. *Closing* is
new: it is the one destructive action, and it gets a confirmation.

## Verified live against cmux (2026-09-12, scratch workspace only)

Method | Params | Verified behaviour
--- | --- | ---
`workspace.create` | `cwd`, `title`, `focus` | Honours all three; `focus:false` leaves the Mac's selection alone. **With no params or `{}` it still creates** (defaults: focused window, caller's cwd). Never call it to probe.
`surface.split` | `surface_id`, `direction` (`left\|right\|up\|down`), `focus`, optional `initial_input` | Creates a new pane beside the given surface's pane, returns `{surface_id, pane_id, workspace_id}`. Missing direction → `invalid_params`.
`surface.create` | `workspace_id`, `pane_id`, `type:"terminal"`, `focus` | New tab in that pane. **With `{}` it creates a tab in the Mac's focused pane.** Never probe it.
`pane.create` | `workspace_id`, `direction` | Also splits; `surface.split` is preferred because it names the surface the phone is viewing.
`pane.list` | `workspace_id` | `container_frame {width,height}` and per pane `pixel_frame {x,y,width,height}`, `focused`, `surface_ids`, `selected_surface_id`. Frames are **all zero for a workspace never yet shown on the Mac**; once shown they stay current even while another workspace is selected (a later split updated them). `x` starts at the sidebar's width (248 here), not 0.
`workspace.select` | `workspace_id` | Switches the Mac's selected workspace. Errors on a missing id, no side effect.
`surface.focus` / `pane.focus` | `surface_id` / `pane_id` | Same shape; error on missing id.
`workspace.close` / `surface.close` | `workspace_id` / `surface_id` | Close; the workspace close takes every pane with it.
`mobile.terminal.create` | `workspace_id` | **Ignores `direction` and `pane_id`; always adds a tab to the pane focused on the Mac**, not the one the phone is viewing. Not used.
`session.restore_previous` | — | Restores the whole previous cmux session. There is no per-workspace restore RPC, so no restore action is built even though the rule no longer forbids one.

Events emitted by these: `pane.created`, `surface.created`, `surface.closed`,
`workspace.closed`. The bridge's `classify` drops them today (they are
"churn"); the app learns of layout changes when it next lists sessions or
opens the split preview. v1 keeps that; forwarding a "layout changed" frame
is listed under follow-ups.

## Design

### Wire: six bridge routes

All JSON, behind the device bearer and the e2e body envelope like every
existing route; request bodies capped at 4 KB (the envelope cap of 8 KB is
on requests only -- responses are unbounded, `/sessions` already exceeds
it). Ids are cmux UUIDs, never refs.

Route | Body | cmux | Response
--- | --- | --- | ---
`POST /sessions` | `{cwd, title?}` | `workspace.create {cwd, title, focus:false}` | `{workspace_id, surface_id}`
`POST /sessions/{id}/panes` | `{surface_id, placement}` where placement ∈ `left\|right\|up\|down\|tab` | `surface.split {surface_id, direction, focus:false}` or, for `tab`, `surface.create {workspace_id, pane_id, type:"terminal", focus:false}` with `pane_id` looked up from `pane.list` | `{surface_id, pane_id}`
`POST /sessions/{id}/select` | `{surface_id?}` | `workspace.select`, then `surface.focus` when a surface is given | `{ok}`
`DELETE /sessions/{id}` | — | `workspace.close` | `{ok}`
`DELETE /sessions/{id}/panes/{surfaceId}` | — | `surface.close` | `{ok}`
`GET /sessions/{id}/layout` | — | `pane.list` | see below

Response for the layout, normalised so the phone never sees pixels:

```json
{
  "estimated": false,
  "panes": [
    {"id": "…", "x": 0.0, "y": 0.0, "w": 0.5, "h": 1.0,
     "focused": true, "surface_ids": ["…", "…"], "selected_surface_id": "…"}
  ]
}
```

Fractions are relative to the bounding box of the panes themselves (min
origin to max extent), not to cmux's `container_frame`, so the sidebar
offset disappears. When every frame is zero (workspace never shown) the
bridge lays the panes out as equal columns in index order and sets
`estimated: true`; the phone draws that preview dashed.

Bridge rules, tested with the fake cmux client:

- A request whose target id is missing or not a UUID is a 400 **before** any
  RPC is made. The test fake asserts every `workspace.create`,
  `surface.split`, `surface.create`, `*.close` call carries an explicit
  `workspace_id` / `surface_id` -- the guard against the default-target
  behaviour that bit twice during this design.
- `cwd` for a new workspace must be absolute, exist, be a directory, and be
  inside `$HOME` after `filepath.EvalSymlinks`; otherwise 400 with a reason
  (`not_found`, `not_dir`, `outside_home`). The check runs on the Mac, so it
  is authoritative.
- `title` is optional, trimmed, capped at 120 chars; empty means cmux picks.
- Every mutation logs one line: route, workspace/surface ids, placement.
  Never the title or the cwd of a workspace (a cwd is a path on the user's
  disk; the log already avoids paths for attachments).
- cmux failure → 502 with the same generic wording the rename route uses.

### Sessions list

- A "+" in the top bar opens **New workspace**: a list of the distinct
  `cwd`s of the workspaces currently shown (most recent activity first, each
  with the workspace titles using it as a hint), a text field for another
  path, an optional title. Create → `POST /sessions` → the list refreshes
  and the new workspace's single pane opens in the terminal screen.
- The long-press menu gains, after Rename and YOLO: **New pane…**, **Show
  on Mac**, and, separated, **Close workspace…**.
- **Close workspace…** confirms with the title, the pane count and, when
  present, the attention badge ("an agent is waiting for an answer here")
  or YOLO badge. On success the row disappears immediately and the list
  refreshes.

### Pane picker dialog

The dialog shown for a workspace with several panes gets a final row,
**New pane…**, which opens the placement sheet with no current pane
selected: the ghost lands as a right split of the focused pane, and the
user can pick another pane by tapping it in the miniature.

### Terminal screen

An overflow menu (the title bar has no room for more words; the existing
`Refresh` and `Wrap` stay): **Split…**, **New tab in this pane**, **Show on
Mac**, and **Close this pane…**.

- **Show on Mac** → `POST /sessions/{id}/select {surface_id}`; the delivery
  label shows "Shown on Mac" for a moment, like the attach outcome.
- **Close this pane…** confirms with the pane title; on success the app
  navigates back to the sessions list.
- **New tab in this pane** creates without a preview (there is nothing to
  place) and switches the terminal screen to the new surface.

### The placement sheet (the effect)

A bottom sheet with a **miniature of the workspace**: each pane from
`/layout` drawn as a rounded rectangle at its fraction, the pane the phone
is viewing filled with the accent, the others outlined, each carrying its
selected surface's short title when there is room. Below it, five chips in a
row: `←  ↑  ↓  →  Tab`.

Choosing a direction animates over ~250 ms: the current pane's rectangle
shrinks to half along that axis and a **ghost pane** -- dashed outline,
translucent accent fill, a "+" in the middle -- slides in from that edge
to occupy the freed half, the way cmux itself would lay it out. Switching
chips animates from the previous ghost to the new one, not from scratch.
`Tab` draws no ghost; instead the current pane grows a small tab strip
along its top with one extra, highlighted tab. Tapping another pane in the
miniature moves the highlight and the ghost with it.

The sheet's button reads what will happen -- "Split right", "Add tab" --
and calls `POST /sessions/{id}/panes`. On success the terminal screen
opens the new surface; the sheet's preview is the last thing seen before
the real pane, so it should look like a smaller version of it.

`estimated: true` layouts draw every pane dashed and a one-line note
("layout unknown until the workspace is shown on the Mac"); the ghost still
animates, since the split's *direction* is right even if the proportions
are not.

Reduced motion: the ghost appears in place without the slide.

### Compatibility

New app against an old bridge: every new route is a 404. The app treats a
404 on any of them as "bridge too old", says so in the dialog, and hides
nothing up front (the version route returns only a string, so there is no
cheap feature probe). Old app against a new bridge: unaffected. Wire
change → next release is a minor.

## Security

- **What changes in the threat model.** A device that holds a paired bearer
  and its e2e key could already type into any terminal, which is shell
  access on the Mac. Creating a workspace or pane adds no new capability.
  Closing is new and destructive: it can end a running agent's session
  along with its scrollback. The mitigation is the phone-side confirmation
  and the fact that closing needs the same credentials as typing `exit`.
- **cwd validation on the bridge** keeps a new workspace inside `$HOME`;
  the shell that starts there inherits nothing the user's own shell would
  not.
- **No new read of the filesystem** in v1 (the directory list comes from
  workspaces cmux already reports).
- **Logs** carry ids and placements only.
- The relay stays blind: all routes are ordinary encrypted bodies.
- Both READMEs, CLAUDE.md and the improvement guide say what the bridge
  can now do, in the same commit that makes it true.

## What this does not do

- No restore of any kind (no per-workspace RPC exists).
- No moving, swapping or resizing panes; no window management.
- No browser or simulator surfaces -- terminals only.
- No live layout push; the preview fetches `/layout` when it opens.
- No "Mac follows phone" auto-select; "Show on Mac" is explicit.
- No directory browser.

## Follow-ups (beads, not v1)

- Bridge directory browser under `$HOME` and a folder picker on the phone.
- Forward `pane.created` / `surface.closed` / `workspace.closed` as a
  lightweight event so an open placement sheet or pane picker refreshes.
- "Mac follows phone" toggle in settings.
- An `initial_input` field on the new-workspace and new-pane dialogs (cmux
  supports it on both RPCs) -- e.g. start `claude` in the new pane.

## How this gets believed

- Bridge tests with the fake cmux client for every route: happy path, missing
  id → 400 without an RPC, bad cwd reasons, cmux failure → 502, layout
  normalisation (sidebar offset removed; zero frames → estimated columns).
- App unit tests for the layout → miniature geometry (fractions, ghost rect
  per direction, tab placement) and for the dialogs' view models.
- Live, with the scratch workspace only: create → split each direction →
  tab → Show on Mac → close pane → close workspace; verify `cmux tree`
  after each step and that focus on the Mac never moved except on Show on
  Mac.
