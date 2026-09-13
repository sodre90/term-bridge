package com.sodre90.cmuxremote.ui.layout

import com.sodre90.cmuxremote.model.LayoutPane
import com.sodre90.cmuxremote.model.PanePlacement
import com.sodre90.cmuxremote.model.WorkspaceLayout
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class PaneMiniatureTest {

    private fun pane(
        id: String,
        x: Double,
        y: Double,
        w: Double,
        h: Double,
        vararg surfaces: String,
        focused: Boolean = false,
    ) = LayoutPane(
        id = id,
        x = x,
        y = y,
        w = w,
        h = h,
        focused = focused,
        surfaceIds = surfaces.toList(),
        selectedSurfaceId = surfaces.first(),
    )

    private val single = WorkspaceLayout(panes = listOf(pane("p", 0.0, 0.0, 1.0, 1.0, "s1", "s2")))

    /** The live 2x2 the bridge reports after right, then up on each side. */
    private val quad = WorkspaceLayout(
        panes = listOf(
            pane("a", 0.0, 0.0, 0.5, 0.5, "sa"),
            pane("b", 0.0, 0.5, 0.5, 0.5, "sb", focused = true),
            pane("c", 0.5, 0.0, 0.5, 0.5, "sc"),
            pane("d", 0.5, 0.5, 0.5, 0.5, "sd"),
        ),
    )

    private fun rect(x: Float, y: Float, w: Float, h: Float) = MiniRect(x, y, w, h)

    @Test
    fun rightKeepsTheLeftHalfAndGhostsTheRight() {
        val m = miniature(single, "s1", PanePlacement.RIGHT)
        assertEquals(rect(0f, 0f, 0.5f, 1f), m.panes.single().rect)
        assertEquals(rect(0.5f, 0f, 0.5f, 1f), m.ghost)
        assertEquals("s1", m.targetSurfaceId)
    }

    @Test
    fun leftGhostsTheLeftHalf() {
        val m = miniature(single, "s1", PanePlacement.LEFT)
        assertEquals(rect(0.5f, 0f, 0.5f, 1f), m.panes.single().rect)
        assertEquals(rect(0f, 0f, 0.5f, 1f), m.ghost)
    }

    @Test
    fun upGhostsTheTopHalfAndDownTheBottom() {
        val up = miniature(single, "s1", PanePlacement.UP)
        assertEquals(rect(0f, 0.5f, 1f, 0.5f), up.panes.single().rect)
        assertEquals(rect(0f, 0f, 1f, 0.5f), up.ghost)

        val down = miniature(single, "s1", PanePlacement.DOWN)
        assertEquals(rect(0f, 0f, 1f, 0.5f), down.panes.single().rect)
        assertEquals(rect(0f, 0.5f, 1f, 0.5f), down.ghost)
    }

    @Test
    fun tabLeavesTheRectanglesAloneAndGrowsOneMoreTabThanThePaneHas() {
        val m = miniature(single, "s2", PanePlacement.TAB)
        assertEquals(rect(0f, 0f, 1f, 1f), m.panes.single().rect)
        assertNull(m.ghost)
        assertEquals(3, m.tabStrip.size)
        assertEquals(0f, m.tabStrip.first().x, 0f)
        assertTrue(m.tabStrip.all { it.y == 0f && it.h > 0f })
        assertEquals(m.tabStrip[0].right, m.tabStrip[1].x, 1e-6f)
        assertEquals("s2", m.targetSurfaceId)
    }

    @Test
    fun onlyTheTargetPaneMovesInTheQuad() {
        val m = miniature(quad, "sd", PanePlacement.UP)
        val byId = m.panes.associateBy { it.id }
        assertEquals(rect(0.5f, 0.75f, 0.5f, 0.25f), byId.getValue("d").rect)
        assertEquals(rect(0.5f, 0.5f, 0.5f, 0.25f), m.ghost)
        assertEquals(rect(0f, 0f, 0.5f, 0.5f), byId.getValue("a").rect)
        assertEquals(rect(0f, 0.5f, 0.5f, 0.5f), byId.getValue("b").rect)
        assertEquals(rect(0.5f, 0f, 0.5f, 0.5f), byId.getValue("c").rect)
        assertEquals(listOf("d"), m.panes.filter { it.isTarget }.map { it.id })
    }

    @Test
    fun withoutASurfaceTheFocusedPaneIsTheTargetThenTheFirst() {
        assertEquals("sb", miniature(quad, null, PanePlacement.RIGHT).targetSurfaceId)
        assertEquals("sb", miniature(quad, "gone-since-the-list-loaded", PanePlacement.RIGHT).targetSurfaceId)
        val unfocused = WorkspaceLayout(panes = quad.panes.map { it.copy(focused = false) })
        assertEquals("sa", miniature(unfocused, "gone-since-the-list-loaded", PanePlacement.RIGHT).targetSurfaceId)
    }

    @Test
    fun anEmptyLayoutHasNothingToTarget() {
        val m = miniature(WorkspaceLayout(), "s1", PanePlacement.RIGHT)
        assertTrue(m.panes.isEmpty())
        assertNull(m.targetSurfaceId)
        assertNull(m.ghost)
    }

    @Test
    fun estimatedLayoutsStillGetAGhostAndSayTheyAreEstimated() {
        val estimated = WorkspaceLayout(
            estimated = true,
            panes = listOf(pane("a", 0.0, 0.0, 0.5, 1.0, "sa"), pane("b", 0.5, 0.0, 0.5, 1.0, "sb")),
        )
        val m = miniature(estimated, "sb", PanePlacement.DOWN)
        assertTrue(m.estimated)
        assertEquals(rect(0.5f, 0.5f, 0.5f, 0.5f), m.ghost)
    }

    @Test
    fun theGhostEntersFromTheEdgeItIsPlacedAt() {
        val ghost = rect(0.5f, 0f, 0.5f, 1f)
        assertEquals(rect(1f, 0f, 0f, 1f), ghostEntryEdge(ghost, PanePlacement.RIGHT))
        assertEquals(rect(0.5f, 0f, 0f, 1f), ghostEntryEdge(ghost, PanePlacement.LEFT))
        val lower = rect(0f, 0.5f, 1f, 0.5f)
        assertEquals(rect(0f, 1f, 1f, 0f), ghostEntryEdge(lower, PanePlacement.DOWN))
        assertEquals(rect(0f, 0.5f, 1f, 0f), ghostEntryEdge(lower, PanePlacement.UP))
    }

    @Test
    fun containsIsHalfOpenSoNeighboursDoNotBothClaimAnEdge() {
        val left = rect(0f, 0f, 0.5f, 1f)
        val right = rect(0.5f, 0f, 0.5f, 1f)
        assertTrue(left.contains(0.49f, 0.5f))
        assertTrue(right.contains(0.5f, 0.5f))
        assertTrue(!left.contains(0.5f, 0.5f))
    }
}
