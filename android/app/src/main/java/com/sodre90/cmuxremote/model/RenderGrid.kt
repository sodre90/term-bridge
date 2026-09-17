package com.sodre90.cmuxremote.model

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.intOrNull

/**
 * The `cmux.render-grid.v1` terminal snapshot: a sparse, style-run encoding of
 * the visible screen rather than raw PTY bytes. Reconstruct a dense grid with
 * [RenderGridDecoder.decode].
 */
@Serializable
data class RenderGrid(
    val format: String? = null,
    val columns: Int = 0,
    val rows: Int = 0,
    val cursor: Cursor? = null,
    // cmux sends modes as objects ({"ansi":bool,"code":int,"on":bool}); kept as
    // raw JSON since rendering never reads them. A wrong value type here aborts
    // the whole frame parse, so stay tolerant.
    val modes: List<JsonElement> = emptyList(),
    @SerialName("row_spans") val rowSpans: List<RowSpan> = emptyList(),
    @SerialName("scrollback_spans") val scrollbackSpans: List<RowSpan> = emptyList(),
    val styles: List<Style> = emptyList(),
    @SerialName("scrollback_rows") val scrollbackRows: Int = 0,
    @SerialName("active_screen") val activeScreen: String? = null,
    val full: Boolean = false,
    @SerialName("state_seq") val stateSeq: Long = 0L,
)

/**
 * Completes a delta frame from the frame before it.
 *
 * The bridge leaves out render-grid blocks it has already sent unchanged --
 * scrollback alone is 143KB of a 187KB frame -- and names them in
 * [TerminalDown.unchanged]. Each named block is taken from [previous] instead.
 *
 * Absent is not the same as unchanged, which is why the names are carried
 * explicitly: an empty scrollback arrives as an absent field too, so a grid
 * whose scrollback genuinely cleared must still be able to say so.
 *
 * The visible screen is completed row by row: [rowsChanged] names the rows
 * this frame's `row_spans` carries, and every other row keeps its previous
 * spans -- a named row with no spans has emptied. A screen named unchanged as
 * a whole is carried like any other block.
 *
 * With no previous grid to draw on -- the first frame, or a frame after a
 * reconnect -- this returns the grid as it arrived. That is safe because the
 * bridge only omits blocks after a full replay it knows the client received,
 * and a reconnect always starts with a fresh full replay.
 */
internal fun RenderGrid.mergedOnto(
    previous: RenderGrid?,
    unchanged: List<String>,
    rowsChanged: List<Int> = emptyList(),
): RenderGrid {
    if (previous == null || (unchanged.isEmpty() && rowsChanged.isEmpty())) return this
    var merged = this
    if (UnchangedBlock.ROW_SPANS in unchanged) {
        merged = merged.copy(rowSpans = previous.rowSpans)
    } else if (rowsChanged.isNotEmpty()) {
        val replaced = rowsChanged.toHashSet()
        merged = merged.copy(rowSpans = previous.rowSpans.filterNot { it.row in replaced } + rowSpans)
    }
    if (UnchangedBlock.SCROLLBACK_SPANS in unchanged) {
        // Only the spans are carried over: the bridge sends scrollback_rows on
        // every frame, so this frame's own count is the current one.
        merged = merged.copy(scrollbackSpans = previous.scrollbackSpans)
    }
    if (UnchangedBlock.STYLES in unchanged) {
        merged = merged.copy(styles = previous.styles)
    }
    // Carried for the same reason as the rest, but it is the one block with a
    // consequence beyond drawing: modes decides whether the arrow keys send
    // application-cursor sequences (see [applicationCursorKeysEnabled]).
    if (UnchangedBlock.MODES in unchanged) {
        merged = merged.copy(modes = previous.modes)
    }
    return merged
}

@Serializable
data class Cursor(
    val row: Int = 0,
    val column: Int = 0,
    val style: String? = null,
    val visible: Boolean = true,
    val blinking: Boolean = false,
)

/** One style-run of text on a row, starting at [column]. */
@Serializable
data class RowSpan(
    val row: Int = 0,
    val column: Int = 0,
    @SerialName("cell_width") val cellWidth: Int = 0,
    @SerialName("style_id") val styleId: Int = 0,
    val text: String = "",
)

/**
 * A referenced style. Colors are kept as raw JSON because cmux may encode them
 * as hex/name strings or palette ints depending on the source; [foregroundString]
 * / [backgroundString] extract a string form when present.
 */
@Serializable
data class Style(
    val id: Int = 0,
    val foreground: JsonElement? = null,
    val background: JsonElement? = null,
    val bold: Boolean = false,
    val faint: Boolean = false,
    val italic: Boolean = false,
    val underline: Boolean = false,
    val inverse: Boolean = false,
    val strikethrough: Boolean = false,
) {
    val foregroundString: String? get() = foreground.asStringOrNull()
    val backgroundString: String? get() = background.asStringOrNull()
}

private fun JsonElement?.asStringOrNull(): String? =
    (this as? JsonPrimitive)?.contentOrNull

/** A single reconstructed terminal cell. */
data class Cell(val char: Char, val styleId: Int)

/** A reconstructed terminal row, exactly [DecodedGrid.columns] cells wide. */
data class DecodedLine(val cells: List<Cell>) {
    val text: String get() = buildString { cells.forEach { append(it.char) } }
}

/** A dense, render-ready grid produced from a [RenderGrid]. */
data class DecodedGrid(
    val columns: Int,
    val rows: Int,
    val lines: List<DecodedLine>,
    val cursor: Cursor?,
    val scrollbackLines: List<DecodedLine> = emptyList(),
    // DECCKM state, so the key bar can send the right arrow/Home/End encoding.
    val applicationCursorKeys: Boolean = false,
    // Mouse-reporting state (DECSET 1000/1002/1003): when on, the TUI wants
    // scroll input forwarded to it instead of the client scrolling its own
    // buffer -- opencode runs this way, which is why its PTY scrollback stays
    // empty. See [mayPageOnOverscroll].
    val mouseReporting: Boolean = false,
    // The pane is on the alternate screen buffer (DECSET 1049) rather than the
    // primary one. See [mayPageOnOverscroll].
    val alternateScreen: Boolean = false,
) {
    /**
     * True when a vertical swipe the local render buffer could not absorb is
     * worth offering to the PANE as a page key.
     *
     * Deliberately a guess, and deliberately a cheap one. Neither signal proves
     * anything is listening -- both only say the pane is the KIND of pane that
     * usually pages:
     *  - mouse reporting is on, so the pane asked for scroll input the way a
     *    trackpad over cmux itself delivers it -- to the application, not the
     *    viewport;
     *  - the pane is on the alternate screen, which has no scrollback by
     *    definition, so there is nothing local beyond the visible grid. This
     *    covers `less` and `vim` started without mouse support.
     *
     * The counterexample that named this: an idle zsh prompt stranded on the
     * alternate screen after Claude Code exited without restoring the primary
     * buffer (cmux-app-4yi). It matches the second clause and pages on nothing.
     * That is survivable only because this gates the OVERSCROLL alone -- local
     * panning happens first and is never given up -- so a wrong guess costs a
     * key the pane ignores, not a pane the user cannot move. An earlier version
     * claimed the whole drag on that guess, and every alt-screen pane became
     * unpannable.
     */
    val mayPageOnOverscroll: Boolean get() = mouseReporting || alternateScreen
}

/**
 * True when DECCKM (DEC private mode 1, "application cursor keys") is set: arrow
 * and Home/End keys must then be sent as SS3 (`ESC O x`) rather than CSI
 * (`ESC [ x`). cmux reports each mode as `{"ansi":bool,"code":int,"on":bool}`,
 * where a DEC-private mode carries `ansi=false`; we want mode 1 present and on.
 */
internal fun applicationCursorKeysEnabled(modes: List<JsonElement>): Boolean =
    modes.any { element ->
        val obj = element as? JsonObject ?: return@any false
        val ansi = (obj["ansi"] as? JsonPrimitive)?.booleanOrNull ?: false
        val code = (obj["code"] as? JsonPrimitive)?.intOrNull
        val on = (obj["on"] as? JsonPrimitive)?.booleanOrNull ?: false
        !ansi && code == 1 && on
    }

/**
 * True when the TUI has requested mouse reporting (DEC private modes 1000
 * normal tracking, 1002 button-event, or 1003 any-event): vertical swipes must
 * be forwarded to the application rather than scrolling local scrollback. This
 * mirrors how a trackpad behaves over such panes in cmux itself -- the wheel
 * goes to the application, not the terminal viewport.
 */
internal fun mouseReportingEnabled(modes: List<JsonElement>): Boolean =
    modes.any { element ->
        val obj = element as? JsonObject ?: return@any false
        val ansi = (obj["ansi"] as? JsonPrimitive)?.booleanOrNull ?: false
        val code = (obj["code"] as? JsonPrimitive)?.intOrNull
        val on = (obj["on"] as? JsonPrimitive)?.booleanOrNull ?: false
        !ansi && (code == 1000 || code == 1002 || code == 1003) && on
    }

/**
 * The `active_screen` value cmux reports for a pane that has swapped to the
 * alternate screen buffer; the primary buffer reports "primary".
 */
private const val ALTERNATE_SCREEN = "alternate"

object RenderGridDecoder {
    private const val BLANK = ' '

    /** Expand the sparse [grid] into a dense [DecodedGrid] (visible + scrollback). */
    fun decode(grid: RenderGrid): DecodedGrid {
        val cols = grid.columns.coerceAtLeast(0)
        val rowCount = grid.rows.coerceAtLeast(0)
        val lines = layout(grid.rowSpans, rowCount, cols)

        val sbRows = grid.scrollbackRows.coerceAtLeast(0)
        val scrollback =
            if (sbRows == 0 || grid.scrollbackSpans.isEmpty()) {
                emptyList()
            } else {
                layout(grid.scrollbackSpans, sbRows, cols)
            }

        return DecodedGrid(
            cols,
            rowCount,
            lines,
            grid.cursor,
            scrollback,
            applicationCursorKeys = applicationCursorKeysEnabled(grid.modes),
            mouseReporting = mouseReportingEnabled(grid.modes),
            alternateScreen = grid.activeScreen == ALTERNATE_SCREEN,
        )
    }

    /**
     * Lay [spans] onto a dense [rowCount] x [cols] grid of cells (width-1 chars).
     *
     * A span's `cell_width` is the number of terminal columns its whole `text`
     * run occupies; cmux only groups multiple characters into one span when
     * they're all narrow (width 1), so `cell_width` is only meaningful as a
     * per-character width when the span carries exactly one Unicode codepoint —
     * that single glyph may be a wide CJK/emoji character (2 columns) or an
     * astral one needing a UTF-16 surrogate pair (2 `Char`s). Iterating by
     * codepoint (not `Char`) keeps a surrogate pair intact and in order; any
     * extra declared width beyond what the codepoint's `Char`s need is a blank
     * filler cell, so a later span's declared `column` still lines up.
     */
    private fun layout(spans: List<RowSpan>, rowCount: Int, cols: Int): List<DecodedLine> {
        val cells = Array(rowCount) { Array(cols) { Cell(BLANK, 0) } }
        for (span in spans) {
            if (span.row < 0 || span.row >= rowCount) continue
            val codepoints = span.text.codePoints().toArray()
            val singleWidth = if (codepoints.size == 1) span.cellWidth.coerceAtLeast(1) else 1
            var col = span.column
            for (cp in codepoints) {
                if (col >= cols) break
                val chars = Character.toChars(cp)
                // Never fewer columns than the codepoint needs Chars for, even if
                // cell_width under-declares it (e.g. a mis-tagged narrow-astral char).
                val width = maxOf(singleWidth, chars.size)
                if (col >= 0) {
                    for (i in chars.indices) {
                        val c = col + i
                        if (c < cols) cells[span.row][c] = Cell(chars[i], span.styleId)
                    }
                    for (c in (col + chars.size) until (col + width).coerceAtMost(cols)) {
                        cells[span.row][c] = Cell(BLANK, span.styleId)
                    }
                }
                col += width
            }
        }
        return cells.map { DecodedLine(it.toList()) }
    }
}
