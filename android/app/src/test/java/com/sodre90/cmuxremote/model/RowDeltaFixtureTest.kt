package com.sodre90.cmuxremote.model

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Test

/**
 * A `rows=1` socket gets the visible screen row by row: the frame carries only
 * the rows that changed and names them in `rows_changed`, and this side
 * completes it from the frame before. The only thing that may not differ is
 * what gets drawn, so this renders a completed frame against the whole screen
 * it was cut from.
 *
 * All three constants are copied from the Go side's
 * TestTheCrossLanguageRowFixtureIsUnchanged. If that test fails after a bridge
 * change, regenerate these from what it prints.
 */
class RowDeltaFixtureTest {

    private val firstWhole =
        """{"columns":6,"rows":3,"styles":[{"id":0},{"id":1,"bold":true}],"row_spans":[""" +
            """{"row":0,"column":0,"style_id":0,"text":"$ ls","cell_width":1},""" +
            """{"row":1,"column":0,"style_id":1,"text":"a.txt","cell_width":1},""" +
            """{"row":2,"column":0,"style_id":0,"text":"spin |","cell_width":1}]}"""

    private val secondWhole =
        """{"columns":6,"rows":3,"styles":[{"id":0},{"id":1,"bold":true}],"row_spans":[""" +
            """{"row":0,"column":0,"style_id":0,"text":"$ ls","cell_width":1},""" +
            """{"row":2,"column":0,"style_id":0,"text":"spin /","cell_width":1}]}"""

    private val secondAsSent =
        """{"columns":6,"row_spans":[{"row":2,"column":0,"style_id":0,"text":"spin /","cell_width":1}],"rows":3}"""
    private val secondUnchanged = listOf(UnchangedBlock.STYLES)
    private val secondRowsChanged = listOf(1, 2)

    private fun grid(json: String): RenderGrid = BridgeJson.decodeFromString(RenderGrid.serializer(), json)

    @Test
    fun aFrameCompletedFromPartialRowsRendersLikeTheWholeScreen() {
        val completed = grid(secondAsSent).mergedOnto(grid(firstWhole), secondUnchanged, secondRowsChanged)
        assertEquals(RenderGridDecoder.decode(grid(secondWhole)), RenderGridDecoder.decode(completed))
        assertEquals(grid(secondWhole).styles, completed.styles)
    }

    /** The test above would pass vacuously if the frame had arrived whole. */
    @Test
    fun theFrameReallyWasPartial() {
        assertNotEquals(grid(secondWhole).rowSpans, grid(secondAsSent).rowSpans)
    }
}
