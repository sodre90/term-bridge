package com.sodre90.cmuxremote.model

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class RenderGridDecoderTest {

    private val sample = """
        {
          "format":"cmux.render-grid.v1",
          "columns":10,"rows":2,
          "cursor":{"row":1,"column":3,"visible":true},
          "styles":[
            {"id":0},
            {"id":1,"bold":true,"foreground":"#ff8800","weird_future":true}
          ],
          "row_spans":[
            {"row":0,"column":0,"cell_width":5,"style_id":0,"text":"hello"},
            {"row":1,"column":2,"cell_width":3,"style_id":1,"text":"git"}
          ],
          "state_seq":42
        }
    """.trimIndent()

    private fun grid() = BridgeJson.decodeFromString(RenderGrid.serializer(), sample)

    @Test
    fun decodesTextAtCorrectColumns() {
        val decoded = RenderGridDecoder.decode(grid())
        assertEquals(2, decoded.rows)
        assertEquals(10, decoded.columns)
        assertEquals("hello     ", decoded.lines[0].text)
        assertEquals("  git     ", decoded.lines[1].text)
    }

    @Test
    fun carriesStyleIdsPerCell() {
        val decoded = RenderGridDecoder.decode(grid())
        assertEquals('g', decoded.lines[1].cells[2].char)
        assertEquals(1, decoded.lines[1].cells[2].styleId)
        assertEquals(0, decoded.lines[1].cells[0].styleId)
    }

    @Test
    fun parsesStyleFlagsAndColorIgnoringUnknownKeys() {
        val s1 = grid().styles.first { it.id == 1 }
        assertTrue(s1.bold)
        assertEquals("#ff8800", s1.foregroundString)
    }

    @Test
    fun preservesCursorPosition() {
        val decoded = RenderGridDecoder.decode(grid())
        assertEquals(1, decoded.cursor?.row)
        assertEquals(3, decoded.cursor?.column)
    }

    @Test
    fun clampsSpansThatOverflowColumns() {
        val js =
            """{"columns":4,"rows":1,"row_spans":[{"row":0,"column":2,"cell_width":5,"style_id":0,"text":"abcdef"}]}"""
        val decoded = RenderGridDecoder.decode(BridgeJson.decodeFromString(RenderGrid.serializer(), js))
        assertEquals("  ab", decoded.lines[0].text)
    }

    @Test
    fun decodesScrollbackLinesBeforeVisible() {
        val js = """
            {"columns":5,"rows":1,"scrollback_rows":2,
             "scrollback_spans":[
               {"row":0,"column":0,"style_id":0,"text":"old1"},
               {"row":1,"column":0,"style_id":0,"text":"old2"}],
             "row_spans":[{"row":0,"column":0,"style_id":0,"text":"live"}]}
        """.trimIndent()
        val d = RenderGridDecoder.decode(BridgeJson.decodeFromString(RenderGrid.serializer(), js))
        assertEquals(2, d.scrollbackLines.size)
        assertEquals("old1 ", d.scrollbackLines[0].text)
        assertEquals("old2 ", d.scrollbackLines[1].text)
        assertEquals("live ", d.lines[0].text)
    }

    @Test
    fun scrollbackEmptyWhenAbsent() {
        val d = RenderGridDecoder.decode(grid())
        assertTrue(d.scrollbackLines.isEmpty())
    }

    @Test
    fun wideCharacterReservesTwoColumnsForNextSpan() {
        // A single wide CJK glyph ("文", cell_width 2) must occupy two grid
        // columns with a blank filler, so the next span's declared column
        // (2) still lands on the right cell instead of overlapping.
        val js = """
            {"columns":6,"rows":1,"row_spans":[
              {"row":0,"column":0,"cell_width":2,"style_id":0,"text":"文"},
              {"row":0,"column":2,"cell_width":1,"style_id":0,"text":"X"}
            ]}
        """.trimIndent()
        val d = RenderGridDecoder.decode(BridgeJson.decodeFromString(RenderGrid.serializer(), js))
        assertEquals('文', d.lines[0].cells[0].char)
        assertEquals(' ', d.lines[0].cells[1].char) // wide glyph's second column
        assertEquals('X', d.lines[0].cells[2].char)
        assertEquals("文 X   ", d.lines[0].text)
    }

    @Test
    fun astralCodepointKeepsSurrogatePairIntact() {
        // An emoji outside the BMP is 2 UTF-16 Chars for 1 codepoint; iterating
        // by Char (the old bug) would split the pair across cells. Both halves
        // must land in adjacent cells, in order, so the row's text reconstructs
        // a valid, renderable string.
        val js = """
            {"columns":4,"rows":1,"row_spans":[
              {"row":0,"column":0,"cell_width":2,"style_id":0,"text":"😀"}
            ]}
        """.trimIndent()
        val d = RenderGridDecoder.decode(BridgeJson.decodeFromString(RenderGrid.serializer(), js))
        assertEquals("😀".codePointAt(0), Character.toCodePoint(d.lines[0].cells[0].char, d.lines[0].cells[1].char))
        assertEquals("😀  ", d.lines[0].text)
    }

    private fun decodeWithModes(modes: String) = RenderGridDecoder.decode(
        BridgeJson.decodeFromString(
            RenderGrid.serializer(),
            """{"columns":1,"rows":1,"modes":$modes}""",
        ),
    )

    @Test
    fun detectsApplicationCursorKeysFromDecckmOn() {
        assertTrue(decodeWithModes("""[{"ansi":false,"code":1,"on":true}]""").applicationCursorKeys)
    }

    @Test
    fun decckmResetMeansNormalCursorKeys() {
        assertFalse(decodeWithModes("""[{"ansi":false,"code":1,"on":false}]""").applicationCursorKeys)
    }

    @Test
    fun ansiModeOneIsNotDecckm() {
        // ANSI mode 1 is distinct from DEC-private mode 1 (DECCKM); only the latter counts.
        assertFalse(decodeWithModes("""[{"ansi":true,"code":1,"on":true}]""").applicationCursorKeys)
    }

    @Test
    fun otherDecModesDoNotEnableApplicationCursorKeys() {
        assertFalse(decodeWithModes("""[{"ansi":false,"code":25,"on":true}]""").applicationCursorKeys)
    }

    @Test
    fun noModesMeansNormalCursorKeys() {
        assertFalse(RenderGridDecoder.decode(grid()).applicationCursorKeys)
    }

    // -- mouse reporting (see mouseReportingEnabled): one of the two reasons a
    //    pane owns its own scrolling, so swipes there become PgUp/PgDn --

    @Test
    fun detectsMouseReportingFromAnyTrackingModeOn() {
        assertTrue(decodeWithModes("""[{"ansi":false,"code":1000,"on":true}]""").mouseReporting)
        assertTrue(decodeWithModes("""[{"ansi":false,"code":1002,"on":true}]""").mouseReporting)
        assertTrue(decodeWithModes("""[{"ansi":false,"code":1003,"on":true}]""").mouseReporting)
    }

    @Test
    fun opencodesFullMouseModeSetIsDetected() {
        val modes = """[{"ansi":false,"code":1000,"on":true},{"ansi":false,"code":1002,"on":true},
            {"ansi":false,"code":1003,"on":true},{"ansi":false,"code":1006,"on":true}]"""
        assertTrue(decodeWithModes(modes).mouseReporting)
    }

    @Test
    fun mouseTrackingOffMeansLocalScrolling() {
        assertFalse(decodeWithModes("""[{"ansi":false,"code":1000,"on":false}]""").mouseReporting)
        assertFalse(decodeWithModes("""[{"ansi":false,"code":1002,"on":false}]""").mouseReporting)
    }

    @Test
    fun ansiMouseCodesDoNotCountAsDecMouseReporting() {
        // 1000+ exist as ANSI (not DEC-private) modes too; only the DEC form counts.
        assertFalse(decodeWithModes("""[{"ansi":true,"code":1000,"on":true}]""").mouseReporting)
    }

    @Test
    fun sgrEncodingAloneIsNotMouseReporting() {
        // 1006 only changes the mouse-report ENCODING; a tracking mode must be on too.
        assertFalse(decodeWithModes("""[{"ansi":false,"code":1006,"on":true}]""").mouseReporting)
    }

    @Test
    fun noModesMeansNoMouseReporting() {
        assertFalse(RenderGridDecoder.decode(grid()).mouseReporting)
    }

    // -- whose overscroll may page the pane (see DecodedGrid.mayPageOnOverscroll) --

    private fun decodeWithScreen(activeScreen: String) = RenderGridDecoder.decode(
        BridgeJson.decodeFromString(
            RenderGrid.serializer(),
            """{"columns":1,"rows":1,"active_screen":"$activeScreen"}""",
        ),
    )

    @Test
    fun anAlternateScreenPaneMayPageWithoutMouseReporting() {
        // `less` and `vim` started without mouse support: no tracking mode, but
        // the alternate screen has no scrollback, so once the visible grid has
        // been panned to its edge there is nothing local left to give.
        val decoded = decodeWithScreen("alternate")
        assertFalse(decoded.mouseReporting)
        assertTrue(decoded.alternateScreen)
        assertTrue(decoded.mayPageOnOverscroll)
    }

    @Test
    fun primaryScreenWithoutMouseReportingScrollsLocally() {
        val decoded = decodeWithScreen("primary")
        assertFalse(decoded.alternateScreen)
        assertFalse(decoded.mayPageOnOverscroll)
    }

    @Test
    fun mouseReportingOnThePrimaryScreenMayPageToo() {
        val decoded = decodeWithModes("""[{"ansi":false,"code":1000,"on":true}]""")
        assertFalse(decoded.alternateScreen)
        assertTrue(decoded.mayPageOnOverscroll)
    }

    @Test
    fun anAbsentActiveScreenIsNotTreatedAsAlternate() {
        // Older cmux builds omit the field; the safe reading is "primary", which
        // leaves local scrolling exactly as it was.
        assertFalse(RenderGridDecoder.decode(grid()).alternateScreen)
        assertFalse(RenderGridDecoder.decode(grid()).mayPageOnOverscroll)
    }
}
