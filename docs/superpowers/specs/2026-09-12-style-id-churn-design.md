# Every span changes every frame because cmux renumbers style ids — Design

- **Date:** 2026-09-12
- **Status:** Closed 2026-09-14 — commit 1 landed (`0696d20`); commits 2–3
  measured and not built, see `2026-09-14-row-delta-design.md` (decisions 1–2)
- **Author:** perdos
- **Bead:** `cmux-app-bly`
- **Follows:** `cmux-app-rw1` (shared-window compression), which this unblocks

## Decisions

Reviewed decision by decision on 2026-09-12; every recommendation was taken.

| # | decision | outcome |
|---|---|---|
| 1 | The problem is `style_id` churn, not scrollback volume | accepted |
| 2 | The bridge may parse cmux's span schema | accepted, with passthrough on anything unexpected and unknown span fields copied through |
| 3 | The six bail-out states below | accepted as listed |
| 4 | A negotiated `styles_appended` wire field (commit 2) | approved, name as proposed |
| 5 | The scrollback shift delta (commit 3) | deferred until commits 1–2 are measured |
| 6 | Per-frame CPU budget for canonicalisation | **under 5 ms**; anything above comes back with the number before landing |

## Context

After the 0.6.0 work — the idle-pane fingerprint gate, sticky blocks, and a
client-chosen poll interval — an open pane on a *busy* session still costs about
133 KB/s. That is measured, not estimated: with the pane closed the same bridge
sends 2 KB/s.

Instrumenting `writeTerminalFrame` showed why immediately:

```
plain=199,861 B   wire=32,471 B   ratio=6.1x   unchanged=[]
```

Twenty-five consecutive frames, all of them like that. `unchanged=[]` means the
delta encoder omitted **nothing** — every sticky block was considered changed on
every frame.

The shared DEFLATE window from `cmux-app-rw1` cannot help here and measurably
does not: DEFLATE's window is 32 KB, so on a 200 KB frame the previous frame's
matching bytes have already fallen out of it by the time the encoder reaches the
end. Measured advantage over per-frame deflate: 6.2x at 1.4 KB, 5.3x at 13.6 KB,
**1.0x at 140 KB**.

## Where the bytes go

One live grid, 327 KB of raw JSON from `mobile.terminal.replay`:

| block | bytes | share |
|---|---:|---:|
| `scrollback_spans` | 111,587 | 63% |
| `row_spans` | 47,104 | 26% |
| `styles` | 12,331 | 7% |
| `terminal_theme` + `terminal_config_theme` | 5,450 | 3% |
| `modes` | 1,126 | 1% |

## The actual cause

The bead's premise — "scrollback is too big" — is wrong, and the truth is more
tractable.

**Scrollback only changes when the pane genuinely scrolls.** Across 19
consecutive transitions it was byte-identical in 18 and correctly omitted by the
existing sticky machinery. The sticky design works.

**When it does change, the change is a pure one-row shift.** Comparing rows by
text and column with a one-row offset: **201/201 rows match**. No mid-row edits.

**But byte-wise, nothing is reusable at all** — common prefix 0 spans, common
suffix 0 spans, out of 1401. The reason is visible in the per-span diff:

```
row 1->0  span0: diff={'row': (1, 0), 'style_id': (3, 1)}   text='  7 +'
row 2->1  span0: diff={'row': (2, 1), 'style_id': (6, 4)}   text=' '
row 6->5  span0: diff={'row': (6, 5), 'style_id': (12, 10)} text='  7 -'
```

`row` shifting is expected. **`style_id` shifting is the problem.** cmux numbers
styles in the order it first meets them scanning the grid, and rebuilds that
table on every replay. Scrolling one row changes what comes first, so the ids
are reassigned even though the styles are not: on the observed scroll, 47 of 48
entries had identical content, only 23 kept their id, and the rest moved by −2
(with two jumping 1→15 and 2→16 — a reorder, not a compaction). Every one of the
~1400 scrollback spans and ~600 row spans carries an id, so all of them change
bytes.

The one exception is id 0: cmux keeps its default style there on every frame
observed, and the app depends on that (blank cells are filled with style 0 and
the grid's default background is read from it). The bridge must therefore pin
its own id 0 to cmux's id 0, and treat a frame whose id 0 is a different style
as a table reset rather than remapping it.

That single renumbering invalidates 158 KB of otherwise-identical data. No
byte-level delta and no compressor can see through it.

### Two supporting facts that shape the design

**`history_rows` is the scroll amount, for free.** On the observed scroll,
`history_rows` advanced by exactly 1 and the content offset was exactly 1. No
offset search is needed — read the difference.

**`style_id` appears in exactly two places:** `row_spans[]` and
`scrollback_spans[]`. The cursor carries `style: "block"`, a shape, not an id.
Canonicalisation has two arrays to rewrite and no hidden third.

**One entry of `styles` genuinely churns.** Its foreground cycles `#E69575` →
`#E99A7A` → `#DA7C5C` frame after frame — an animated gradient in the agent's
own UI. So `styles` has real new content most frames and will never be
byte-stable. Canonicalisation alone will not make it sticky.

## Decision: give style ids a stable identity on the bridge

Per socket, keep a map from style **content** to a bridge-assigned id, and a
table that only ever grows. On each frame, rewrite `style_id` in `row_spans` and
`scrollback_spans` to the bridge's ids and emit `styles` as the bridge's table.

The app needs no change for this. It only ever looks `style_id` up in whatever
table the frame carries; it does not care who numbered it.

What this unlocks, in order of value:

1. **Scrollback stops churning.** With stable ids, a scrollback that did not
   scroll is byte-identical again and the existing sticky machinery omits it —
   111 KB per frame, on the frames where the pane is not scrolling.
2. **The styles table becomes append-only.** Because the bridge's table only
   grows, "entries added since the last frame" is a natural delta: 12 KB → a few
   hundred bytes. This is what handles the animated-colour entry.
3. **Row spans get much smaller on the wire.** They still change — they are the
   visible screen — but without id churn they compress far better, and the frame
   drops under DEFLATE's 32 KB window, at which point `cmux-app-rw1`'s shared
   window starts paying off here too instead of returning 1.0x.
4. **A scrollback shift delta becomes possible**, keyed on Δ`history_rows`:
   "drop N rows from the top, append these N". Only worth building after 1–3 are
   measured, since 1–3 may already be enough.

### Why not the alternatives

**Compress harder.** Already refuted by measurement: the window is 32 KB and the
frame is 200 KB. A bigger window would mean abandoning DEFLATE for zstd on both
sides, which is a far larger change than fixing the churn, and it would only
paper over sending 158 KB of unchanged data.

**Ask cmux for stable ids.** cmux is a black box (invariant 1). Not available.

**Diff the spans generically.** A structural diff would work but has to parse and
compare 1400 spans per frame to rediscover what Δ`history_rows` states directly.

## The cost, stated plainly

Today the bridge treats the grid as opaque below its top-level keys — `strip()`
and `gridFingerprint()` both decode one shallow level and leave every value as
`json.RawMessage`. This design breaks that: the bridge must parse cmux's span
schema and re-serialise it. That is **new coupling to a black box's output
format**, and it is the main thing to weigh against the saving.

Two mitigations, both already the house style:

- **Passthrough on anything unexpected.** Any parse failure, any span missing
  the fields we expect, any shape we do not recognise: emit the grid unchanged,
  exactly as `strip()` does today. A redundant frame costs bytes; a mangled one
  costs correctness.
- **Fields we do not understand are copied, not dropped.** Rewrite `style_id`
  and leave every other key on a span untouched, so a cmux that adds a field
  keeps working.

The per-frame CPU cost of parsing ~160 KB of spans at 4 fps must be measured on
the user's Mac before this is called done, not assumed.

## When the delta path must bail out

These are grid states this design has **not** observed and must therefore treat
conservatively — each one forces a full, un-canonicalised frame and a reset of
the per-socket table:

| field | condition | why |
|---|---|---|
| `render_epoch` | changed | pane was reset; nothing carries over |
| `scrollback_rows` | changed | resize; row numbering is no longer comparable |
| `cleared_rows` | non-empty | rows were cleared in place, not scrolled |
| `scrolled_rows` | non-zero | viewport moved independently of history |
| `anchor` | not `viewport` | user panned up; scrollback is not tracking the tail |
| `active_screen` | not `primary` | alternate screen (vim, less); scrollback frozen |

All six were constant across every frame captured (`anchor=viewport`,
`scrolled_rows=0`, `cleared_rows=[]`, `active_screen=primary`,
`scrollback_rows=240`, one `render_epoch`), which is precisely why they are
listed as unmeasured rather than as handled. The bail-out is cheap; discovering
one of them live is not.

## What this does not fix

- **The first frame of a pane.** A reconnect starts with an empty table and pays
  full price by definition. That is what makes a reconnect a clean resync and it
  should stay that way.
- **`row_spans` volume.** The visible screen genuinely changes. It gets cheaper
  here, not free.
- **The reconnect burst** (~120 KB/s for ~30 s, still unexplained) is a separate
  bead and is not addressed.

## How this gets believed

The same instrumentation that found the problem, which means a before/after that
cannot be argued with:

1. A temporary `slog` line in `writeTerminalFrame` reporting `plain`, `wire` and
   `unchanged` per frame. Before: `plain≈200,000  wire≈32,500  unchanged=[]`.
   After, on a pane that is not scrolling, `unchanged` must name
   `scrollback_spans`, and `plain` must fall by roughly the 111 KB that block
   costs.
2. `nettop` on the bridge with one pane open, against the 133 KB/s already
   recorded, with the pane-closed 2 KB/s as the floor. Hold the pane and the
   activity level fixed; this session has already been burned twice by measuring
   two different workloads and comparing them.
3. A cross-language test that a grid rewritten by the bridge renders identically
   in the app — the ids change, so the rendered output is the only thing that
   may not.
