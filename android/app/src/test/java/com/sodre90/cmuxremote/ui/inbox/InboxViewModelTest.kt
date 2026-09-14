package com.sodre90.cmuxremote.ui.inbox

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
import com.sodre90.cmuxremote.model.PendingFeedItem
import com.sodre90.cmuxremote.ui.TestViewModelHost
import com.sodre90.cmuxremote.ui.UiState
import com.sodre90.cmuxremote.ui.sessions.TerminalMatch
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.runBlocking
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import java.util.concurrent.TimeUnit

/** A [BridgeGateway] that always reports no configured slot for [anyBridgeConfigured]
 *  (so InboxViewModel's events-reconnect loop never starts -- these tests are
 *  only about [InboxViewModel.state]/[InboxViewModel.actionError] transitions,
 *  not the reconnect machinery already covered by SocketReconnectorTest) while
 *  still handing out a real [bridge] for reads/replies. */
private class FakeInboxBridgeGateway(private val bridge: FallbackBridgeClient?) : BridgeGateway {
    val monitor = ConnectionMonitor()
    override fun activeBridge(): FallbackBridgeClient? = bridge
    override fun anyBridgeConfigured(): Boolean = false
    override fun eventsSocket(slot: ConnectionSlot): EventsSocket? = null
    override fun terminalSocket(slot: ConnectionSlot, surfaceId: String): TerminalSocket? = null
    override fun relayHealth(): RelayHealth = RelayHealth()
    override fun connectionMonitor(): ConnectionMonitor = monitor
    override fun slotCredentials(): SlotCredentials = SlotCredentials()
    override fun slotCredentialHealth(): SlotCredentialHealth = SlotCredentialHealth()
    override fun appForeground(): StateFlow<Boolean> = MutableStateFlow(true)
}

/**
 * Covers `state: StateFlow<UiState<List<PendingFeedItem>>>` and its interplay
 * with [InboxViewModel.actionError]. The load that matters here is the *first*
 * one: it settles on Error rather than sitting on Loading, so the screen stops
 * asserting an empty inbox over a request that never returned. Once a list is
 * showing, a later failed refresh keeps it and speaks through actionError only.
 */
class InboxViewModelTest {

    private lateinit var server: MockWebServer
    private lateinit var host: TestViewModelHost

    @Before
    fun setUp() {
        host = TestViewModelHost()
        server = MockWebServer().apply { start() }
    }

    // Ordered: the ViewModels' refresh loops stop before the server they talk
    // to goes away.
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

    // Matches the exact strings.xml text each error path falls back to
    // (see CmuxNavHost's INBOX route) so assertions below can keep checking
    // the literal message.
    private fun inboxViewModel(bridge: BridgeGateway) = host.hold(InboxViewModel::class.java) {
        InboxViewModel(
            bridge = bridge,
            bridgeNotConfiguredMessage = "Bridge not configured",
            loadInboxFailedMessage = "Failed to load inbox",
            replyFailedMessage = "Reply failed",
            promptGoneMessage = "That prompt was already answered on home-server",
            terminalNotFoundMessage = "Couldn't find that item's terminal",
        )
    }

    @Test
    fun bridgeNotConfiguredSettlesOnError() {
        val vm = inboxViewModel(FakeInboxBridgeGateway(null))
        assertEquals("Bridge not configured", vm.actionError.value)
        assertEquals("Bridge not configured", (vm.state.value as UiState.Error).message)
    }

    @Test
    fun firstLoadFailureSettlesOnErrorRatherThanLoadingForever() {
        server.enqueue(MockResponse().setResponseCode(500).setBody("""{"error":"boom"}"""))
        val vm = inboxViewModel(FakeInboxBridgeGateway(bridgeFor(server)))

        waitUntil { vm.actionError.value != null }

        assertTrue(vm.state.value is UiState.Error)
    }

    @Test
    fun retryAfterAFailedFirstLoadGoesBackThroughLoadingToReady() {
        server.enqueue(MockResponse().setResponseCode(500).setBody("""{"error":"boom"}"""))
        server.enqueue(MockResponse().setBody("""{"items":[{"id":"i1","kind":"question"}]}"""))
        val vm = inboxViewModel(FakeInboxBridgeGateway(bridgeFor(server)))
        waitUntil { vm.state.value is UiState.Error }

        vm.retry()

        waitUntil { vm.state.value is UiState.Ready }
        assertEquals(listOf("i1"), (vm.state.value as UiState.Ready).data.map { it.id })
        assertEquals(null, vm.actionError.value)
    }

    @Test
    fun userRefreshRaisesIsRefreshingWhileItRunsAndLowersItAfterwards() {
        server.enqueue(MockResponse().setBody("""{"items":[{"id":"i1","kind":"question"}]}"""))
        server.enqueue(
            MockResponse()
                .setBody("""{"items":[{"id":"i2","kind":"question"}]}""")
                .setBodyDelay(200, TimeUnit.MILLISECONDS),
        )
        val vm = inboxViewModel(FakeInboxBridgeGateway(bridgeFor(server)))
        waitUntil { vm.state.value is UiState.Ready }

        vm.userRefresh()

        // The spinner PullToRefreshBox follows has to actually go up, or the
        // pull gesture looks like it did nothing.
        waitUntil { vm.isRefreshing.value }
        waitUntil { !vm.isRefreshing.value }
        assertEquals(listOf("i2"), (vm.state.value as UiState.Ready).data.map { it.id })
    }

    @Test
    fun userRefreshWithNoBridgeReportsItWithoutStrandingTheSpinner() {
        val vm = inboxViewModel(FakeInboxBridgeGateway(null))

        vm.userRefresh()

        assertEquals("Bridge not configured", vm.actionError.value)
        assertEquals(false, vm.isRefreshing.value)
    }

    @Test
    fun successfulLoadPopulatesReadyState() {
        server.enqueue(MockResponse().setBody("""{"items":[{"id":"i1","kind":"question"}]}"""))
        val vm = inboxViewModel(FakeInboxBridgeGateway(bridgeFor(server)))

        waitUntil { vm.state.value is UiState.Ready }

        val ready = vm.state.value as UiState.Ready
        assertEquals(listOf("i1"), ready.data.map { it.id })
    }

    @Test
    fun questionAndPermissionRequestKindsAreKeptOtherKindsAreFilteredOut() {
        server.enqueue(
            MockResponse().setBody(
                """{"items":[
                    {"id":"i1","kind":"question"},
                    {"id":"i2","kind":"permissionRequest"},
                    {"id":"i3","kind":"exitPlan"}
                ]}""",
            ),
        )
        val vm = inboxViewModel(FakeInboxBridgeGateway(bridgeFor(server)))

        waitUntil { vm.state.value is UiState.Ready }

        assertEquals(listOf("i1", "i2"), (vm.state.value as UiState.Ready).data.map { it.id })
    }

    @Test
    fun aRefreshFailureAfterASuccessfulLoadKeepsTheStaleListAndSetsActionError() {
        server.enqueue(MockResponse().setBody("""{"items":[{"id":"i1","kind":"question"}]}"""))
        server.enqueue(MockResponse().setResponseCode(500).setBody("""{"error":"boom"}"""))
        val vm = inboxViewModel(FakeInboxBridgeGateway(bridgeFor(server)))
        waitUntil { vm.state.value is UiState.Ready }

        vm.refresh()
        waitUntil { vm.actionError.value != null }

        val state = vm.state.value
        assertTrue(state is UiState.Ready)
        assertEquals(listOf("i1"), (state as UiState.Ready).data.map { it.id })
    }

    @Test
    fun replySuccessRemovesTheItemFromTheReadyList() {
        server.enqueue(
            MockResponse().setBody(
                """{"items":[
                    {"id":"i1","kind":"question","request_id":"r1"},
                    {"id":"i2","kind":"question","request_id":"r2"}
                ]}""",
            ),
        )
        server.enqueue(MockResponse()) // POST /feed/i1/reply
        val vm = inboxViewModel(FakeInboxBridgeGateway(bridgeFor(server)))
        waitUntil { vm.state.value is UiState.Ready }
        val item = (vm.state.value as UiState.Ready).data.first { it.id == "i1" }

        vm.reply(item, listOf("yes"))

        waitUntil { (vm.state.value as? UiState.Ready)?.data?.map { it.id } == listOf("i2") }
    }

    @Test
    fun replyFailureSetsActionErrorAndLeavesTheListUntouched() {
        server.enqueue(MockResponse().setBody("""{"items":[{"id":"i1","kind":"question","request_id":"r1"}]}"""))
        server.enqueue(MockResponse().setResponseCode(500).setBody("""{"error":"boom"}""")) // POST /feed/i1/reply
        val vm = inboxViewModel(FakeInboxBridgeGateway(bridgeFor(server)))
        waitUntil { vm.state.value is UiState.Ready }
        val item = (vm.state.value as UiState.Ready).data.first()

        vm.reply(item, listOf("yes"))

        waitUntil { vm.actionError.value != null }
        assertEquals(listOf("i1"), (vm.state.value as UiState.Ready).data.map { it.id })
    }

    @Test
    fun replyToAPromptAnsweredAtTheKeyboardDropsTheItemAndSaysSoPlainly() {
        server.enqueue(
            MockResponse().setBody(
                """{"items":[
                    {"id":"i1","kind":"permissionRequest","request_id":"r1"},
                    {"id":"i2","kind":"question","request_id":"r2"}
                ]}""",
            ),
        )
        // POST /feed/i1/reply
        server.enqueue(MockResponse().setResponseCode(409).setBody("""{"error":"prompt_gone"}"""))
        val vm = inboxViewModel(FakeInboxBridgeGateway(bridgeFor(server)))
        waitUntil { vm.state.value is UiState.Ready }
        val item = (vm.state.value as UiState.Ready).data.first { it.id == "i1" }

        vm.replyPermission(item, approve = true)

        waitUntil { (vm.state.value as? UiState.Ready)?.data?.map { it.id } == listOf("i2") }
        assertEquals("That prompt was already answered on home-server", vm.actionError.value)
    }

    @Test
    fun replyPermissionApproveSuccessRemovesTheItemFromTheReadyList() {
        server.enqueue(
            MockResponse().setBody(
                """{"items":[{"id":"i1","kind":"permissionRequest","request_id":"r1"}]}""",
            ),
        )
        server.enqueue(MockResponse()) // POST /feed/i1/reply
        val vm = inboxViewModel(FakeInboxBridgeGateway(bridgeFor(server)))
        waitUntil { vm.state.value is UiState.Ready }
        val item = (vm.state.value as UiState.Ready).data.first()

        vm.replyPermission(item, approve = true)

        waitUntil { (vm.state.value as? UiState.Ready)?.data?.isEmpty() == true }
    }

    @Test
    fun replyPermissionDenyFailureSetsActionErrorAndLeavesTheListUntouched() {
        server.enqueue(
            MockResponse().setBody(
                """{"items":[{"id":"i1","kind":"permissionRequest","request_id":"r1"}]}""",
            ),
        )
        server.enqueue(MockResponse().setResponseCode(500).setBody("""{"error":"boom"}""")) // POST /feed/i1/reply
        val vm = inboxViewModel(FakeInboxBridgeGateway(bridgeFor(server)))
        waitUntil { vm.state.value is UiState.Ready }
        val item = (vm.state.value as UiState.Ready).data.first()

        vm.replyPermission(item, approve = false)

        waitUntil { vm.actionError.value != null }
        assertEquals(listOf("i1"), (vm.state.value as UiState.Ready).data.map { it.id })
    }

    @Test
    fun terminalTargetResolvesTheSurfaceOfTheWorkspaceMatchingTheItemsCwd() {
        server.enqueue(
            MockResponse().setBody(
                """{"items":[{"id":"i1","kind":"question","cwd":"/home/dev/proj"}]}""",
            ),
        )
        val vm = inboxViewModel(FakeInboxBridgeGateway(bridgeFor(server)))
        waitUntil { vm.state.value is UiState.Ready }
        val item = (vm.state.value as UiState.Ready).data.first()

        server.enqueue(
            MockResponse().setBody(
                """{"workspaces":[{"id":"w1","cwd":"/home/dev/proj","terminals":[{"id":"p1"}]}]}""",
            ),
        )
        val target = runBlocking { vm.terminalTarget(item) }

        assertEquals(TerminalMatch.Direct("p1"), target)
    }

    @Test
    fun terminalTargetReturnsAmbiguousWhenSeveralWorkspacesShareTheCwdAndSetsNoActionError() {
        server.enqueue(
            MockResponse().setBody(
                """{"items":[{"id":"i1","kind":"question","cwd":"/home/dev/proj"}]}""",
            ),
        )
        val vm = inboxViewModel(FakeInboxBridgeGateway(bridgeFor(server)))
        waitUntil { vm.state.value is UiState.Ready }
        val item = (vm.state.value as UiState.Ready).data.first()

        server.enqueue(
            MockResponse().setBody(
                """{"workspaces":[
                    {"id":"w1","cwd":"/home/dev/proj","terminals":[{"id":"p1"}]},
                    {"id":"w2","cwd":"/home/dev/proj","terminals":[{"id":"p2"}]}
                ]}""",
            ),
        )
        val target = runBlocking { vm.terminalTarget(item) }

        assertTrue(target is TerminalMatch.Ambiguous)
        assertEquals(listOf("w1", "w2"), (target as TerminalMatch.Ambiguous).workspaces.map { it.id })
        assertEquals(null, vm.actionError.value)
    }

    @Test
    fun terminalTargetSetsActionErrorWhenNoWorkspaceCwdMatches() {
        server.enqueue(
            MockResponse().setBody(
                """{"items":[{"id":"i1","kind":"question","cwd":"/home/dev/proj"}]}""",
            ),
        )
        val vm = inboxViewModel(FakeInboxBridgeGateway(bridgeFor(server)))
        waitUntil { vm.state.value is UiState.Ready }
        val item = (vm.state.value as UiState.Ready).data.first()

        server.enqueue(
            MockResponse().setBody(
                """{"workspaces":[{"id":"w1","cwd":"/home/dev/other","terminals":[{"id":"p1"}]}]}""",
            ),
        )
        val target = runBlocking { vm.terminalTarget(item) }

        assertEquals(null, target)
        assertEquals("Couldn't find that item's terminal", vm.actionError.value)
    }

    @Test
    fun terminalTargetWithNoBridgeConfiguredReturnsNullAndSetsActionError() {
        val vm = inboxViewModel(FakeInboxBridgeGateway(null))

        val target = runBlocking { vm.terminalTarget(PendingFeedItem(id = "i1", cwd = "/home/dev/proj")) }

        assertEquals(null, target)
        assertEquals("Bridge not configured", vm.actionError.value)
    }
}
