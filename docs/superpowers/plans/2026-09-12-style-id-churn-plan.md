# Plan: give style ids a stable identity on the bridge

Design: `docs/superpowers/specs/2026-09-12-style-id-churn-design.md`.
Issue: `cmux-app-bly`.

Four commits, ordered so that each one is separately measurable and each can be
abandoned without stranding the ones before it. The first is the whole idea; the
rest only pay off because of it.

All of this is bridge-side. The app's `RenderGridDecoder` already looks
`style_id` up in the table the frame carries, so commits 1–3 need no Kotlin
change — which is exactly why commit 1 leads with the test that proves it.

## Commit 0 (prerequisite) — decide `cmux-app-rw1`

`cmux-app-rw1`'s streaming compression is uncommitted and lives in the same
file. It lands (or is reverted) on its own before any of this starts, so
one-logical-item-per-commit stays honourable.

## Commit 1 — canonicalise `style_id` per socket

`internal/server/terminal.go` (new `styleTable` type), plus tests.

Per socket, a map from a style entry's canonical JSON to a bridge-assigned id,
and a slice holding the table in id order. On each frame: walk `styles` to build
cmux-id → bridge-id, then rewrite `style_id` on every span in `row_spans` and
`scrollback_spans`, and replace `styles` with the bridge's table.

The table only grows. It is per socket and never shared; a reconnect starts
empty, which is what keeps a reconnect a clean resync.

Bail out to the unmodified grid — and reset the table — on any of the six
conditions in the design's bail-out table, on any JSON parse failure, and on any
span whose `style_id` is not an integer or not present in `styles`.

Tests:
- A grid whose ids cmux renumbered, but whose content did not change, comes out
  **byte-identical two frames running**. This is the regression the whole design
  exists for; it fails today.
- Spans keep every field the bridge does not understand.
- Each bail-out condition returns the grid untouched and clears the table.
- A style entry that is genuinely new gets a genuinely new id, and an entry that
  reappears after dropping out of cmux's table gets its **original** id back.
- Cross-language: the app renders a canonicalised grid identically to the
  original. Ids change, so rendered output is the only invariant left, and this
  is the one test that spans both languages (invariant 3).

Acceptance: the temporary `writeTerminalFrame` log shows `unchanged` naming
`scrollback_spans` on a pane that is not scrolling, where today it names nothing.

## Commit 2 — send only styles that are new

**Not built (2026-09-14).** Measured after commit 1 and the shared window
(`4add77a`): `styles` was already named in `unchanged` on 248 of 256 frames, so
the table costs nothing on 97% of frames. What remained was `row_spans`, which
`2026-09-14-row-delta-plan.md` addressed instead (24.6 KB/s → ~4 KB/s).

`internal/server/terminal.go`, `internal/wire/terminal.go`, `model/Dtos.kt`,
`data/e2e` decode path, plus tests on both sides.

Because the table only grows, a frame can carry the entries added since the last
one instead of all 48. Needs a wire field (`styles_appended`, alongside the
existing `unchanged` names) and therefore lands on **all** copies in the same
commit, Go and Kotlin, with tests each side — invariant 3.

Negotiated, like `delta` and `stream` before it: an app that did not ask keeps
getting whole tables, or it would render a pane against a table it only has half
of.

This is what handles the animated-colour entry that will never be sticky.
Expected 12 KB → a few hundred bytes per frame.

## Commit 3 — measure, then decide on the scrollback shift delta

**Not built (2026-09-14).** Measured: the scrollback block was re-sent on 3 of
256 frames over 41 s of a working pane, ~1.5 KB/s. Not worth a second source of
truth next to the grid; revisit only if a scrolling pane is measured expensive.

Do not write this until commits 1 and 2 are measured.

If a scrolling pane is still expensive, add the shift delta keyed on
Δ`history_rows`: "drop N rows from the top, append these N". The design shows
the shift is exact (201/201 rows) and that `history_rows` states it directly, so
this is mechanical — but it is also the only commit here that puts a second
source of truth next to the grid, and it should not be paid for unless the
measurement asks for it.

## Measurement protocol

Carried from the design, and from two mistakes already made in this workstream:

- **Hold the pane and the workload fixed.** Two measurements of different panes
  are not a before/after. The 36x and the 90% claims in this workstream both
  came from generalising one pane's numbers to another's.
- **Quote what was measured, not what was inferred.** `nettop` on the bridge,
  pane open vs pane closed, against the recorded 133 KB/s and 2 KB/s.
- Re-run the per-frame `slog` instrumentation for the `plain`/`wire`/`unchanged`
  triple; remove it before the commit lands.
