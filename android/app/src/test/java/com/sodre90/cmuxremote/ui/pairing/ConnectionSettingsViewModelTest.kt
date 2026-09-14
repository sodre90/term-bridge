package com.sodre90.cmuxremote.ui.pairing

import com.sodre90.cmuxremote.data.BridgeClient
import com.sodre90.cmuxremote.data.BridgeGateway
import com.sodre90.cmuxremote.data.ConnectionMonitor
import com.sodre90.cmuxremote.data.ConnectionSlot
import com.sodre90.cmuxremote.data.DEFAULT_POLL_MS_METERED
import com.sodre90.cmuxremote.data.DEFAULT_POLL_MS_UNMETERED
import com.sodre90.cmuxremote.data.EventsSocket
import com.sodre90.cmuxremote.data.FallbackBridgeClient
import com.sodre90.cmuxremote.data.RelayHealth
import com.sodre90.cmuxremote.data.SlotCredentialHealth
import com.sodre90.cmuxremote.data.SlotCredentials
import com.sodre90.cmuxremote.data.TerminalDisplayGateway
import com.sodre90.cmuxremote.data.TerminalSocket
import com.sodre90.cmuxremote.ui.TestViewModelHost
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import java.util.concurrent.TimeUnit

/** A [BridgeGateway] that hands out [bridge] for [activeBridge] and stubs
 *  everything else -- these tests only exercise [ConnectionSettingsViewModel],
 *  which only ever calls [BridgeGateway.activeBridge]. */
private class FakeTestPushBridgeGateway(private val bridge: FallbackBridgeClient?) : BridgeGateway {
    val monitor = ConnectionMonitor()
    val credentialHealth = SlotCredentialHealth()
    override fun activeBridge(): FallbackBridgeClient? = bridge
    override fun anyBridgeConfigured(): Boolean = bridge != null
    override fun eventsSocket(slot: ConnectionSlot): EventsSocket? = null
    override fun terminalSocket(slot: ConnectionSlot, surfaceId: String): TerminalSocket? = null
    override fun relayHealth(): RelayHealth = RelayHealth()
    override fun connectionMonitor(): ConnectionMonitor = monitor
    override fun slotCredentials(): SlotCredentials = SlotCredentials()
    override fun slotCredentialHealth(): SlotCredentialHealth = credentialHealth
    override fun appForeground(): StateFlow<Boolean> = MutableStateFlow(true)
}

/** These tests only exercise the test-push flow, never font zoom -- a fixed
 *  stub is enough. */
private class FakeTerminalDisplayGateway : TerminalDisplayGateway {
    override fun loadFontZoom(): Float = 1f
    var wheelScrolling = true
    override fun loadWheelScrolling(): Boolean = wheelScrolling
    override fun saveWheelScrolling(enabled: Boolean) { wheelScrolling = enabled }
    val pollMs = mutableMapOf(false to DEFAULT_POLL_MS_UNMETERED, true to DEFAULT_POLL_MS_METERED)
    override fun loadTerminalPollMs(metered: Boolean): Int = pollMs.getValue(metered)
    override fun saveTerminalPollMs(metered: Boolean, ms: Int) { pollMs[metered] = ms }
    override fun saveFontZoom(zoom: Float) = Unit
}

class ConnectionSettingsViewModelTest {

    private lateinit var server: MockWebServer
    private lateinit var host: TestViewModelHost

    @Before
    fun setUp() {
        host = TestViewModelHost()
        server = MockWebServer().apply { start() }
    }

    // Ordered: the ViewModels' in-flight test-push calls stop before the server
    // they talk to goes away.
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

    // Matches the exact strings.xml text each error path falls back to (see
    // CmuxNavHost's SETTINGS route) so assertions below can keep checking the
    // literal message.
    private fun testPushViewModel(bridge: BridgeGateway) =
        host.hold(ConnectionSettingsViewModel::class.java) {
            ConnectionSettingsViewModel(
                bridge = bridge,
                terminalDisplay = FakeTerminalDisplayGateway(),
                bridgeNotConfiguredMessage = "Bridge not configured",
                testPushFailedMessage = "Test push failed",
            )
        }

    @Test
    fun initialStateIsIdle() {
        val vm = testPushViewModel(FakeTestPushBridgeGateway(null))

        assertEquals(TestPushUiState.Idle, vm.testPushState.value)
    }

    @Test
    fun noBridgeConfiguredSetsErrorImmediatelyWithNoNetworkCall() {
        val vm = testPushViewModel(FakeTestPushBridgeGateway(null))

        vm.sendTestPush()

        assertEquals(TestPushUiState.Error("Bridge not configured"), vm.testPushState.value)
        assertEquals(0, server.requestCount)
    }

    @Test
    fun successfulSendTransitionsToSuccess() {
        server.enqueue(MockResponse())
        val vm = testPushViewModel(FakeTestPushBridgeGateway(bridgeFor(server)))

        vm.sendTestPush()

        waitUntil { vm.testPushState.value is TestPushUiState.Success }
        val recorded = server.takeRequest()
        assertEquals("/devices/test-push", recorded.path)
        assertEquals("POST", recorded.method)
    }

    @Test
    fun failureSurfacesTheActualErrorMessageNotAGenericOne() {
        server.enqueue(MockResponse().setResponseCode(503).setBody("""{"error":"agent_offline"}"""))
        val vm = testPushViewModel(FakeTestPushBridgeGateway(bridgeFor(server)))

        vm.sendTestPush()

        waitUntil { vm.testPushState.value is TestPushUiState.Error }
        val error = vm.testPushState.value as TestPushUiState.Error
        assertTrue("expected the real status code in the message, got: ${error.message}", error.message.contains("503"))
        assertTrue(
            "expected the real error body in the message, got: ${error.message}",
            error.message.contains("agent_offline"),
        )
    }

    @Test
    fun stateIsSendingWhileTheCallIsInFlight() {
        server.enqueue(MockResponse().setBodyDelay(200, TimeUnit.MILLISECONDS))
        val vm = testPushViewModel(FakeTestPushBridgeGateway(bridgeFor(server)))

        vm.sendTestPush()

        assertEquals(TestPushUiState.Sending, vm.testPushState.value)
        waitUntil { vm.testPushState.value !is TestPushUiState.Sending }
    }

    @Test
    fun aSecondCallWhileSendingIsIgnored() {
        server.enqueue(MockResponse().setBodyDelay(300, TimeUnit.MILLISECONDS))
        val vm = testPushViewModel(FakeTestPushBridgeGateway(bridgeFor(server)))

        vm.sendTestPush()
        vm.sendTestPush() // ignored -- already sending

        waitUntil { vm.testPushState.value is TestPushUiState.Success }
        assertEquals(1, server.requestCount)
    }
    // -- bridge version

    @Test
    fun bridgeVersionStartsUnknownUntilAsked() {
        val vm = testPushViewModel(FakeTestPushBridgeGateway(bridgeFor(server)))

        assertEquals(BridgeVersionUiState.Loading, vm.bridgeVersion.value)
        assertEquals(0, server.requestCount)
    }

    @Test
    fun bridgeVersionIsReadFromTheAgent() {
        server.enqueue(MockResponse().setBody("""{"bridge":"0.3.0"}"""))
        val vm = testPushViewModel(FakeTestPushBridgeGateway(bridgeFor(server)))

        vm.loadBridgeVersion()

        waitUntil { vm.bridgeVersion.value is BridgeVersionUiState.Known }
        assertEquals(BridgeVersionUiState.Known("0.3.0"), vm.bridgeVersion.value)
        val recorded = server.takeRequest()
        assertEquals("/version", recorded.path)
        assertEquals("GET", recorded.method)
    }

    @Test
    fun noBridgeConfiguredReportsUnavailableWithNoNetworkCall() {
        val vm = testPushViewModel(FakeTestPushBridgeGateway(null))

        vm.loadBridgeVersion()

        assertEquals(BridgeVersionUiState.Unavailable, vm.bridgeVersion.value)
        assertEquals(0, server.requestCount)
    }

    // An agent from before GET /version existed answers 404. The screen must
    // say "unknown", not crash or sit on "checking" forever.
    @Test
    fun anAgentTooOldForTheRouteReportsUnavailable() {
        server.enqueue(MockResponse().setResponseCode(404))
        val vm = testPushViewModel(FakeTestPushBridgeGateway(bridgeFor(server)))

        vm.loadBridgeVersion()

        waitUntil { vm.bridgeVersion.value == BridgeVersionUiState.Unavailable }
    }

    // A 200 with the field missing decodes to "" -- printing an empty version
    // would look like a rendering bug rather than an unknown.
    @Test
    fun anEmptyVersionIsTreatedAsUnknownRatherThanPrinted() {
        server.enqueue(MockResponse().setBody("{}"))
        val vm = testPushViewModel(FakeTestPushBridgeGateway(bridgeFor(server)))

        vm.loadBridgeVersion()

        waitUntil { vm.bridgeVersion.value == BridgeVersionUiState.Unavailable }
    }
}
