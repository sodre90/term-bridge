package com.sodre90.cmuxremote.ui.terminal

import com.sodre90.cmuxremote.data.BridgeClient
import com.sodre90.cmuxremote.data.BridgeGateway
import com.sodre90.cmuxremote.data.ConnectionMonitor
import com.sodre90.cmuxremote.data.ConnectionSlot
import com.sodre90.cmuxremote.data.DEFAULT_TERMINAL_POLL_MS
import com.sodre90.cmuxremote.data.EventsSocket
import com.sodre90.cmuxremote.data.FallbackBridgeClient
import com.sodre90.cmuxremote.data.RelayHealth
import com.sodre90.cmuxremote.data.SlotCredentialHealth
import com.sodre90.cmuxremote.data.SlotCredentials
import com.sodre90.cmuxremote.data.TerminalDisplayGateway
import com.sodre90.cmuxremote.data.TerminalSocket
import com.sodre90.cmuxremote.model.PanePlacement
import com.sodre90.cmuxremote.ui.TestViewModelHost
import com.sodre90.cmuxremote.ui.layout.PlacementState
import com.sodre90.cmuxremote.ui.sessions.ActionOutcome
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.Dispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicReference

/**
 * The pane actions behind the terminal's overflow menu. The terminal socket
 * is never provided (the fake gateway has none), so the screen sits in its
 * error state throughout; the actions only need the REST client and the
 * workspace the sessions list says owns this surface.
 */
class TerminalPaneActionsTest {

    private lateinit var server: MockWebServer
    private lateinit var host: TestViewModelHost
    private val seen = mutableListOf<RecordedRequest>()
    private var sessionsHost = ""

    private class FakeGateway(private val bridge: FallbackBridgeClient) : BridgeGateway {
        override fun activeBridge(): FallbackBridgeClient = bridge
        override fun anyBridgeConfigured(): Boolean = true
        override fun eventsSocket(slot: ConnectionSlot): EventsSocket? = null
        override fun terminalSocket(slot: ConnectionSlot, surfaceId: String): TerminalSocket? = null
        override fun relayHealth(): RelayHealth = RelayHealth()
        override fun connectionMonitor(): ConnectionMonitor = ConnectionMonitor()
        override fun slotCredentials(): SlotCredentials = SlotCredentials()
        override fun slotCredentialHealth(): SlotCredentialHealth = SlotCredentialHealth()
        override fun appForeground(): StateFlow<Boolean> = MutableStateFlow(true)
    }

    private object NoDisplayPrefs : TerminalDisplayGateway {
        override fun loadFontZoom(): Float = 1f
        override fun saveFontZoom(zoom: Float) = Unit
        override fun loadTerminalPollMs(metered: Boolean): Int = DEFAULT_TERMINAL_POLL_MS
        override fun saveTerminalPollMs(metered: Boolean, ms: Int) = Unit
    }

    @Before
    fun setUp() {
        host = TestViewModelHost()
        server = MockWebServer().apply { start() }
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse = when (request.path) {
                "/sessions" -> MockResponse().setBody(
                    """{"workspaces":[{"id":"ws-a","cwd":"/x","title":"A","terminals":[{"id":"s-other"}]},
                        {"id":"ws-b","cwd":"/y","title":"B","terminals":[{"id":"s-here","title":"B shell"}]}]
                        $sessionsHost}""",
                )
                "/sessions/ws-b/panes" -> recorded(request, """{"surface_id":"s-new","pane_id":"p-b"}""")
                "/sessions/ws-b/layout" -> MockResponse().setBody(
                    """{"estimated":false,"panes":[{"id":"p-b","x":0,"y":0,"w":1,"h":1,"focused":true,
                        "surface_ids":["s-here"],"selected_surface_id":"s-here"}]}""",
                )
                "/sessions/ws-b/select", "/sessions/ws-b/panes/s-here" -> recorded(request, """{"ok":true}""")
                else -> MockResponse().setResponseCode(404)
            }
        }
    }

    private fun recorded(request: RecordedRequest, body: String): MockResponse {
        synchronized(seen) { seen.add(request) }
        return MockResponse().setBody(body)
    }

    @After
    fun tearDown() {
        host.clearViewModels()
        server.shutdown()
    }

    private fun viewModel(): TerminalViewModel {
        val bridge = FallbackBridgeClient(
            primary = { BridgeClient(OkHttpClient(), server.url("/").toString()) },
            fallback = { null },
        )
        val vm = host.hold(TerminalViewModel::class.java) {
            TerminalViewModel(FakeGateway(bridge), NoDisplayPrefs, "s-here", "unused", "unused")
        }
        waitUntil { vm.workspaceId.value == "ws-b" }
        return vm
    }

    private fun waitUntil(timeoutMs: Long = 3_000, block: () -> Boolean) {
        val deadline = System.currentTimeMillis() + timeoutMs
        while (System.currentTimeMillis() < deadline) {
            if (block()) return
            Thread.sleep(20)
        }
        assertTrue("condition not met within ${timeoutMs}ms", block())
    }

    @Test
    fun theOwningWorkspaceIsTheOneWhoseTerminalsHoldThisSurface() {
        assertEquals("ws-b", viewModel().workspaceId.value)
    }

    @Test
    fun aHostWithoutTabsHidesTheNewTabActionAndACmuxHostKeepsIt() {
        assertTrue(viewModel().hostHasTabs())
        host.clearViewModels()

        sessionsHost = ""","host":{"name":"home-server","kind":"tmux","capabilities":{"tabs":false,"feed":false}}"""
        assertEquals(false, viewModel().hostHasTabs())
    }

    @Test
    fun newTabIsATabPlacementFromThisSurfaceAndHandsBackTheNewOne() {
        val vm = viewModel()
        val opened = AtomicReference<String>()

        vm.newTab(onCreated = { opened.set(it) })

        waitUntil { opened.get() == "s-new" }
        val request = synchronized(seen) { seen.single() }
        assertEquals("""{"surface_id":"s-here","placement":"tab"}""", request.body.readUtf8())
        assertNull(vm.actionOutcome.value)
    }

    @Test
    fun showOnMacFocusesThisSurface() {
        val vm = viewModel()

        vm.showOnMac()

        waitUntil { vm.actionOutcome.value == ActionOutcome.ShownOnMac }
        assertEquals("""{"surface_id":"s-here"}""", synchronized(seen) { seen.single() }.body.readUtf8())
    }

    @Test
    fun closePaneDeletesThisSurfaceThenLetsTheCallerLeave() {
        val vm = viewModel()
        val left = AtomicBoolean(false)

        vm.closePane(onClosed = { left.set(true) })

        waitUntil { left.get() }
        val request = synchronized(seen) { seen.single() }
        assertEquals("DELETE", request.method)
        assertEquals("/sessions/ws-b/panes/s-here", request.path)
        assertNull(vm.actionOutcome.value)
    }

    @Test
    fun splitOpensThisWorkspacesLayoutWithItsPaneTitlesAndHandsBackTheNewSurface() {
        val vm = viewModel()

        vm.openPlacement()

        waitUntil { vm.placement.state.value is PlacementState.Ready }
        val ready = vm.placement.state.value as PlacementState.Ready
        assertEquals("ws-b", ready.workspaceId)
        assertEquals(mapOf("s-here" to "B shell"), ready.titles)
        assertEquals(listOf("s-here"), ready.layout.panes.single().surfaceIds)

        val opened = AtomicReference<String>()
        vm.placement.createPane("s-here", PanePlacement.UP, onCreated = { opened.set(it) })

        waitUntil { opened.get() == "s-new" }
        assertNull(vm.placement.state.value)
        val request = synchronized(seen) { seen.single() }
        assertEquals("""{"surface_id":"s-here","placement":"up"}""", request.body.readUtf8())
    }
}
