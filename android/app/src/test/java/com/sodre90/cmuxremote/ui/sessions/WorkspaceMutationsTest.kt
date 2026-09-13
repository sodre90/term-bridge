package com.sodre90.cmuxremote.ui.sessions

import com.sodre90.cmuxremote.data.BridgeException
import com.sodre90.cmuxremote.model.Workspace
import org.junit.Assert.assertEquals
import org.junit.Test
import java.io.IOException

class WorkspaceMutationsTest {

    private fun ws(id: String, cwd: String, title: String = "", preview: String = "") =
        Workspace(id = id, cwd = cwd, title = title, preview = preview)

    @Test
    fun recentDirectoriesAreDistinctInListOrderWithTheWorkspacesUsingThem() {
        val recent = recentDirectories(
            listOf(
                ws("a", "/Users/me/prj/llama.cpp", title = "Qwen Flash"),
                ws("b", "/Users/me/prj/cmux-app", title = "Android app"),
                ws("c", "/Users/me/prj/llama.cpp", title = "", preview = "Claude is waiting"),
                ws("d", "", title = "no cwd yet"),
                ws("e", "/Users/me/prj/cmux-app", title = "Photo attach"),
            ),
        )
        assertEquals(
            listOf(
                RecentDirectory("/Users/me/prj/llama.cpp", listOf("Qwen Flash", "Claude is waiting")),
                RecentDirectory("/Users/me/prj/cmux-app", listOf("Android app", "Photo attach")),
            ),
            recent,
        )
    }

    @Test
    fun aWorkspaceWithNeitherTitleNorPreviewStillOffersItsDirectory() {
        assertEquals(
            listOf(RecentDirectory("/Users/me", emptyList())),
            recentDirectories(listOf(ws("a", "/Users/me"))),
        )
    }

    @Test
    fun aFourOhFourMeansTheBridgePredatesTheRoute() {
        assertEquals(ActionFailure.BRIDGE_TOO_OLD, actionFailureOf(BridgeException(404, "404 page not found")))
    }

    private fun refused(code: Int, reason: String) = actionFailureOf(BridgeException(code, """{"error":"$reason"}"""))

    @Test
    fun theBridgesDirectoryRefusalsAreTold() {
        assertEquals(ActionFailure.CWD_NOT_ABSOLUTE, refused(400, "cwd_not_absolute"))
        assertEquals(ActionFailure.CWD_NOT_FOUND, refused(400, "cwd_not_found"))
        assertEquals(ActionFailure.CWD_NOT_DIR, refused(400, "cwd_not_dir"))
        assertEquals(ActionFailure.CWD_OUTSIDE_HOME, refused(400, "cwd_outside_home"))
    }

    @Test
    fun anythingElseIsJustAFailure() {
        assertEquals(ActionFailure.OTHER, refused(400, "invalid json"))
        assertEquals(ActionFailure.OTHER, refused(502, "cmux workspace.close failed"))
        assertEquals(ActionFailure.OTHER, actionFailureOf(IOException("timeout")))
    }
}
