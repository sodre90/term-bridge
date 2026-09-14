package com.sodre90.cmuxremote.ui.layout

import com.sodre90.cmuxremote.model.PanePlacement
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class OfferedPlacementsTest {

    @Test
    fun hostWithTabsOffersEverySplitAndTheTab() {
        val offered = offeredPlacements(tabs = true)

        assertTrue(PanePlacement.TAB in offered)
        assertEquals(5, offered.size)
    }

    @Test
    fun hostWithoutTabsOffersOnlySplits() {
        val offered = offeredPlacements(tabs = false)

        assertFalse(PanePlacement.TAB in offered)
        assertEquals(listOf(PanePlacement.LEFT, PanePlacement.UP, PanePlacement.DOWN, PanePlacement.RIGHT), offered)
    }
}
