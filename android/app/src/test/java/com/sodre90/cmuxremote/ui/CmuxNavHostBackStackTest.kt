package com.sodre90.cmuxremote.ui

import android.app.Application
import android.os.Build
import androidx.navigation.NavController
import androidx.navigation.compose.ComposeNavigator
import androidx.navigation.compose.composable
import androidx.navigation.createGraph
import androidx.navigation.testing.TestNavHostController
import org.junit.Assert.assertEquals
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.annotation.Config

/**
 * Back-stack coverage for Settings' "Done" ([leaveSettings]).
 *
 * cmux-app-4qm was a back-stack bug no unit test could have caught: Done
 * navigated unconditionally, which from Sessions pushed a SECOND Sessions
 * entry. Both of its paths were verified by hand on the device and neither was
 * guarded (cmux-app-4hc). The two need opposite behaviour, so getting either
 * one wrong is invisible until someone opens Settings the other way.
 *
 * Drives the real [leaveSettings] against a TestNavHostController rather than
 * composing CmuxNavHost, which would pull in every screen's ViewModel and its
 * networking.
 */
// A plain Application: the real CmuxApp's onCreate warms up its AppContainer on
// Dispatchers.IO, which under Robolectric dies of "AndroidKeyStore not found"
// in the background -- and kotlinx-coroutines-test hands that uncaught
// exception to whichever runTest starts next, failing an unrelated Compose
// test (seen 2026-09-14 on TapWithinSlopTest).
@RunWith(RobolectricTestRunner::class)
@Config(sdk = [Build.VERSION_CODES.UPSIDE_DOWN_CAKE], application = Application::class)
class CmuxNavHostBackStackTest {

    private fun controller(startDestination: String): TestNavHostController {
        val nav = TestNavHostController(RuntimeEnvironment.getApplication())
        nav.navigatorProvider.addNavigator(ComposeNavigator())
        nav.graph = nav.createGraph(startDestination = startDestination) {
            composable(Routes.SESSIONS) {}
            composable(Routes.SETTINGS) {}
            composable(Routes.INBOX) {}
        }
        return nav
    }

    private fun NavController.routes(): List<String> =
        currentBackStack.value.mapNotNull { it.destination.route }

    // Opened from Sessions: the stack is [SESSIONS, SETTINGS] and Done is a
    // plain pop. Navigating here is exactly the 4qm regression -- it left
    // [SESSIONS, SESSIONS], two SessionsViewModels polling, and a Back press
    // that swapped one identical screen for another.
    @Test fun doneFromSessionsPopsInsteadOfPushingASecondSessions() {
        val nav = controller(Routes.SESSIONS)
        nav.navigate(Routes.SETTINGS)
        assertEquals(listOf(Routes.SESSIONS, Routes.SETTINGS), nav.routes())

        nav.leaveSettings()

        assertEquals(listOf(Routes.SESSIONS), nav.routes())
    }

    // First run: SETTINGS is the start destination, there is no Sessions
    // underneath, so Done has to navigate -- and must not leave Settings on
    // the stack for Back to return to.
    @Test fun doneOnFirstRunNavigatesToSessionsAndDropsSettings() {
        val nav = controller(Routes.SETTINGS)
        assertEquals(listOf(Routes.SETTINGS), nav.routes())

        nav.leaveSettings()

        assertEquals(listOf(Routes.SESSIONS), nav.routes())
    }

    // Not covered on purpose: reaching SETTINGS from anything other than
    // SESSIONS. onSettings is only wired on the Sessions screen, so the
    // navigate branch is only ever taken on first run, where the stack is
    // exactly [SETTINGS]. A test that drove it from, say, INBOX would assert
    // [SESSIONS, INBOX, SESSIONS] -- the duplicate-Sessions shape 4qm was
    // filed for -- as if it were intended behaviour for a path that does not
    // exist. If Settings ever gains a second entry point, that is the moment
    // to decide what Done should do from there and to test it.
}
