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
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.test.TouchInjectionScope
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.performTouchInput
import androidx.compose.ui.unit.dp
import org.junit.Assert.assertEquals
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config

/**
 * The grid's tap-to-type must survive as a tap and die as a drag -- in every
 * direction. With Wrap on nothing scrolls the grid horizontally, so nothing
 * consumed a sideways swipe, and `detectTapGestures` counted it as a tap that
 * popped the keyboard on lift-off (cmux-app-3il).
 */
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [Build.VERSION_CODES.UPSIDE_DOWN_CAKE], application = Application::class)
class TapWithinSlopTest {

    @get:Rule val compose = createComposeRule()

    private var taps = 0

    /**
     * A wrapped grid: the content only ever scrolls vertically. Phone-sized, so
     * a swipe stays inside it -- leaving the bounds is the one other thing that
     * ends a `detectTapGestures` tap, and would mask the bug.
     */
    @Composable
    private fun WrappedGrid() {
        Box(modifier = Modifier.size(800.dp).onTapWithinSlop { taps++ }.testTag(GRID)) {
            Column(modifier = Modifier.verticalScroll(rememberScrollState())) {
                Box(modifier = Modifier.fillMaxWidth().height(2400.dp))
            }
        }
    }

    private fun gesture(block: TouchInjectionScope.() -> Unit) {
        compose.setContent { WrappedGrid() }
        compose.onNodeWithTag(GRID).performTouchInput(block)
        compose.waitForIdle()
    }

    @Test fun aTapStillTaps() {
        gesture {
            down(center)
            up()
        }
        assertEquals(1, taps)
    }

    @Test fun aHorizontalSwipeIsNotATap() {
        gesture {
            down(center)
            moveBy(Offset(150f, 0f))
            up()
        }
        assertEquals(0, taps)
    }

    @Test fun aVerticalSwipeIsStillNotATap() {
        gesture {
            down(center)
            moveBy(Offset(0f, -150f))
            up()
        }
        assertEquals(0, taps)
    }

    @Test fun aWobbleUnderSlopIsStillATap() {
        gesture {
            val underSlop = 2.dp.toPx()
            down(center)
            moveBy(Offset(underSlop, underSlop))
            up()
        }
        assertEquals(1, taps)
    }

    private companion object {
        const val GRID = "grid"
    }
}
