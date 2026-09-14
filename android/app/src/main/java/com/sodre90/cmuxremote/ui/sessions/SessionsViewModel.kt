package com.sodre90.cmuxremote.ui.sessions

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.sodre90.cmuxremote.data.BridgeGateway
import com.sodre90.cmuxremote.data.ConnectionStatus
import com.sodre90.cmuxremote.data.FallbackBridgeClient
import com.sodre90.cmuxremote.data.SocketReconnector
import com.sodre90.cmuxremote.data.WorkspaceOrderGateway
import com.sodre90.cmuxremote.model.EventFrame
import com.sodre90.cmuxremote.model.HostInfo
import com.sodre90.cmuxremote.model.Workspace
import com.sodre90.cmuxremote.ui.UiState
import com.sodre90.cmuxremote.ui.inbox.isPendingInboxKind
import com.sodre90.cmuxremote.ui.layout.PlacementController
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

    // Backs the top-bar Inbox badge -- deliberately the count of what the
    // Inbox screen itself renders (see [isPendingInboxKind]), not workspaces'
    // cmux `has_unread` flag: that flag fires on any new output, so it
    // previously showed a badge count with nothing behind it once opened.
    // Kept stale on a failed fetch rather than reset to 0, same as [state] on
    // a background refresh failure -- see [applyPendingCount].
    private val _pendingCount = MutableStateFlow(0)
    val pendingCount: StateFlow<Int> = _pendingCount.asStateFlow()

    /** What the selected host reports about itself -- a cmux host until the
     *  first fetch answers. The screen hides the Inbox and YOLO controls for a
     *  host whose feed capability is off (a tmux host has no agent feed). */
    val hostInfo: StateFlow<HostInfo> = bridge.activeBridge()?.hostInfo ?: MutableStateFlow(HostInfo())

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

    /** The placement sheet's state and actions; a created pane refetches
     *  the list so the new one is in it when the terminal comes back. */
    val placement = PlacementController(
        scope = viewModelScope,
        client = { bridge.activeBridge() },
        afterCreate = { refreshRequests.tryEmit(Unit) },
    )

    fun openPlacement(ws: Workspace) {
        placement.open(ws.id, ws.terminals.associate { it.id to it.title })
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

    // This ViewModel outlives its screen on the back stack while a terminal
    // is open, and the event stream keeps asking it to refetch a list nobody
    // is looking at (measured: eleven /sessions in seventy seconds behind one
    // busy pane, cmux-app-ocd). Events seen while away collapse into one
    // refetch on return. Starts in view so a bare ViewModel refreshes.
    private var listInView = true
    private var refetchWhenShown = false

    /** The sessions screen entered composition. */
    fun listShown() {
        listInView = true
        if (refetchWhenShown) {
            refetchWhenShown = false
            autoRefresh()
        }
    }

    /** The sessions screen left composition (a terminal or the Inbox is on top). */
    fun listHidden() {
        listInView = false
    }

    // Coalesces bursts of cmux feed events (e.g. many PreToolUse frames during
    // one agent turn) into a single refetch instead of hammering `cmux rpc`.
    private val refreshRequests =
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
            try {
                val response = client.sessions()
                _state.value = UiState.Ready(response.workspaces)
                applyPendingCount(client, response.pendingCount)
            } catch (e: Exception) {
                _state.value = UiState.Error(e.message ?: loadSessionsFailedMessage)
            }
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
        if (!listInView) {
            refetchWhenShown = true
            return
        }
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
            val response = client.sessions()
            _state.value = UiState.Ready(response.workspaces)
            _actionError.value = null
            applyPendingCount(client, response.pendingCount)
        } catch (e: Exception) {
            _actionError.value = e.message ?: refreshSessionsFailedMessage
        }
    }

    /** The Inbox badge rides on the list fetch; only a bridge that left it out
     *  (no feed, feed unreadable, or too old to count) costs a /feed/pending
     *  request, and a failed one keeps the badge as it was. */
    private suspend fun applyPendingCount(client: FallbackBridgeClient, counted: Int?) {
        if (counted != null) {
            _pendingCount.value = counted
            return
        }
        if (!client.hostInfo.value.capabilities.feed) {
            _pendingCount.value = 0
            return
        }
        _pendingCount.value = runCatching { client.pendingFeed() }
            .getOrNull()
            ?.count { isPendingInboxKind(it.kind) }
            ?: _pendingCount.value
    }

    // Re-fetch on cmux agent activity: SessionStart/SessionEnd change which
    // workspaces exist, Notification/Stop change attention + preview. Mirrors
    // InboxViewModel's reconnect-with-backoff loop over the same /events
    // socket.
    //
    // The whole subscription is keyed on app foreground: viewModelScope keeps
    // running with the screen off, and without this gate the open socket's
    // 20s keepalive pings plus a /sessions refetch per event burst would
    // burn cellular data all day in the user's pocket.
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
                    // the whole background gap.
                    onBeforeReconnect = { refreshRequests.tryEmit(Unit) },
                ) { frame ->
                    if (frame.type != "heartbeat") refreshRequests.tryEmit(Unit)
                    true
                }
            }
        }
    }

    private companion object {
        const val EVENT_REFRESH_DEBOUNCE_MS = 800L
    }
}
