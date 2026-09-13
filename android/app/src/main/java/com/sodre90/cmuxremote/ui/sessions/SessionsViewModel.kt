package com.sodre90.cmuxremote.ui.sessions

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.sodre90.cmuxremote.data.BridgeGateway
import com.sodre90.cmuxremote.data.ConnectionStatus
import com.sodre90.cmuxremote.data.FallbackBridgeClient
import com.sodre90.cmuxremote.data.SocketReconnector
import com.sodre90.cmuxremote.data.WorkspaceOrderGateway
import com.sodre90.cmuxremote.model.EventFrame
import com.sodre90.cmuxremote.model.Workspace
import com.sodre90.cmuxremote.ui.UiState
import com.sodre90.cmuxremote.ui.inbox.isPendingInboxKind
import com.sodre90.cmuxremote.ui.inbox.isPendingSetChangeSignal
import kotlinx.coroutines.FlowPreview
import kotlinx.coroutines.channels.BufferOverflow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.debounce
import kotlinx.coroutines.launch

// The error-message parameters are pre-resolved `strings.xml` text passed in by
// the caller (see CmuxNavHost) rather than resolved here: a ViewModel has no
// @Composable context to call `stringResource()` itself.
@OptIn(FlowPreview::class)
class SessionsViewModel(
    private val bridge: BridgeGateway,
    private val workspaceOrder: WorkspaceOrderGateway,
    private val bridgeNotConfiguredMessage: String,
    private val renameFailedMessage: String,
    private val setYoloModeFailedMessage: String,
    private val loadSessionsFailedMessage: String,
    private val refreshSessionsFailedMessage: String,
) : ViewModel() {

    private val _state = MutableStateFlow<UiState<List<Workspace>>>(UiState.Loading)
    val state: StateFlow<UiState<List<Workspace>>> = _state.asStateFlow()

    // Backs the top-bar Inbox badge -- deliberately the same `/feed/pending`
    // count the Inbox screen itself renders (see [isPendingInboxKind]), not
    // workspaces' cmux `has_unread` flag: that flag fires on any new output,
    // so it previously showed a badge count with nothing behind it once
    // opened. Kept stale on a failed fetch rather than reset to 0, same as
    // [state] on a background refresh failure -- see [fetchAndApply].
    private val _pendingCount = MutableStateFlow(0)
    val pendingCount: StateFlow<Int> = _pendingCount.asStateFlow()

    // Surfaced separately from [state] so a failed rename doesn't blow away an
    // already-loaded list (mirrors InboxViewModel's state/actionError split).
    private val _actionError = MutableStateFlow<String?>(null)
    val actionError: StateFlow<String?> = _actionError.asStateFlow()

    // The last create/show/close outcome, for a one-line notice; the screen
    // clears it once shown. Kept apart from [actionError] because a success
    // is worth a line too ("Shown on Mac") and a failure needs a typed
    // reason the screen can word, not raw exception text.
    private val _actionOutcome = MutableStateFlow<ActionOutcome?>(null)
    val actionOutcome: StateFlow<ActionOutcome?> = _actionOutcome.asStateFlow()

    fun dismissActionOutcome() {
        _actionOutcome.value = null
    }

    // True only while a user-initiated refresh (pull gesture or the Refresh
    // button) is in flight -- drives PullToRefreshBox's spinner. Background
    // auto-refresh (see [subscribeToEvents]) deliberately does NOT set this;
    // it should update the list with no visible indicator at all.
    private val _isRefreshing = MutableStateFlow(false)
    val isRefreshing: StateFlow<Boolean> = _isRefreshing.asStateFlow()

    // Guards against overlapping fetches from any source (pull, button, or
    // auto-refresh) -- separate from [_isRefreshing], which is UI-only.
    private var fetchInFlight = false

    // Coalesces bursts of cmux feed events (e.g. many PreToolUse frames during
    // one agent turn) into a single refetch instead of hammering `cmux rpc`.
    private val refreshRequests =
        MutableSharedFlow<Unit>(extraBufferCapacity = 1, onBufferOverflow = BufferOverflow.DROP_OLDEST)

    // Like [refreshRequests] but only the badge-count refetch (GET /feed/pending),
    // and only for `feed` frames -- the pending count cannot move on any other
    // frame type, so non-feed agent activity (terminal output churn etc.)
    // costs one /sessions fetch instead of two full-body requests per burst.
    private val badgeRefreshRequests =
        MutableSharedFlow<Unit>(extraBufferCapacity = 1, onBufferOverflow = BufferOverflow.DROP_OLDEST)

    private val reconnector = SocketReconnector<EventFrame>(
        bridge.relayHealth(),
        monitor = bridge.connectionMonitor(),
        slotCredentials = bridge.slotCredentials(),
    )

    /** Which transport the app is on right now, and whether it is mid-failover
     *  -- rendered by [ConnectionStatusStrip]. Read straight off the shared
     *  process-wide monitor rather than mirrored into local state, so it
     *  reflects the REST path and every socket, not just this screen's. */
    val connectionStatus: StateFlow<ConnectionStatus> = bridge.connectionMonitor().status

    init {
        refresh()
        viewModelScope.launch {
            refreshRequests.debounce(EVENT_REFRESH_DEBOUNCE_MS).collect { autoRefresh() }
        }
        viewModelScope.launch {
            badgeRefreshRequests.debounce(EVENT_REFRESH_DEBOUNCE_MS).collect { refreshPendingCount() }
        }
        subscribeToEvents()
    }

    /** The phone-local custom sort order (see [com.sodre90.cmuxremote.data.WorkspaceOrderStore]). */
    fun loadOrder(): List<String> = workspaceOrder.loadOrder()

    fun saveOrder(order: List<String>) = workspaceOrder.saveOrder(order)

    /** The persisted "Waiting first" sort toggle (see
     *  [com.sodre90.cmuxremote.data.WorkspaceOrderStore]). */
    fun loadSortByAttention(): Boolean = workspaceOrder.loadSortByAttention()

    fun saveSortByAttention(sortByAttention: Boolean) = workspaceOrder.saveSortByAttention(sortByAttention)

    /** Sets a workspace's display title in cmux, then reloads the list so the
     *  new title (cmux's single source of truth for it) comes back fresh. */
    fun renameWorkspace(id: String, title: String) {
        val client = bridge.activeBridge() ?: run {
            _actionError.value = bridgeNotConfiguredMessage
            return
        }
        viewModelScope.launch {
            try {
                client.renameWorkspace(id, title)
                _actionError.value = null
                refresh()
            } catch (e: Exception) {
                _actionError.value = e.message ?: renameFailedMessage
            }
        }
    }

    /** Sets a workspace's YOLO auto-reply mode. Unlike [renameWorkspace], the
     *  bridge echoes back nothing to reconcile (the mode is exactly what we
     *  sent, not cmux-transformed), so this patches the already-loaded list
     *  in place rather than dropping into [UiState.Loading] and reloading
     *  the whole screen. */
    fun setYoloMode(id: String, mode: String) {
        val client = bridge.activeBridge() ?: run {
            _actionError.value = bridgeNotConfiguredMessage
            return
        }
        viewModelScope.launch {
            try {
                client.setYoloMode(id, mode)
                _actionError.value = null
                val current = _state.value
                if (current is UiState.Ready) {
                    _state.value = UiState.Ready(
                        current.data.map { if (it.id == id) it.copy(yoloMode = mode) else it }
                    )
                }
            } catch (e: Exception) {
                _actionError.value = e.message ?: setYoloModeFailedMessage
            }
        }
    }

    /** Directories to offer for a new workspace, from the list as loaded. */
    fun recentDirectories(): List<RecentDirectory> =
        (state.value as? UiState.Ready)?.data?.let(::recentDirectories).orEmpty()

    /** Creates a workspace on the Mac and hands its first terminal's surface
     *  id to [onCreated] so the caller can open it; the list reloads behind. */
    fun createWorkspace(cwd: String, title: String?, onCreated: (surfaceId: String) -> Unit) {
        mutate(onSuccess = null) { client ->
            val created = client.createWorkspace(cwd, title)
            onCreated(created.surfaceId)
            refreshRequests.tryEmit(Unit)
        }
    }

    /** Makes the Mac show a workspace. */
    fun showOnMac(workspaceId: String) {
        mutate(ActionOutcome.ShownOnMac) { it.selectWorkspace(workspaceId) }
    }

    /** Closes a workspace after the screen's confirmation. The row goes at
     *  once and the list refetches silently behind it, the way an event
     *  does -- a hard [refresh] would replace the list with a spinner. */
    fun closeWorkspace(workspaceId: String) {
        mutate(ActionOutcome.WorkspaceClosed) { client ->
            client.closeWorkspace(workspaceId)
            val current = _state.value
            if (current is UiState.Ready) {
                _state.value = UiState.Ready(current.data.filterNot { it.id == workspaceId })
            }
            refreshRequests.tryEmit(Unit)
        }
    }

    private fun mutate(onSuccess: ActionOutcome?, block: suspend (FallbackBridgeClient) -> Unit) {
        val client = bridge.activeBridge() ?: run {
            _actionError.value = bridgeNotConfiguredMessage
            return
        }
        viewModelScope.launch {
            try {
                block(client)
                _actionOutcome.value = onSuccess
            } catch (e: Exception) {
                _actionOutcome.value = ActionOutcome.Failed(actionFailureOf(e), e.message)
            }
        }
    }

    fun refresh() {
        val client = bridge.activeBridge()
        if (client == null) {
            _state.value = UiState.Error(bridgeNotConfiguredMessage)
            return
        }
        _state.value = UiState.Loading
        viewModelScope.launch {
            _state.value = try {
                UiState.Ready(client.sessions())
            } catch (e: Exception) {
                UiState.Error(e.message ?: loadSessionsFailedMessage)
            }
            refreshPendingCount(client)
        }
    }

    /** User-initiated refresh (pull-to-refresh gesture or the Refresh button)
     *  -- refetches without dropping the list into [UiState.Loading], and
     *  shows [isRefreshing] so PullToRefreshBox's spinner appears. Skips if a
     *  fetch is already in flight. */
    fun userRefresh() {
        val client = bridge.activeBridge() ?: run {
            _actionError.value = bridgeNotConfiguredMessage
            return
        }
        if (fetchInFlight) return
        viewModelScope.launch {
            fetchInFlight = true
            _isRefreshing.value = true
            try {
                fetchAndApply(client)
            } finally {
                _isRefreshing.value = false
                fetchInFlight = false
            }
        }
    }

    /** Background refresh triggered by [subscribeToEvents] -- same non-
     *  blocking refetch as [userRefresh] but never touches [isRefreshing],
     *  so cmux agent activity the user didn't ask about never pops the
     *  pull-to-refresh spinner. */
    private fun autoRefresh() {
        val client = bridge.activeBridge() ?: return
        if (fetchInFlight) return
        viewModelScope.launch {
            fetchInFlight = true
            try {
                fetchAndApply(client)
            } finally {
                fetchInFlight = false
            }
        }
    }

    private suspend fun fetchAndApply(client: FallbackBridgeClient) {
        try {
            _state.value = UiState.Ready(client.sessions())
            _actionError.value = null
        } catch (e: Exception) {
            _actionError.value = e.message ?: refreshSessionsFailedMessage
        }
        refreshPendingCount(client)
    }

    private suspend fun refreshPendingCount(client: FallbackBridgeClient) {
        _pendingCount.value = runCatching { client.pendingFeed() }
            .getOrNull()
            ?.count { isPendingInboxKind(it.kind) }
            ?: _pendingCount.value
    }

    /** Event-driven badge refetch -- same shape as [refreshPendingCount] but
     *  resolving its own client, so the debounced collector can call it
     *  without re-checking pairing. */
    private suspend fun refreshPendingCount() {
        val client = bridge.activeBridge() ?: return
        refreshPendingCount(client)
    }

    // Re-fetch on cmux agent activity: SessionStart/SessionEnd change which
    // workspaces exist, Notification/Stop change attention + preview. Mirrors
    // InboxViewModel's reconnect-with-backoff loop over the same /events
    // socket.
    //
    // The whole subscription is keyed on app foreground: viewModelScope keeps
    // running with the screen off, and without this gate the open socket's
    // 20s keepalive pings plus a /sessions+/feed/pending refetch pair per
    // event burst would burn cellular data all day in the user's pocket.
    // collectLatest tears the reconnect loop (and its socket, via the flow's
    // awaitClose) down on background and rebuilds it on return; push covers
    // attention meanwhile. Tests default [BridgeGateway.appForeground] to
    // true, so they exercise the foreground branch unchanged.
    private fun subscribeToEvents() {
        if (!bridge.anyBridgeConfigured()) return
        viewModelScope.launch {
            bridge.appForeground().collectLatest { foreground ->
                if (!foreground) return@collectLatest
                reconnector.run(
                    openSocket = { slot, onOpen -> bridge.eventsSocket(slot)?.connect(onOpen) },
                    // catch up on anything missed while disconnected -- including
                    // the whole background gap, so both fetches re-run here.
                    onBeforeReconnect = {
                        refreshRequests.tryEmit(Unit)
                        badgeRefreshRequests.tryEmit(Unit)
                    },
                ) { frame ->
                    if (frame.type != "heartbeat") {
                        refreshRequests.tryEmit(Unit)
                        if (isPendingSetChangeSignal(frame.type)) badgeRefreshRequests.tryEmit(Unit)
                    }
                    true
                }
            }
        }
    }

    private companion object {
        const val EVENT_REFRESH_DEBOUNCE_MS = 800L
    }
}
