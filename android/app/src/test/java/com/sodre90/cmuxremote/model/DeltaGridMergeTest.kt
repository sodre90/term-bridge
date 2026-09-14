package com.sodre90.cmuxremote.model

import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Test

class DeltaGridMergeTest {

    private val history = listOf(RowSpan(row = 0, text = "history"))

    private val previous = RenderGrid(
        columns = 8,
        rows = 1,
        rowSpans = listOf(RowSpan(row = 0, text = "before")),
        scrollbackSpans = history,
        scrollbackRows = 1,
    )

    /** DECCKM set, so a merge that loses [RenderGrid.modes] is visible as the
     *  arrow keys silently reverting to CSI. */
    private val applicationCursorKeys = Json.parseToJsonElement("""{"ansi":false,"code":1,"on":true}""")

    private val styled = previous.copy(
        styles = listOf(Style(id = 1), Style(id = 2, bold = true)),
        modes = listOf(applicationCursorKeys),
    )

    /** The ordinary delta frame: new visible rows, scrollback left out. */
    @Test
    fun carriesAnOmittedScrollbackForwardFromThePreviousGrid() {
        val delta = RenderGrid(
            columns = 8,
            rows = 1,
            rowSpans = listOf(RowSpan(row = 0, text = "after")),
            scrollbackRows = 1,
        )
        val merged = delta.mergedOnto(previous, listOf(UnchangedBlock.SCROLLBACK_SPANS))
        assertEquals(history, merged.scrollbackSpans)
        assertEquals("after", merged.rowSpans.single().text)
    }

    /**
     * The distinction the explicit name list exists for. A pane whose
     * scrollback genuinely cleared sends an absent block too, and must not have
     * the old history put back.
     */
    @Test
    fun leavesAClearedScrollbackClearedWhenItIsNotNamedUnchanged() {
        val cleared = RenderGrid(columns = 8, rows = 1, scrollbackRows = 0)
        val merged = cleared.mergedOnto(previous, emptyList())
        assertTrue("a cleared scrollback must stay cleared", merged.scrollbackSpans.isEmpty())
    }

    /** A replay frame names nothing, so it replaces the base wholesale --
     *  which is what makes a reconnect a clean resync. */
    @Test
    fun aFrameThatNamesNothingIsUsedAsItArrived() {
        val replay = RenderGrid(columns = 8, rows = 1, scrollbackSpans = history, scrollbackRows = 1)
        assertSame(replay, replay.mergedOnto(previous, emptyList()))
    }

    /** Nothing to merge from: the first frame on a socket. */
    @Test
    fun returnsTheFrameUnchangedWithNoPreviousGrid() {
        val first = RenderGrid(columns = 8, rows = 1)
        assertSame(first, first.mergedOnto(null, listOf(UnchangedBlock.SCROLLBACK_SPANS)))
    }

    /** The bridge sends scrollback_rows on every frame, so this frame's own
     *  count wins rather than the carried-over one. */
    @Test
    fun takesTheRowCountFromTheArrivingFrame() {
        val delta = RenderGrid(columns = 8, rows = 1, scrollbackRows = 7)
        val merged = delta.mergedOnto(previous, listOf(UnchangedBlock.SCROLLBACK_SPANS))
        assertEquals(7, merged.scrollbackRows)
    }

    /** styles is named unchanged far more often than scrollback is -- a pane's
     *  palette barely moves -- so this is the common delta path, not an edge. */
    @Test
    fun carriesAnOmittedPaletteForwardFromThePreviousGrid() {
        val delta = RenderGrid(columns = 8, rows = 1)
        val merged = delta.mergedOnto(styled, listOf(UnchangedBlock.STYLES))
        assertEquals(styled.styles, merged.styles)
    }

    /**
     * modes is the one carried block with a consequence beyond drawing: it
     * decides whether the arrow keys send application-cursor sequences. Getting
     * it wrong sends a pane bytes it never asked for.
     */
    @Test
    fun carriesOmittedModesForwardSoTheArrowKeysStayCorrect() {
        val delta = RenderGrid(columns = 8, rows = 1)
        val merged = delta.mergedOnto(styled, listOf(UnchangedBlock.MODES))
        assertEquals(styled.modes, merged.modes)
        assertTrue("application cursor keys must survive the merge", applicationCursorKeysEnabled(merged.modes))
    }

    /** Several blocks omitted at once is the ordinary case once a pane settles. */
    @Test
    fun carriesEveryNamedBlockInOneFrame() {
        val delta = RenderGrid(columns = 8, rows = 1)
        val merged = delta.mergedOnto(
            styled,
            listOf(UnchangedBlock.SCROLLBACK_SPANS, UnchangedBlock.STYLES, UnchangedBlock.MODES),
        )
        assertEquals(history, merged.scrollbackSpans)
        assertEquals(styled.styles, merged.styles)
        assertEquals(styled.modes, merged.modes)
    }

    /** The per-row path: the frame carries only the rows it names, the rest
     *  keep their previous spans. */
    @Test
    fun replacesOnlyTheRowsNamedChanged() {
        val twoRows = previous.copy(
            rows = 2,
            rowSpans = listOf(RowSpan(row = 0, text = "prompt"), RowSpan(row = 1, text = "spin |")),
        )
        val delta = RenderGrid(columns = 8, rows = 2, rowSpans = listOf(RowSpan(row = 1, text = "spin /")))
        val merged = delta.mergedOnto(twoRows, emptyList(), rowsChanged = listOf(1))
        assertEquals(listOf("prompt", "spin /"), merged.rowSpans.sortedBy { it.row }.map { it.text })
    }

    /** A named row that arrives with no spans has emptied and must not keep
     *  its old text. */
    @Test
    fun clearsANamedRowThatArrivedWithoutSpans() {
        val twoRows = previous.copy(
            rows = 2,
            rowSpans = listOf(RowSpan(row = 0, text = "prompt"), RowSpan(row = 1, text = "gone")),
        )
        val delta = RenderGrid(columns = 8, rows = 2)
        val merged = delta.mergedOnto(twoRows, emptyList(), rowsChanged = listOf(1))
        assertEquals(listOf("prompt"), merged.rowSpans.map { it.text })
    }

    /** A screen named unchanged as a whole is carried like any other block. */
    @Test
    fun carriesTheWholeScreenWhenItIsNamedUnchanged() {
        val delta = RenderGrid(columns = 8, rows = 1)
        val merged = delta.mergedOnto(previous, listOf(UnchangedBlock.ROW_SPANS))
        assertEquals(previous.rowSpans, merged.rowSpans)
    }

    /** Absent rows_changed on a frame that names nothing is a whole screen,
     *  as it always was. */
    @Test
    fun aFrameWithoutRowsChangedReplacesTheScreen() {
        val delta = RenderGrid(columns = 8, rows = 1, rowSpans = listOf(RowSpan(row = 0, text = "after")))
        assertSame(delta, delta.mergedOnto(previous, emptyList()))
    }

    /** A block the bridge did NOT name must keep this frame's own value, even
     *  when a sibling block was carried over. */
    @Test
    fun leavesAnUnnamedBlockAloneWhileCarryingANamedOne() {
        val ownStyles = listOf(Style(id = 9))
        val delta = RenderGrid(columns = 8, rows = 1, styles = ownStyles)
        val merged = delta.mergedOnto(styled, listOf(UnchangedBlock.MODES))
        assertEquals("styles was not named unchanged, so it must not be replaced", ownStyles, merged.styles)
        assertEquals(styled.modes, merged.modes)
    }

    /** An unrecognised block name from a newer bridge must not be guessed at. */
    @Test
    fun ignoresABlockNameItDoesNotKnow() {
        val delta = RenderGrid(columns = 8, rows = 1)
        val merged = delta.mergedOnto(previous, listOf("something_new"))
        assertTrue(merged.scrollbackSpans.isEmpty())
    }
}
