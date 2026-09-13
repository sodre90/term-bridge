package com.sodre90.cmuxremote.ui.sessions

import com.goterl.lazysodium.LazySodiumJava
import com.goterl.lazysodium.SodiumJava
import com.sodre90.cmuxremote.data.BridgeClient
import com.sodre90.cmuxremote.data.BridgeGateway
import com.sodre90.cmuxremote.data.ConnectionMonitor
import com.sodre90.cmuxremote.data.ConnectionSlot
import com.sodre90.cmuxremote.data.EventsSocket
import com.sodre90.cmuxremote.data.FallbackBridgeClient
import com.sodre90.cmuxremote.data.RelayHealth
import com.sodre90.cmuxremote.data.SlotCredentialHealth
import com.sodre90.cmuxremote.data.SlotCredentials
import com.sodre90.cmuxremote.data.TerminalSocket
import com.sodre90.cmuxremote.data.WorkspaceOrderGateway
import com.sodre90.cmuxremote.data.e2e.Cipher
import com.sodre90.cmuxremote.data.e2e.DIR_AGENT_TO_DEVICE
import com.sodre90.cmuxremote.data.e2e.PairedSession
import com.sodre90.cmuxremote.data.e2e.ReplayRejectedException
import com.sodre90.cmuxremote.data.e2e.ReplayWindow
import com.sodre90.cmuxremote.data.e2e.nonce
import com.sodre90.cmuxremote.model.PanePlacement
import com.sodre90.cmuxremote.ui.TestViewModelHost
import com.sodre90.cmuxremote.ui.UiState
import com.sodre90.cmuxremote.ui.layout.PlacementState
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import okhttp3.OkHttpClient
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okhttp3.mockwebserver.Dispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import okio.ByteString.Companion.toByteString
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import java.nio.ByteBuffer
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicReference

/** A [PairedSession] fixed to [secret] with no persistence, so frames pushed
 *  from the fake server can be decrypted by a real [EventsSocket]. Only the
 *  receive side is used by these tests. */
private class RecordingSession(private val secret: ByteArray) : PairedSession {
    private var window = ReplayWindow()
    override fun sharedSecret(): ByteArray = secret
    override fun nextSendCounter(): Long = error("not used by these tests")
    override fun <T> validateAndCommitRecvCounter(n: Long, decrypt: (ByteArray) -> T): T {
        if (!window.canAccept(n)) throw ReplayRejectedException(n, window.highestSeen)
        return decrypt(secret).also { window = window.commit(n) }
    }
}

private class FakeWorkspaceOrderGateway : WorkspaceOrderGateway {
    private var order: List<String> = emptyList()
    private var sortByAttention = false
    override fun loadOrder(): List<String> = order
    override fun saveOrder(order: List<String>) {
        this.order = order
    }
    override fun loadSortByAttention(): Boolean = sortByAttention
    override fun saveSortByAttention(sortByAttention: Boolean) {
        this.sortByAttention = sortByAttention
    }
}

/** A [BridgeGateway] whose bridge/events wiring can be swapped in after
 *  construction -- lets a test skip the automatic init-time refresh()/
 *  subscribeToEvents() by starting unconfigured, or start pre-wired to a
 *  [MockWebServer] when a test needs the real events flow. Only RELAY is
 *  ever configured; DIRECT always reports unconfigured, matching a
 *  single-slot pairing. */
private class FakeSessionsBridgeGateway : BridgeGateway {
    var bridge: FallbackBridgeClient? = null
    var events: EventsSocket? = null
    val monitor = ConnectionMonitor()

    // Mutable so tests can drive the background/foreground transitions the
    // real app gets from MainActivity's onStart/onStop.
    val foreground = MutableStateFlow(true)

    override fun activeBridge(): FallbackBridgeClient? = bridge
    override fun anyBridgeConfigured(): Boolean = bridge != null
    override fun eventsSocket(slot: ConnectionSlot): EventsSocket? = if (slot == ConnectionSlot.RELAY) events else null
    override fun terminalSocket(slot: ConnectionSlot, surfaceId: String): TerminalSocket? = null
    override fun relayHealth(): RelayHealth = RelayHealth()
    override fun connectionMonitor(): ConnectionMonitor = monitor
    override fun slotCredentials(): SlotCredentials = SlotCredentials()
    override fun slotCredentialHealth(): SlotCredentialHealth = SlotCredentialHealth()
    override fun appForeground(): StateFlow<Boolean> = foreground
}

/**
 * Covers the two pieces of SessionsViewModel's refresh machinery that had no
 * JVM tests before: the `fetchInFlight` overlap guard shared by
 * userRefresh/autoRefresh (and refresh()'s deliberate exemption from it),
 * and the debounced coalescing of cmux event-driven refetches. All
 * synchronization is on observable side effects (request counts / VM state)
 * via polling rather than assumptions about coroutine dispatch order, since
 * the real network calls run on the real Dispatchers.IO thread pool
 * regardless of which dispatcher backs Dispatchers.Main in the test.
 */
class SessionsViewModelTest {

    private lateinit var server: MockWebServer
    private lateinit var host: TestViewModelHost
    private val orderGateway = FakeWorkspaceOrderGateway()
    private val secret = ByteArray(32) { it.toByte() }
    private val cipher = Cipher(LazySodiumJava(SodiumJava()))

    @Before
    fun setUp() {
        host = TestViewModelHost()
        server = MockWebServer().apply { start() }
    }

    // Ordered: the ViewModels' refresh collectors and events loop stop before
    // the server they talk to goes away.
    @After
    fun tearDown() {
        host.clearViewModels()
        server.shutdown()
    }

    private fun waitUntil(timeoutMs: Long = 3_000, block: () -> Boolean) {
        val deadline = System.currentTimeMillis() + timeoutMs
        while (System.currentTimeMillis() < deadline) {
            if (block()) return
            Thread.sleep(20)
        }
        assertTrue("condition not met within ${timeoutMs}ms", block())
    }

    private fun bridgeFor(server: MockWebServer) = FallbackBridgeClient(
        primary = { BridgeClient(OkHttpClient(), server.url("/").toString()) },
        fallback = { null },
    )

    // None of these tests assert on VM-owned error text, so the injected
    // messages are arbitrary fixed strings, not the real strings.xml values
    // (see CmuxNavHost's SESSIONS route for those).
    private fun sessionsViewModel(bridge: BridgeGateway, workspaceOrder: WorkspaceOrderGateway) =
        host.hold(SessionsViewModel::class.java) {
            SessionsViewModel(
                bridge = bridge,
                workspaceOrder = workspaceOrder,
                bridgeNotConfiguredMessage = "Bridge not configured",
                renameFailedMessage = "Rename failed",
                setYoloModeFailedMessage = "Setting YOLO mode failed",
                loadSessionsFailedMessage = "Failed to load sessions",
                refreshSessionsFailedMessage = "Failed to refresh sessions",
            )
        }

    private fun frameFor(json: String, counter: Long): okio.ByteString {
        val ct = cipher.seal(secret, nonce(DIR_AGENT_TO_DEVICE, counter), json.toByteArray(Charsets.UTF_8))
        val frame = ByteArray(8 + ct.size)
        ByteBuffer.wrap(frame, 0, 8).putLong(counter)
        ct.copyInto(frame, 8)
        return frame.toByteString()
    }

    @Test
    fun userRefreshSkipsAConcurrentCallWhileAFetchIsAlreadyInFlight() {
        // Only /sessions is counted/gated -- fetchAndApply also fires a
        // /feed/pending request (for the Inbox badge count) that must not be
        // mistaken for a second /sessions call by this test's assertions.
        val requestCount = AtomicInteger(0)
        val gate = CountDownLatch(1)
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse {
                if (request.path != "/sessions") return MockResponse().setBody("""{"items":[]}""")
                requestCount.incrementAndGet()
                gate.await()
                return MockResponse().setBody("""{"workspaces":[]}""")
            }
        }

        // Starts unconfigured so init's own refresh() (not gated by
        // fetchInFlight at all) never touches the server -- this test is
        // only about userRefresh deduping against itself.
        val gw = FakeSessionsBridgeGateway()
        val vm = sessionsViewModel(gw, orderGateway)
        gw.bridge = bridgeFor(server)

        vm.userRefresh()
        waitUntil { requestCount.get() == 1 } // in flight, blocked on the gate

        vm.userRefresh() // must be a no-op: a fetch is already in flight
        Thread.sleep(200) // give a wrongly-issued second request a chance to arrive
        assertEquals(1, requestCount.get())

        gate.countDown()
        waitUntil { !vm.isRefreshing.value }

        vm.userRefresh() // fetchInFlight is clear again -> must proceed
        waitUntil { requestCount.get() == 2 }
    }

    @Test
    fun refreshProceedsEvenWhileAUserRefreshIsInFlight() {
        // Only /sessions is counted/gated -- see the same note in
        // userRefreshSkipsAConcurrentCallWhileAFetchIsAlreadyInFlight above.
        val requestCount = AtomicInteger(0)
        val gate = CountDownLatch(1)
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse {
                if (request.path != "/sessions") return MockResponse().setBody("""{"items":[]}""")
                requestCount.incrementAndGet()
                gate.await()
                return MockResponse().setBody("""{"workspaces":[]}""")
            }
        }

        val gw = FakeSessionsBridgeGateway()
        val vm = sessionsViewModel(gw, orderGateway)
        gw.bridge = bridgeFor(server)

        vm.userRefresh()
        waitUntil { requestCount.get() == 1 } // in flight, blocked on the gate

        // refresh() is NOT gated by fetchInFlight -- it's the "hard reload"
        // path (init, post-rename) and must issue its own request even
        // though userRefresh's fetch hasn't resolved yet.
        vm.refresh()
        waitUntil { requestCount.get() == 2 }

        gate.countDown()
        waitUntil { vm.state.value is UiState.Ready }
    }

    @Test
    fun rapidEventFramesCoalesceIntoASingleAutoRefresh() {
        val requestCount = AtomicInteger(0)
        val socketRef = AtomicReference<WebSocket>()
        val socketOpened = CountDownLatch(1)
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse = when (request.path) {
                "/events" -> MockResponse().withWebSocketUpgrade(
                    object : WebSocketListener() {
                        override fun onOpen(webSocket: WebSocket, response: Response) {
                            socketRef.set(webSocket)
                            socketOpened.countDown()
                        }
                    },
                )
                "/sessions" -> {
                    requestCount.incrementAndGet()
                    MockResponse().setBody("""{"workspaces":[]}""")
                }
                else -> MockResponse().setResponseCode(404)
            }
        }

        val gw = FakeSessionsBridgeGateway().apply {
            bridge = bridgeFor(server)
            events = EventsSocket(OkHttpClient(), server.url("/").toString(), RecordingSession(secret), cipher)
        }
        sessionsViewModel(gw, orderGateway) // init's refresh() + subscribeToEvents()

        assertTrue(socketOpened.await(5, TimeUnit.SECONDS))
        waitUntil { requestCount.get() >= 1 } // init's own refresh()
        val baseline = requestCount.get()

        val socket = socketRef.get()
        repeat(3) { i -> socket.send(frameFor("""{"type":"feed","name":"feed.updated"}""", i.toLong())) }

        Thread.sleep(300) // well inside the 800ms debounce window -- must not have fired yet
        assertEquals(baseline, requestCount.get())

        waitUntil(timeoutMs = 3_000) { requestCount.get() == baseline + 1 }
        Thread.sleep(500) // give a wrongly-uncoalesced extra trigger a chance to show up
        assertEquals(baseline + 1, requestCount.get())
    }

    @Test
    fun autoRefreshSkipsWhileAUserRefreshIsInFlight() {
        val requestCount = AtomicInteger(0)
        val gate = CountDownLatch(1)
        val gateEnabled = AtomicBoolean(false)
        val socketRef = AtomicReference<WebSocket>()
        val socketOpened = CountDownLatch(1)
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse = when (request.path) {
                "/events" -> MockResponse().withWebSocketUpgrade(
                    object : WebSocketListener() {
                        override fun onOpen(webSocket: WebSocket, response: Response) {
                            socketRef.set(webSocket)
                            socketOpened.countDown()
                        }
                    },
                )
                "/sessions" -> {
                    requestCount.incrementAndGet()
                    if (gateEnabled.get()) gate.await()
                    MockResponse().setBody("""{"workspaces":[]}""")
                }
                else -> MockResponse().setResponseCode(404)
            }
        }

        val gw = FakeSessionsBridgeGateway().apply {
            bridge = bridgeFor(server)
            events = EventsSocket(OkHttpClient(), server.url("/").toString(), RecordingSession(secret), cipher)
        }
        val vm = sessionsViewModel(gw, orderGateway)

        assertTrue(socketOpened.await(5, TimeUnit.SECONDS))
        waitUntil { requestCount.get() >= 1 } // init's own refresh(), ungated
        val socket = socketRef.get()

        // Put a userRefresh fetch in flight and hold it there.
        gateEnabled.set(true)
        vm.userRefresh()
        waitUntil { requestCount.get() >= 2 } // userRefresh's request has arrived and is now blocked
        val blockedAt = requestCount.get()

        // A cmux event fires while that fetch is still in flight: after the
        // debounce window elapses, autoRefresh's fetchInFlight guard must
        // skip it entirely -- no new request at all, not even a blocked one.
        socket.send(frameFor("""{"type":"feed","name":"feed.updated"}""", 0L))
        Thread.sleep(1_200) // past the 800ms debounce window
        assertEquals(blockedAt, requestCount.get())

        // Release the blocked userRefresh fetch; fetchInFlight clears.
        gateEnabled.set(false)
        gate.countDown()
        waitUntil { !vm.isRefreshing.value }
        assertEquals(blockedAt, requestCount.get()) // still just the one blocked request

        // A later event, once nothing is in flight, must trigger autoRefresh normally.
        socket.send(frameFor("""{"type":"feed","name":"feed.updated"}""", 1L))
        waitUntil(timeoutMs = 3_000) { requestCount.get() == blockedAt + 1 }
        assertTrue(vm.state.value is UiState.Ready)
    }

    @Test
    fun eventsSocketStaysDownWhileBackgroundedAndResumesOnForeground() {
        val requestCount = AtomicInteger(0)
        val openCount = AtomicInteger(0)
        val socketRef = AtomicReference<WebSocket>()
        val socketOpened = CountDownLatch(1)
        val socketClosed = CountDownLatch(1)
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse = when (request.path) {
                "/events" -> MockResponse().withWebSocketUpgrade(
                    object : WebSocketListener() {
                        override fun onOpen(webSocket: WebSocket, response: Response) {
                            openCount.incrementAndGet()
                            socketRef.set(webSocket)
                            socketOpened.countDown()
                        }

                        override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                            socketClosed.countDown()
                        }

                        override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                            socketClosed.countDown()
                        }

                        override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                            socketClosed.countDown()
                        }
                    },
                )
                "/sessions" -> {
                    requestCount.incrementAndGet()
                    MockResponse().setBody("""{"workspaces":[]}""")
                }
                else -> MockResponse().setResponseCode(404) // /feed/pending; its failure is swallowed
            }
        }

        val gw = FakeSessionsBridgeGateway().apply {
            bridge = bridgeFor(server)
            events = EventsSocket(OkHttpClient(), server.url("/").toString(), RecordingSession(secret), cipher)
        }
        gw.foreground.value = false // pocketed before the VM even exists

        sessionsViewModel(gw, orderGateway)

        // Backgrounded: the subscription must not dial /events at all -- this
        // is what stops keepalive pings and event refetches burning cellular
        // data with the screen off.
        Thread.sleep(500)
        assertEquals(0, openCount.get())

        // Coming to the foreground opens the socket and event-driven refresh works.
        gw.foreground.value = true
        assertTrue(socketOpened.await(5, TimeUnit.SECONDS))
        waitUntil { requestCount.get() >= 1 } // init's own refresh()
        val baseline = requestCount.get()
        socketRef.get().send(frameFor("""{"type":"feed","name":"feed.updated"}""", 0L))
        waitUntil(timeoutMs = 3_000) { requestCount.get() == baseline + 1 }

        // Backgrounding again tears the socket down...
        gw.foreground.value = false
        assertTrue(socketClosed.await(5, TimeUnit.SECONDS))

        // ...and nothing re-dials while backgrounded: a still-running loop
        // would have redialled well within its 5s backoff cap.
        Thread.sleep(6_000)
        assertEquals(1, openCount.get())
    }

    @Test
    fun sortByAttentionReadsAndWritesThroughTheOrderGateway() {
        val vm = sessionsViewModel(FakeSessionsBridgeGateway(), orderGateway)

        assertEquals(false, vm.loadSortByAttention()) // unset -> off, matching WorkspaceOrderStore's default

        vm.saveSortByAttention(true)

        assertEquals(true, vm.loadSortByAttention())
        // Reaches the underlying gateway, not just VM-local state.
        assertEquals(true, orderGateway.loadSortByAttention())
    }

    private val twoWorkspaces = """{"workspaces":[
        {"id":"ws-a","cwd":"/Users/me/prj/a","title":"A","terminals":[{"id":"s-a","title":"A"}]},
        {"id":"ws-b","cwd":"/Users/me/prj/b","title":"B","terminals":[{"id":"s-b","title":"B"}]}]}"""

    /** A server that lists [twoWorkspaces] and answers the mutation routes
     *  with [mutation], recording every non-list request. */
    private fun mutationServer(mutation: (RecordedRequest) -> MockResponse): MutableList<RecordedRequest> {
        val seen = mutableListOf<RecordedRequest>()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse = when (request.path) {
                "/sessions" -> if (request.method == "GET") {
                    MockResponse().setBody(twoWorkspaces)
                } else {
                    synchronized(seen) { seen.add(request) }
                    mutation(request)
                }
                "/feed/pending" -> MockResponse().setBody("""{"items":[]}""")
                else -> {
                    synchronized(seen) { seen.add(request) }
                    mutation(request)
                }
            }
        }
        return seen
    }

    private fun loadedViewModel(): SessionsViewModel {
        val gw = FakeSessionsBridgeGateway()
        gw.bridge = bridgeFor(server)
        val vm = sessionsViewModel(gw, orderGateway)
        waitUntil { vm.state.value is UiState.Ready }
        return vm
    }

    @Test
    fun createWorkspaceHandsTheNewSurfaceToTheCallerAndReloads() {
        val seen = mutationServer {
            MockResponse().setBody("""{"workspace_id":"ws-new","surface_id":"s-new"}""")
        }
        val vm = loadedViewModel()
        assertEquals(listOf("/Users/me/prj/a", "/Users/me/prj/b"), vm.recentDirectories().map { it.path })

        val opened = AtomicReference<String>()
        vm.createWorkspace("/Users/me/prj/c", "C", onCreated = { opened.set(it) })

        waitUntil { opened.get() == "s-new" }
        val create = synchronized(seen) { seen.single() }
        assertEquals("POST", create.method)
        assertEquals("""{"cwd":"/Users/me/prj/c","title":"C"}""", create.body.readUtf8())
        assertEquals(null, vm.actionOutcome.value)
    }

    @Test
    fun closeWorkspaceDropsTheRowBeforeTheReloadAndSaysSo() {
        val lists = AtomicInteger(0)
        val reloadGate = CountDownLatch(1)
        val deletes = mutableListOf<RecordedRequest>()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse = when {
                request.path == "/sessions" -> {
                    // The reload after the close waits until the test has
                    // looked at the list, so the immediate drop is observable.
                    if (lists.incrementAndGet() > 1) reloadGate.await()
                    MockResponse().setBody(twoWorkspaces)
                }
                request.method == "DELETE" -> {
                    synchronized(deletes) { deletes.add(request) }
                    MockResponse().setBody("""{"ok":true}""")
                }
                else -> MockResponse().setBody("""{"items":[]}""")
            }
        }
        val vm = loadedViewModel()

        vm.closeWorkspace("ws-a")

        waitUntil { vm.actionOutcome.value == ActionOutcome.WorkspaceClosed }
        assertEquals(listOf("ws-b"), (vm.state.value as UiState.Ready).data.map { it.id })
        assertEquals("/sessions/ws-a", synchronized(deletes) { deletes.single().path })
        reloadGate.countDown()
    }

    @Test
    fun aBridgeWithoutTheRoutesIsReportedAsTooOld() {
        mutationServer { MockResponse().setResponseCode(404).setBody("404 page not found") }
        val vm = loadedViewModel()

        vm.showOnMac("ws-a")

        waitUntil { vm.actionOutcome.value is ActionOutcome.Failed }
        assertEquals(ActionFailure.BRIDGE_TOO_OLD, (vm.actionOutcome.value as ActionOutcome.Failed).failure)
        vm.dismissActionOutcome()
        assertEquals(null, vm.actionOutcome.value)
    }

    @Test
    fun showOnMacSelectsTheWorkspaceAndSaysSo() {
        val seen = mutationServer { MockResponse().setBody("""{"ok":true}""") }
        val vm = loadedViewModel()

        vm.showOnMac("ws-b")

        waitUntil { vm.actionOutcome.value == ActionOutcome.ShownOnMac }
        val select = synchronized(seen) { seen.single() }
        assertEquals("/sessions/ws-b/select", select.path)
        assertEquals("{}", select.body.readUtf8())
    }

    @Test
    fun newPaneFromTheListLoadsTheLayoutWithTitlesThenCreatesAndReloads() {
        val lists = AtomicInteger(0)
        val panes = AtomicInteger(0)
        val seen = mutableListOf<RecordedRequest>()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse = when (request.path) {
                "/sessions" -> {
                    lists.incrementAndGet()
                    MockResponse().setBody(twoWorkspaces)
                }
                "/sessions/ws-b/layout" -> MockResponse().setBody(
                    """{"estimated":true,"panes":[{"id":"p-b","x":0,"y":0,"w":1,"h":1,"focused":true,
                        "surface_ids":["s-b"],"selected_surface_id":"s-b"}]}""",
                )
                "/sessions/ws-b/panes" -> {
                    synchronized(seen) { seen.add(request) }
                    if (panes.incrementAndGet() == 1) {
                        MockResponse().setResponseCode(502).setBody("""{"error":"cmux surface.split failed"}""")
                    } else {
                        MockResponse().setBody("""{"surface_id":"s-new","pane_id":"p-new"}""")
                    }
                }
                else -> MockResponse().setBody("""{"items":[]}""")
            }
        }
        val vm = loadedViewModel()
        val listsBefore = lists.get()

        vm.openPlacement((vm.state.value as UiState.Ready).data.single { it.id == "ws-b" })

        waitUntil { vm.placement.state.value is PlacementState.Ready }
        val ready = vm.placement.state.value as PlacementState.Ready
        assertEquals(mapOf("s-b" to "B"), ready.titles)
        assertEquals(true, ready.layout.estimated)

        val opened = AtomicReference<String>()
        vm.placement.createPane("s-b", PanePlacement.LEFT, onCreated = { opened.set(it) })

        waitUntil { (vm.placement.state.value as? PlacementState.Ready)?.error != null }
        assertEquals(ActionFailure.OTHER, (vm.placement.state.value as PlacementState.Ready).error?.failure)
        assertEquals(null, opened.get())

        vm.placement.createPane("s-b", PanePlacement.LEFT, onCreated = { opened.set(it) })

        waitUntil { opened.get() == "s-new" }
        assertEquals(null, vm.placement.state.value)
        assertEquals(
            listOf("""{"surface_id":"s-b","placement":"left"}""", """{"surface_id":"s-b","placement":"left"}"""),
            synchronized(seen) { seen.map { it.body.readUtf8() } },
        )
        waitUntil { lists.get() > listsBefore }
    }

    @Test
    fun aSecondTapWhileTheSplitIsInFlightSendsNothing() {
        val creates = AtomicInteger(0)
        val createGate = CountDownLatch(1)
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse = when (request.path) {
                "/sessions" -> MockResponse().setBody(twoWorkspaces)
                "/sessions/ws-a/layout" -> MockResponse().setBody(
                    """{"panes":[{"id":"p-a","x":0,"y":0,"w":1,"h":1,
                        "surface_ids":["s-a"],"selected_surface_id":"s-a"}]}""",
                )
                "/sessions/ws-a/panes" -> {
                    creates.incrementAndGet()
                    createGate.await(3, TimeUnit.SECONDS)
                    MockResponse().setBody("""{"surface_id":"s-new","pane_id":"p-new"}""")
                }
                else -> MockResponse().setBody("""{"items":[]}""")
            }
        }
        val vm = loadedViewModel()
        vm.openPlacement((vm.state.value as UiState.Ready).data.first())
        waitUntil { vm.placement.state.value is PlacementState.Ready }

        val opened = AtomicInteger(0)
        vm.placement.createPane("s-a", PanePlacement.RIGHT, onCreated = { opened.incrementAndGet() })
        waitUntil { (vm.placement.state.value as? PlacementState.Ready)?.busy == true }
        vm.placement.createPane("s-a", PanePlacement.RIGHT, onCreated = { opened.incrementAndGet() })
        createGate.countDown()

        waitUntil { opened.get() == 1 }
        assertEquals(1, creates.get())
        assertEquals(null, vm.placement.state.value)
    }

    @Test
    fun aLayoutTheBridgeCannotGiveLeavesTheSheetWithTheReason() {
        mutationServer { MockResponse().setResponseCode(404).setBody("404 page not found") }
        val vm = loadedViewModel()

        vm.openPlacement((vm.state.value as UiState.Ready).data.first())

        waitUntil { vm.placement.state.value is PlacementState.Failed }
        val failed = vm.placement.state.value as PlacementState.Failed
        assertEquals(ActionFailure.BRIDGE_TOO_OLD, failed.outcome.failure)
        vm.placement.close()
        assertEquals(null, vm.placement.state.value)
    }
}
