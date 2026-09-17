package com.sodre90.cmuxremote.ui.terminal

import android.app.Application
import android.os.Build
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.input.nestedscroll.nestedScroll
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.performTouchInput
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import com.sodre90.cmuxremote.model.DecodedGrid
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config

/**
 * The handoff itself: local panning first, and only what the grid could NOT
 * absorb reaches the pane.
 *
 * This is the one assumption the fix rests on and the one [SwipePagerTest]
 * cannot reach -- that a descendant `verticalScroll` reports its leftover to an
 * ancestor's [androidx.compose.ui.input.nestedscroll.NestedScrollConnection]
 * rather than swallowing the whole drag. Compose's pointer consumption is
 * all-or-nothing, so the version this replaces could not tell a drag the grid
 * used from one it refused, and resolved that by claiming every drag on the
 * Initial pass -- which left every alt-screen pane unpannable (cmux-app-4yi).
 */
// A plain Application: the real CmuxApp builds an AndroidKeyStore-backed
// Settings on construction, which no JVM-side runtime can provide, and none of
// it is involved in a scroll gesture.
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [Build.VERSION_CODES.UPSIDE_DOWN_CAKE], application = Application::class)
class PaneOverscrollHandoffTest {

    @get:Rule val compose = createComposeRule()

    private val sent = mutableListOf<String>()

    private val viewportDp = 100.dp
    private val contentDp = 300.dp

    /** An idle shell stranded on the alternate screen -- the pane that named
     *  cmux-app-4yi. It pages on nothing, and that has to stay survivable. */
    private val altScreenGrid = DecodedGrid(
        columns = 80,
        rows = 24,
        lines = emptyList(),
        cursor = null,
        alternateScreen = true,
    )

    @Composable
    private fun GridUnderPager() {
        val scroll = rememberScrollState()
        val viewportPx = with(LocalDensity.current) { viewportDp.toPx() }
        val pager = rememberPaneOverscrollPager(
            grid = altScreenGrid,
            viewportHeightPx = viewportPx,
            onSend = { sent += it },
        )
        Box(modifier = Modifier.size(viewportDp).nestedScroll(pager).testTag(GRID)) {
            Column(modifier = Modifier.verticalScroll(scroll)) {
                Box(modifier = Modifier.fillMaxWidth().height(contentDp))
            }
        }
    }

    // The grid starts at the top, so upward is the direction with room in it:
    // 200dp of content below the fold to pan through before anything is left over.
    private fun dragUpBy(dp: Dp) {
        compose.setContent { GridUnderPager() }
        val travelPx = with(compose.density) { dp.toPx() }
        compose.onNodeWithTag(GRID).performTouchInput {
            down(center)
            moveBy(Offset(0f, -travelPx))
            up()
        }
        compose.waitForIdle()
    }

    @Test fun aDragTheGridCanAbsorbNeverReachesThePane() {
        // 60dp of pan against 200dp of room: the grid takes all of it, so the
        // pane must hear nothing. This is what the alt-screen pane lost -- its
        // whole grid is taller than the phone viewport, so this drag IS the
        // scrolling the user was asking for.
        dragUpBy(60.dp)
        assertEquals(emptyList<String>(), sent)
    }

    @Test fun onlyTheOverscrollBeyondTheGridsEdgeReachesThePane() {
        // 400dp against the same 200dp of room: the grid runs out and the
        // remainder pages the pane. Dragging UP pushes later output into view,
        // which is PgDn.
        dragUpBy(400.dp)
        assertTrue("expected the overscroll to page the pane, got $sent", sent.isNotEmpty())
        assertEquals(List(sent.size) { "$ESC[6~" }, sent)
    }

    private companion object {
        const val GRID = "grid"
    }
}
