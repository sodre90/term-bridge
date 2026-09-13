# Plan: open, split, show and close workspaces and panes from the phone

Spec: `docs/superpowers/specs/2026-09-12-workspace-layout-control-design.md`.

Three commits, each gated by the full Go and Android gates. Commit 1 is the
one that lifts the rule, so the docs change there. Live test with a scratch
workspace after commit 1 (bridge) and after commit 3 (phone), bridge
deployed to the Mac first each time.

Hazard for whoever runs this: `workspace.create` and `surface.create` create
even with no params. Probe only `select`/`focus`/`close` shapes (they error
without an id), and do every experiment inside a workspace made for it
(`cmux new-workspace --name cmux-app-scratch --cwd /tmp --focus false`),
closed by UUID at the end.

## Commit 1 — bridge routes, layout, docs

`bridge: create, split, select and close workspaces and panes from the app`

- `internal/wire/sessions.go`: `CreateWorkspaceRequest{Cwd, Title}`,
  `CreateWorkspaceResponse{WorkspaceID, SurfaceID}`,
  `CreatePaneRequest{SurfaceID, Placement}`, `CreatePaneResponse{SurfaceID,
  PaneID}`, `SelectRequest{SurfaceID}`, `Layout{Estimated, Panes
  []LayoutPane}`, `LayoutPane{ID, X, Y, W, H, Focused, SurfaceIDs,
  SelectedSurfaceID}`. Placement constants.
- `internal/server/layout.go`: `handleLayout`; `normaliseLayout(raw)` pure
  function over `pane.list` output -- bounding box of the panes, fractions,
  zero-frame fallback with `Estimated`.
- `internal/server/mutate.go` (or one file per route if they grow):
  `handleCreateWorkspace` (cwd validation: absolute, `EvalSymlinks`, under
  `$HOME`, `os.Stat` is dir; reasons `not_found`/`not_dir`/`outside_home`),
  `handleCreatePane` (placement switch; `tab` looks up the pane via
  `pane.list` first), `handleSelect`, `handleCloseWorkspace`,
  `handleCloseSurface`. UUID check on every path id before any RPC.
- `server.go` routes: `POST /sessions`, `GET /sessions/{id}/layout`,
  `POST /sessions/{id}/panes`, `POST /sessions/{id}/select`,
  `DELETE /sessions/{id}`, `DELETE /sessions/{id}/panes/{surfaceId}`.
- Fake cmux client in tests: assert an explicit target id on every
  create/close call it sees; table tests per route (happy, 400 before RPC on
  missing/malformed id, cwd reasons, 502 on RPC error); `normaliseLayout`
  tests with the live shapes captured above (sidebar offset 248, two-column
  split, the 2x2 after a downward split, all-zero frames).
- `rename.go` comment: no longer "the one deliberate mutation".
- Docs in the same commit: `CLAUDE.md` invariant 1 (drop the create/close/
  restore clause, keep black-box), `docs/improvement-guide.md` §1 and §9
  (line "Adding cmux workspace create/close/restore. Never." replaced by a
  pointer to the spec), `README.md` "The bridge performs **only** …"
  paragraph and the *What the app does* list, `bridge/README.md` route
  list, CHANGELOG *Added* + *Compatibility*.
- Kotlin DTOs for every new body/response land here too (wire lockstep),
  with `DtosTest` round-trips, even though no screen uses them yet.

Live after deploy: with the scratch workspace, curl each route through the
agent's direct listener (or `cmux rpc` comparison) and check `cmux tree`.

## Commit 2 — app: new workspace, show on Mac, close

`app: create, show and close workspaces and panes from the sessions list and the terminal`

- `BridgeClient` + `FallbackBridgeClient`: `createWorkspace`, `createPane`,
  `select`, `closeWorkspace`, `closeSurface`, `layout`.
- Sessions list: "+" top-bar action → `NewWorkspaceDialog` (recent cwds
  derived from the current workspace list, typed path, optional title);
  long-press menu items **Show on Mac**, **Close workspace…** (confirmation
  with title / pane count / attention or YOLO badge). `SessionsViewModel`
  gains the calls and an outcome flow for a snackbar.
- Terminal screen: overflow menu with **New tab in this pane**, **Show on
  Mac**, **Close this pane…**; new tab switches the screen to the returned
  surface; close navigates back.
- 404 on a new route → "bridge too old" wording in the dialog.
- Tests: view-model tests for recent-cwd derivation (distinct, ordered by
  activity), confirmation gating, navigation after close, 404 mapping.

## Commit 3 — app: split with the placement preview

`app: choose where a new pane goes with a live preview of the workspace layout`

- `ui/layout/PaneMiniature.kt`: pure geometry `MiniatureLayout(panes,
  current, placement) -> rects` (pane rects, ghost rect per direction, tab
  strip for `tab`) -- unit-tested without Compose.
- `PlacementSheet` composable: miniature drawn with `Canvas`, animated
  fractions (`animateFloatAsState` per rect edge, ~250 ms, reduced-motion
  aware), direction chips + Tab, tap-to-select pane, estimated layouts
  dashed with the note, button text "Split right" / "Add tab".
- Entry points: **Split…** in the terminal overflow (current pane
  preselected), **New pane…** in the workspace long-press menu and in the
  pane picker dialog (focused pane preselected).
- On success navigate to the new surface's terminal.
- Tests: `MiniatureLayoutTest` for each direction and tab, for an
  estimated layout, and for the live 2x2 shape from the spec.

Live on the phone: split each direction and add a tab in the scratch
workspace, watching the Mac; Show on Mac from a pane; close the pane and
then the workspace.

## Not in this plan

Directory browser, layout-changed events, Mac-follows-phone, initial
command in the new pane -- each a follow-up bead once the epic closes.
