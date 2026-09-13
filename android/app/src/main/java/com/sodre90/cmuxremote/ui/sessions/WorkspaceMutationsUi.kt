package com.sodre90.cmuxremote.ui.sessions

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.res.pluralStringResource
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.sodre90.cmuxremote.R
import com.sodre90.cmuxremote.model.Attention
import com.sodre90.cmuxremote.model.Workspace

/**
 * Where a new workspace should live: the directories already in use, each
 * a tap away, and a field for any other path. The bridge is the judge of
 * the path (it must exist on the Mac, under the home folder); a refusal
 * comes back as an [ActionOutcome.Failed] the screen words.
 */
@Composable
internal fun NewWorkspaceDialog(
    recent: List<RecentDirectory>,
    onCreate: (cwd: String, title: String?) -> Unit,
    onDismiss: () -> Unit,
) {
    var path by rememberSaveable { mutableStateOf("") }
    var title by rememberSaveable { mutableStateOf("") }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(stringResource(R.string.sessions_new_workspace)) },
        text = {
            Column(
                modifier = Modifier.heightIn(max = 420.dp).verticalScroll(rememberScrollState()),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                if (recent.isNotEmpty()) {
                    Text(
                        stringResource(R.string.sessions_new_workspace_recent),
                        style = MaterialTheme.typography.labelLarge,
                    )
                    recent.forEach { dir ->
                        RecentDirectoryRow(dir, selected = dir.path == path, onSelect = { path = dir.path })
                    }
                }
                OutlinedTextField(
                    value = path,
                    onValueChange = { path = it },
                    label = { Text(stringResource(R.string.sessions_new_workspace_path_label)) },
                    placeholder = { Text(stringResource(R.string.sessions_new_workspace_path_hint)) },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
                OutlinedTextField(
                    value = title,
                    onValueChange = { title = it },
                    label = { Text(stringResource(R.string.sessions_new_workspace_title_label)) },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
            }
        },
        confirmButton = {
            TextButton(
                onClick = { onCreate(path.trim(), title.trim().ifBlank { null }) },
                enabled = path.isNotBlank(),
            ) {
                Text(stringResource(R.string.sessions_new_workspace_create))
            }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text(stringResource(R.string.action_cancel)) } },
    )
}

@Composable
private fun RecentDirectoryRow(dir: RecentDirectory, selected: Boolean, onSelect: () -> Unit) {
    Column(
        modifier = Modifier.fillMaxWidth().clickable(onClick = onSelect).padding(vertical = 6.dp),
    ) {
        Text(
            dir.path,
            style = MaterialTheme.typography.bodyMedium,
            fontWeight = if (selected) FontWeight.Bold else FontWeight.Normal,
            color = if (selected) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurface,
        )
        if (dir.usedBy.isNotEmpty()) {
            Text(
                stringResource(R.string.sessions_new_workspace_used_by, dir.usedBy.joinToString(", ")),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

/**
 * The one guard on the one destructive action: names the workspace, counts
 * its panes, and says when an agent in it is mid-prompt or YOLO is on.
 */
@Composable
internal fun CloseWorkspaceDialog(ws: Workspace, onConfirm: () -> Unit, onDismiss: () -> Unit) {
    val name = ws.title.ifBlank { ws.preview.ifBlank { ws.cwd } }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(stringResource(R.string.sessions_close_workspace_title)) },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                val panes = ws.terminals.size
                Text(pluralStringResource(R.plurals.sessions_close_workspace_panes, panes, name, panes))
                if (ws.attention != Attention.NONE) {
                    Text(
                        stringResource(R.string.sessions_close_workspace_attention),
                        color = MaterialTheme.colorScheme.error,
                    )
                }
                if (ws.yoloMode.isNotEmpty()) {
                    Text(stringResource(R.string.sessions_close_workspace_yolo))
                }
            }
        },
        confirmButton = {
            TextButton(onClick = onConfirm) {
                Text(stringResource(R.string.action_close_workspace), color = MaterialTheme.colorScheme.error)
            }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text(stringResource(R.string.action_cancel)) } },
    )
}

@Composable
internal fun ClosePaneDialog(paneName: String, onConfirm: () -> Unit, onDismiss: () -> Unit) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(stringResource(R.string.terminal_close_pane_title)) },
        text = {
            Text(
                if (paneName.isBlank()) {
                    stringResource(R.string.terminal_close_pane_body_unnamed)
                } else {
                    stringResource(R.string.terminal_close_pane_body, paneName)
                },
            )
        },
        confirmButton = {
            TextButton(onClick = onConfirm) {
                Text(stringResource(R.string.action_close_pane), color = MaterialTheme.colorScheme.error)
            }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text(stringResource(R.string.action_cancel)) } },
    )
}

/** The one line a finished action gets. */
@Composable
internal fun actionOutcomeText(outcome: ActionOutcome): String = when (outcome) {
    ActionOutcome.ShownOnMac -> stringResource(R.string.action_outcome_shown_on_mac)
    ActionOutcome.WorkspaceClosed -> stringResource(R.string.action_outcome_workspace_closed)
    is ActionOutcome.Failed -> when (outcome.failure) {
        ActionFailure.BRIDGE_TOO_OLD -> stringResource(R.string.action_failed_bridge_too_old)
        ActionFailure.CWD_NOT_ABSOLUTE -> stringResource(R.string.action_failed_cwd_not_absolute)
        ActionFailure.CWD_NOT_FOUND -> stringResource(R.string.action_failed_cwd_not_found)
        ActionFailure.CWD_NOT_DIR -> stringResource(R.string.action_failed_cwd_not_dir)
        ActionFailure.CWD_OUTSIDE_HOME -> stringResource(R.string.action_failed_cwd_outside_home)
        ActionFailure.OTHER -> outcome.detail?.let { stringResource(R.string.action_failed_other, it) }
            ?: stringResource(R.string.action_failed_unknown)
    }
}
