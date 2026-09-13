package com.sodre90.cmuxremote.ui.sessions

import com.sodre90.cmuxremote.data.BridgeException
import com.sodre90.cmuxremote.model.Workspace
import java.net.HttpURLConnection

/**
 * A directory offered for a new workspace: one already used by a workspace
 * cmux reports, with the titles of those workspaces as a hint. cmux has no
 * directory-listing RPC, so these and a typed path are the only choices.
 */
data class RecentDirectory(val path: String, val usedBy: List<String>)

/** Distinct workspace directories in the list's own order (cmux's, which
 *  puts recent work first), blank ones dropped. */
fun recentDirectories(workspaces: List<Workspace>): List<RecentDirectory> =
    workspaces
        .filter { it.cwd.isNotBlank() }
        .groupBy { it.cwd }
        .map { (cwd, group) ->
            RecentDirectory(cwd, group.map { it.title.ifBlank { it.preview } }.filter { it.isNotBlank() })
        }

/** Why a workspace or pane action failed, as far as the phone can tell. */
enum class ActionFailure {
    /** Every new route is a 404 on a bridge from before they existed. */
    BRIDGE_TOO_OLD,
    CWD_NOT_ABSOLUTE,
    CWD_NOT_FOUND,
    CWD_NOT_DIR,
    CWD_OUTSIDE_HOME,
    OTHER,
}

/** Maps a bridge refusal to something the dialog can word; the cwd reasons
 *  mirror `cwdRefusal` in bridge/internal/server/workspaces.go. */
fun actionFailureOf(e: Exception): ActionFailure {
    val bridge = e as? BridgeException ?: return ActionFailure.OTHER
    if (bridge.code == HttpURLConnection.HTTP_NOT_FOUND) return ActionFailure.BRIDGE_TOO_OLD
    if (bridge.code != HttpURLConnection.HTTP_BAD_REQUEST) return ActionFailure.OTHER
    return when {
        "cwd_not_absolute" in bridge.bodyText -> ActionFailure.CWD_NOT_ABSOLUTE
        "cwd_not_found" in bridge.bodyText -> ActionFailure.CWD_NOT_FOUND
        "cwd_not_dir" in bridge.bodyText -> ActionFailure.CWD_NOT_DIR
        "cwd_outside_home" in bridge.bodyText -> ActionFailure.CWD_OUTSIDE_HOME
        else -> ActionFailure.OTHER
    }
}

/** What a workspace or pane action came to, for the one-line notice the
 *  screen shows. Successes that navigate away carry nothing to say. */
sealed interface ActionOutcome {
    data object ShownOnMac : ActionOutcome
    data object WorkspaceClosed : ActionOutcome
    data class Failed(val failure: ActionFailure, val detail: String?) : ActionOutcome
}
