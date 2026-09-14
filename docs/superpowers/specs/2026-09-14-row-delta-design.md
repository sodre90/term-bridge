# The visible screen is re-sent whole for a one-row change — Design

- **Date:** 2026-09-14
- **Status:** Approved 2026-09-14 (user: "sounds good, approved")
- **Author:** perdos
- **Bead:** `cmux-app-bly` (continues the 2026-09-12 style-id churn design)
- **Follows:** `0696d20` (stable style ids), `4add77a` (shared DEFLATE window)

## Decisions

| # | decision | outcome |
|---|---|---|
| 1 | Commit 2 of the style-id plan (`styles_appended`) is dropped | measured worthless: styles are already sticky on 97% of frames |
| 2 | Commit 3 of the style-id plan (scrollback shift delta) is not built | measured ~1.5 KB/s; the spec said not to build it unless asked for |
| 3 | The visible `row_spans` block is sent per row, only the rows that changed | this document |
| 4 | A row is "unchanged" iff its spans' bytes equal the ones this socket last sent | the same rule as the sticky blocks; correct by construction, see *Why bytes* |
| 5 | Negotiated by the app with `?rows=1`, only alongside `?delta=1` | an app that did not ask keeps getting whole blocks |

## Context

Measured live 2026-09-14 on the same busy Claude Code pane the 2026-09-12
design was measured on, with both earlier commits deployed (emulator over
relay, 250 ms poll, per-frame `slog` on the bridge, `nettop` on its process):

| | 2026-09-12 (before) | 2026-09-14 (after style ids + shared window) |
|---|---|---|
| plain / wire per frame | 200 KB / 32.5 KB | 41.9 KB / 6.0 KB |
| `unchanged` | `[]` every frame | `scrollback_spans` 252/256, `styles` 248/256 |
| scrollback re-sent (41 s working) | every frame | 3 frames |
| bridge bytes out, pane open / closed | 133 KB/s / 2 KB/s | 24.6 KB/s / 0.33 KB/s |

So the two landed commits delivered ~5.5x, and what is left is one block:
`row_spans`, the visible screen, ~50 KB plain / ~5.7 KB wire, sent four times a
second because the agent UI's spinner changes a row or two every tick. Three
consecutive replays of that pane: 611 spans over 65 rows; between frames
**1–2 rows changed** (487 B and 4.9 KB of spans). Everything else in the frame
is already omitted.

## Decision: send only the rows that changed

Per socket, the bridge remembers the bytes of each visible row's spans as it
last sent them. On an output frame that the app negotiated for, `row_spans`
carries only the spans of rows whose bytes differ from that record — including
a row that now has no spans at all — and a new top-level frame field
`rows_changed` lists those row numbers. When no row differs, `row_spans` is
left out and named in `unchanged` like any other sticky block.

The app completes the frame from the one before it: rows named in
`rows_changed` are replaced (cleared, then filled from the frame's spans); every
other row keeps its previous spans; a `row_spans` named in `unchanged` is
carried over whole.

`rows_changed` absent means `row_spans` is whole, as today. That keeps every
older pairing working without a confirming header: a bridge that does not know
`?rows=1` never sends the field, and an app that did not ask never receives a
partial block.

### Why bytes, and why this needs no bail-out table

A row is kept only when its spans' bytes are identical to the last frame's.
The app resolves `style_id` against the `styles` table of the *current* frame,
so a kept row means exactly what the frame would have said had it carried it
— whatever the style table did meanwhile, including the style-id
canonicalisation bailing out and cmux's raw ids coming through. A resize
changes the bytes of every reflowed row, so all of them are sent; rows that
fell off the bottom arrive as changed-and-empty. There is no grid state in
which keeping a byte-identical row is wrong, which is why this design has no
list of unobserved states to refuse, unlike the style table.

What does fail the frame is a `row_spans` the bridge cannot read as an array
of objects each carrying an integer `row`: it goes out whole and the record is
reset — the same direction as `strip()` and `canonicalise()`, a redundant frame
over a wrong one.

## Expected effect

On the measured pane, 50 KB → 0.5–5 KB plain of `row_spans` per frame; with
the shared window on top, roughly 24 KB/s → a few KB/s. The first frame after a
reconnect still pays full price, by design.

## Cost

One more parse of `row_spans` per frame on the bridge (~50 KB, to group by
row); budgeted with the style table's 5 ms. One new wire field on all copies
(Go `wire.TerminalDown`, Kotlin `TerminalDown`), plus a merge step in
`RenderGrid.mergedOnto`. A lost frame already closes the socket
(`DesyncException`), so partial rows add no new failure mode.

## How this gets believed

The same instrumentation and the same pane: per-frame `plain`/`wire` with
`rows_changed` counts, then `nettop` pane open vs closed against the 24.6 KB/s
and 0.33 KB/s above. And a cross-language test that a frame merged from
partial rows renders identically to the whole grid it was cut from.
