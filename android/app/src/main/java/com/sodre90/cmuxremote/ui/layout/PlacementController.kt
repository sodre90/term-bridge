package com.sodre90.cmuxremote.ui.layout

import com.sodre90.cmuxremote.data.FallbackBridgeClient
import com.sodre90.cmuxremote.model.WorkspaceLayout
import com.sodre90.cmuxremote.ui.sessions.ActionOutcome
import com.sodre90.cmuxremote.ui.sessions.actionFailureOf
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

/** The placement sheet's state; null when the sheet is not open. */
sealed interface PlacementState {
    data object Loading : PlacementState

    /** The layout came back; [titles] name surfaces for the miniature. A
     *  refused split keeps the sheet up with [error] under its button. */
    data class Ready(
        val workspaceId: String,
        val layout: WorkspaceLayout,
        val titles: Map<String, String>,
        /** Whether the host has tabs at all; a tmux host offers only splits. */
        val tabs: Boolean = true,
        val busy: Boolean = false,
        val error: ActionOutcome.Failed? = null,
    ) : PlacementState

    /** The layout itself could not be fetched; nothing to place on. */
    data class Failed(val outcome: ActionOutcome.Failed) : PlacementState
}

/**
 * Opens the placement sheet on a workspace's layout and carries out the
 * split or tab it asks for. Shared by the sessions and terminal screens,
 * which differ only in what happens after: [afterCreate] runs before the
 * caller's own callback.
 */
class PlacementController(
    private val scope: CoroutineScope,
    private val client: () -> FallbackBridgeClient?,
    private val afterCreate: () -> Unit = {},
) {
    private val _state = MutableStateFlow<PlacementState?>(null)
    val state: StateFlow<PlacementState?> = _state.asStateFlow()

    fun open(workspaceId: String, titles: Map<String, String>) {
        val bridge = client() ?: return
        _state.value = PlacementState.Loading
        scope.launch {
            _state.value = try {
                PlacementState.Ready(
                    workspaceId,
                    bridge.layout(workspaceId),
                    titles,
                    tabs = bridge.hostInfo.value.capabilities.tabs,
                )
            } catch (e: Exception) {
                PlacementState.Failed(ActionOutcome.Failed(actionFailureOf(e), e.message))
            }
        }
    }

    fun close() {
        _state.value = null
    }

    /** Creates the pane; on success the sheet closes and [onCreated] gets
     *  the new surface, on refusal the sheet stays with the reason. */
    fun createPane(surfaceId: String, placement: String, onCreated: (surfaceId: String) -> Unit) {
        val bridge = client() ?: return
        val ready = _state.value as? PlacementState.Ready ?: return
        if (ready.busy) return
        _state.value = ready.copy(busy = true, error = null)
        scope.launch {
            try {
                val created = bridge.createPane(ready.workspaceId, surfaceId, placement)
                _state.value = null
                afterCreate()
                onCreated(created.surfaceId)
            } catch (e: Exception) {
                val failure = ActionOutcome.Failed(actionFailureOf(e), e.message)
                _state.update { (it as? PlacementState.Ready)?.copy(busy = false, error = failure) ?: it }
            }
        }
    }
}
