package com.sodre90.cmuxremote.ui.inbox

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.sodre90.cmuxremote.data.BridgeException
import com.sodre90.cmuxremote.data.BridgeGateway
import com.sodre90.cmuxremote.data.ConnectionStatus
import com.sodre90.cmuxremote.data.FallbackBridgeClient
import com.sodre90.cmuxremote.data.SocketReconnector
import com.sodre90.cmuxremote.model.EventFrame
import com.sodre90.cmuxremote.model.FeedReply
import com.sodre90.cmuxremote.model.PendingFeedItem
import com.sodre90.cmuxremote.ui.UiState
import com.sodre90.cmuxremote.ui.sessions.TerminalMatch
import com.sodre90.cmuxremote.ui.sessions.pendingItemTarget
import kotlinx.coroutines.FlowPreview
import kotlinx.coroutines.channels.BufferOverflow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.debounce
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.add
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import kotlinx.serialization.json.putJsonArray

/**
 * Backs the agent inbox. Pending blocking prompts come from `GET /feed/pending`
 * (cmux `feed.list`), which carries the real `request_id` and the choosable
 * options the user must pick from — the event stream has neither. The live
 * `/events` socket is used only as a trigger to re-fetch whenever the pending
 * set may have changed; the prompt content always comes from a fresh
 * pending-feed fetch.
 *
 * The error-message parameters are pre-resolved `strings.xml` text passed in
 * by the caller (see CmuxNavHost) rather than resolved here: a ViewModel has
 * no @Composable context to call `stringResource()` itself.
 */
@OptIn(FlowPreview::class)
class InboxViewModel(
    bridge: BridgeGateway,
    private val bridgeNotConfiguredMessage: String,
    private val loadInboxFailedMessage: String,
    private val replyFailedMessage: String,
    private val promptGoneMessage: String,
    private val terminalNotFoundMessage: String,
) : ViewModel() {

    private val client = bridge.activeBridge()

    // [UiState.Error] is set only when the *first* load fails, i.e. while there
    // is still no list to blow away -- a later failed refresh keeps the list and
    // surfaces itself through [actionError] instead. Before that distinction
    // existed, a first-load failure left this on Loading forever, which
    // InboxScreen rendered as "No pending prompts": an empty inbox asserted over
    // a request that had never returned, on the very screen a push sends you to.
    private val _state = MutableStateFlow<UiState<List<PendingFeedItem>>>(UiState.Loading)
    val state: StateFlow<UiState<List<PendingFeedItem>>> = _state.asStateFlow()

    // Surfaced separately from [state] so a failed refresh/reply doesn't blow
    // away an already-loaded list (mirrors SessionsViewModel's actionError).
    private val _actionError = MutableStateFlow<String?>(null)
    val actionError: StateFlow<String?> = _actionError.asStateFlow()

    // True only while a refresh the user asked for is in flight -- see
    // [userRefresh].
    private val _isRefreshing = MutableStateFlow(false)
    val isRefreshing: StateFlow<Boolean> = _isRefreshing.asStateFlow()

    // Coalesces bursts of cmux feed events (two per tool call) into a single
    // refetch instead of hammering feed.list -- same trick as SessionsViewModel.
    private val refreshRequests =
        MutableSharedFlow<Unit>(extraBufferCapacity = 1, onBufferOverflow = BufferOverflow.DROP_OLDEST)

    private val reconnector = SocketReconnector<EventFrame>(
        bridge.relayHealth(),
        monitor = bridge.connectionMonitor(),
        slotCredentials = bridge.slotCredentials(),
    )

    /** Same process-wide transport status SessionsViewModel exposes -- replying
     *  to a prompt is the one action where knowing the connection is mid-
     *  failover actually changes what the user does. */
    val connectionStatus: StateFlow<ConnectionStatus> = bridge.connectionMonitor().status

    init {
        refresh()
        viewModelScope.launch {
            refreshRequests.debounce(EVENT_REFRESH_DEBOUNCE_MS).collect { refresh() }
        }
        // Re-fetch whenever the pending set may have changed -- see
        // [isPendingSetChangeSignal] for why that is every feed frame and not
        // just the attention-flagged ones. The socket is dropped when the app
        // is backgrounded (the subscription below is keyed on foreground --
        // see SessionsViewModel.subscribeToEvents for the data-usage rationale),
        // so reconnect with backoff and re-sync pending items after each gap
        // instead of dying on the first disconnect.
        if (bridge.anyBridgeConfigured()) {
            viewModelScope.launch {
                bridge.appForeground().collectLatest { foreground ->
                    if (!foreground) return@collectLatest
                    reconnector.run(
                        openSocket = { slot, onOpen -> bridge.eventsSocket(slot)?.connect(onOpen) },
                        onBeforeReconnect = { refresh() },
                    ) { frame ->
                        if (isPendingSetChangeSignal(frame.type)) refreshRequests.tryEmit(Unit)
                        true
                    }
                }
            }
        }
    }

    fun refresh() {
        val c = clientOrReportMissing() ?: return
        viewModelScope.launch { fetchPending(c) }
    }

    /** The refresh the user asked for -- the pull gesture or the top-bar button.
     *  Identical fetch to [refresh], but it drives [isRefreshing] so the pull
     *  spinner has something to follow; event-driven refetches deliberately
     *  don't, so agent activity nobody asked about never pops it. */
    fun userRefresh() {
        val c = clientOrReportMissing() ?: return
        viewModelScope.launch {
            _isRefreshing.value = true
            try {
                fetchPending(c)
            } finally {
                _isRefreshing.value = false
            }
        }
    }

    // Same as SessionsViewModel: with no bridge there is nothing to wait for,
    // so this is a settled failure rather than a load in progress.
    private fun clientOrReportMissing(): FallbackBridgeClient? = client.also {
        if (it == null) {
            if (_state.value is UiState.Loading) _state.value = UiState.Error(bridgeNotConfiguredMessage)
            _actionError.value = bridgeNotConfiguredMessage
        }
    }

    private suspend fun fetchPending(c: FallbackBridgeClient) {
        try {
            val items = c.pendingFeed().filter { isPendingInboxKind(it.kind) }
            _state.value = UiState.Ready(items)
            _actionError.value = null
        } catch (ex: Exception) {
            // Demotes to Error only while nothing has loaded yet; once a
            // list is showing it stays -- see [_state]'s doc comment.
            val message = ex.message ?: loadInboxFailedMessage
            if (_state.value is UiState.Loading) _state.value = UiState.Error(message)
            _actionError.value = message
        }
    }

    /** Retry after a failed first load. Unlike [refresh] this drops back to
     *  Loading so the button visibly does something -- safe because it is only
     *  reachable from the error screen, where there is no list to lose. */
    fun retry() {
        _state.value = UiState.Loading
        _actionError.value = null
        refresh()
    }

    /** Answer a question item with the labels of the chosen options. */
    fun reply(item: PendingFeedItem, selections: List<String>) {
        val params = buildJsonObject {
            putJsonArray("selections") { selections.forEach { add(it) } }
        }
        sendReply(item, "question", params)
    }

    /** Approve or deny a permissionRequest item once -- not a recurring
     *  YOLO auto-mode (see [com.sodre90.cmuxremote.model.YoloMode] for those). */
    fun replyPermission(item: PendingFeedItem, approve: Boolean) {
        val params = buildJsonObject { put("mode", if (approve) "once" else "deny") }
        sendReply(item, "permissionRequest", params)
    }

    private fun sendReply(item: PendingFeedItem, kind: String, params: JsonObject) {
        val c = client ?: run {
            _actionError.value = bridgeNotConfiguredMessage
            return
        }
        viewModelScope.launch {
            try {
                c.replyFeed(item.id, FeedReply(kind, item.requestId, params))
                dropItem(item)
            } catch (ex: BridgeException) {
                // 409: someone answered the prompt at the keyboard first (a
                // tmux host reports this; cmux never does). The card is stale
                // the moment the bridge says so -- waiting for the next
                // refetch would leave a prompt on screen that no longer exists.
                if (ex.code == HTTP_CONFLICT) {
                    dropItem(item)
                    _actionError.value = promptGoneMessage
                } else {
                    _actionError.value = ex.message ?: replyFailedMessage
                }
            } catch (ex: Exception) {
                _actionError.value = ex.message ?: replyFailedMessage
            }
        }
    }

    private fun dropItem(item: PendingFeedItem) {
        _state.update { cur ->
            if (cur is UiState.Ready) UiState.Ready(cur.data.filterNot { it.id == item.id }) else cur
        }
    }

    /**
     * Resolves [item]'s originating terminal for the inbox row's "open
     * terminal" affordance -- either a single surface directly, several
     * candidate workspaces for the caller to show a picker over (see
     * [pendingItemTarget]'s doc comment on why cwd alone can be ambiguous),
     * or null (with [actionError] set) if nothing matches at all.
     * [PendingFeedItem] carries no workspace/surface id of its own, so this
     * always does a fresh live lookup rather than reusing [state].
     */
    suspend fun terminalTarget(item: PendingFeedItem): TerminalMatch? {
        val c = client ?: run {
            _actionError.value = bridgeNotConfiguredMessage
            return null
        }
        val target = runCatching { c.sessions().workspaces }.getOrNull()?.let { pendingItemTarget(item, it) }
        if (target == null) _actionError.value = terminalNotFoundMessage
        return target
    }

    private companion object {
        const val EVENT_REFRESH_DEBOUNCE_MS = 800L
        const val HTTP_CONFLICT = 409
    }
}
