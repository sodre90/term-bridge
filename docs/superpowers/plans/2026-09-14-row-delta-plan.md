# Plan: send only the visible rows that changed

Design: `docs/superpowers/specs/2026-09-14-row-delta-design.md`.
Issue: `cmux-app-bly`.

One wire-format commit (Go and Kotlin together, invariant 3), then a
measurement, then docs. Nothing else from the 2026-09-12 plan is built.

## Commit 1 — `rows_changed` on both sides

Bridge:
- `internal/wire/terminal.go`: `RowsChanged []int` (`rows_changed,omitempty`)
  on `TerminalDown`, documented next to `Unchanged`.
- `internal/server/terminal.go`: `?rows=1` read alongside `?delta=1` and
  logged on connect; `deltaEncoder` gains a per-row record when asked for,
  primed by the replay frame like the sticky blocks; `strip` returns the
  changed rows. `row_spans` joins the sticky names only on this path.
- Tests: a one-row change yields that row's spans and `rows_changed=[n]`; a
  row that emptied is listed with no spans; an unchanged screen names
  `row_spans` in `unchanged`; an unreadable block goes out whole and resets
  the record; a socket that did not ask never sees the field; the replay
  frame primes.

App:
- `model/Dtos.kt`: `rowsChanged: List<Int>` on `TerminalDown`,
  `UnchangedBlock.ROW_SPANS`.
- `model/RenderGrid.kt`: `mergedOnto` replaces the named rows and keeps the
  rest; carries `row_spans` whole when named in `unchanged`.
- `data/TerminalSocket.kt`: `rows=1` on the URL.
- Tests: `DeltaGridMergeTest` for replace/keep/clear/carry; the fixture-based
  cross-language check that a merged frame renders as the whole one.

Gates: Go and Android (incl. detekt).

## Measurement

Same pane, same instrumentation as the design's table; quote `plain`/`wire`
per frame and `nettop` open/closed. Remove the instrumentation before
anything lands; record the numbers on the bead.

## Docs

`bridge/README.md` (API notes on the terminal socket's negotiated
capabilities), `CHANGELOG.md`, and close the 2026-09-12 plan's commits 2–3 as
not built, with the numbers.
