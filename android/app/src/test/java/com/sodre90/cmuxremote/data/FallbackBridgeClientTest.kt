package com.sodre90.cmuxremote.data

import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import okhttp3.OkHttpClient
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.SocketPolicy
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Before
import org.junit.Test
import java.io.IOException
import java.util.concurrent.TimeUnit

class FallbackBridgeClientTest {

    private lateinit var primaryServer: MockWebServer
    private lateinit var fallbackServer: MockWebServer

    @Before
    fun setUp() {
        primaryServer = MockWebServer().apply { start() }
        fallbackServer = MockWebServer().apply { start() }
    }

    @After
    fun tearDown() {
        primaryServer.shutdown()
        fallbackServer.shutdown()
    }

    /** connectTimeoutMs is also used as the read timeout, so a NO_RESPONSE
     *  MockResponse (server accepts the connection but never replies) fails
     *  fast and deterministically instead of hanging for OkHttp's real
     *  10s default. */
    private fun clientFor(server: MockWebServer, connectTimeoutMs: Long = 2_000): BridgeClient {
        val http = OkHttpClient.Builder()
            .connectTimeout(connectTimeoutMs, TimeUnit.MILLISECONDS)
            .readTimeout(connectTimeoutMs, TimeUnit.MILLISECONDS)
            .build()
        return BridgeClient(http, server.url("/").toString())
    }

    @Test
    fun sessionsRemembersTheHostBehindThePairing() {
        primaryServer.enqueue(
            MockResponse().setBody(
                """{"workspaces":[],
                "host":{"name":"home-server","kind":"tmux","capabilities":{"tabs":false,"feed":false}}}""",
            ),
        )
        val fb = FallbackBridgeClient(primary = { clientFor(primaryServer) }, fallback = { clientFor(fallbackServer) })
        assertTrue(fb.hostInfo.value.capabilities.tabs)

        runBlocking { fb.sessions() }

        assertEquals("home-server", fb.hostInfo.value.name)
        assertFalse(fb.hostInfo.value.capabilities.tabs)
    }

    @Test
    fun primarySuccessNeverCallsFallback() {
        primaryServer.enqueue(MockResponse().setBody("""{"workspaces":[]}"""))
        val fb = FallbackBridgeClient(primary = { clientFor(primaryServer) }, fallback = { clientFor(fallbackServer) })

        val result = runBlocking { fb.sessions() }

        assertEquals(0, result.size)
        assertEquals(1, primaryServer.requestCount)
        assertEquals(0, fallbackServer.requestCount)
    }

    @Test
    fun primaryFailureFallsBackAndSucceeds() {
        primaryServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        fallbackServer.enqueue(MockResponse().setBody("""{"workspaces":[]}"""))
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer, connectTimeoutMs = 300) },
            fallback = { clientFor(fallbackServer) },
        )

        val result = runBlocking { fb.sessions() }

        assertEquals(0, result.size)
        assertEquals(1, primaryServer.requestCount)
        assertEquals(1, fallbackServer.requestCount)
    }

    @Test
    fun bothFailPropagatesException() {
        primaryServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        fallbackServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer, connectTimeoutMs = 300) },
            fallback = { clientFor(fallbackServer, connectTimeoutMs = 300) },
        )

        try {
            runBlocking { fb.sessions() }
            fail("expected an exception when both primary and fallback fail")
        } catch (e: IOException) {
            // Relay and direct fail for unrelated reasons, so reporting only
            // the last one hides which half is actually broken.
            val both = e as? BothTransportsFailedException
                ?: fail("want BothTransportsFailedException, got $e") as Nothing
            assertNotNull("relay cause must be kept", both.relayError)
            assertTrue("message must name relay: ${both.message}", both.message!!.contains("Relay:"))
            assertTrue("message must name direct: ${both.message}", both.message!!.contains("Direct:"))
        }
    }

    /** The failover has to be visible, not just correct -- the UI strip
     *  (ConnectionStatusStrip) renders straight off these transitions. */
    @Test
    fun failoverIsReportedToTheMonitor() {
        primaryServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        fallbackServer.enqueue(MockResponse().setBody("""{"workspaces":[]}"""))
        val monitor = ConnectionMonitor()
        val seen = mutableListOf<ConnectionStatus>()
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer, connectTimeoutMs = 300) },
            fallback = { clientFor(fallbackServer) },
            monitor = monitor,
        )

        runBlocking {
            val watcher = launch { monitor.status.collect { seen += it } }
            fb.sessions()
            watcher.cancel()
        }

        // Asserted as an ordered subsequence, not by index: status is a
        // conflated StateFlow, and whether the collector starts before or
        // after the initial Unknown is a scheduling detail.
        val relayFirst = seen.indexOf(ConnectionStatus.Connecting(ConnectionSlot.RELAY))
        val fallingBack = seen.indexOfFirst { it is ConnectionStatus.FallingBack }
        assertTrue("relay must be attempted first, saw $seen", relayFirst >= 0)
        assertTrue("fallback must be reported after relay, saw $seen", fallingBack > relayFirst)
        assertNotNull(
            "the relay's own error is why we fell back",
            (seen[fallingBack] as ConnectionStatus.FallingBack).reason,
        )
        // Read off the flow, not [seen]: the final emission lands after the
        // last suspension point, so a conflated collector may never see it.
        assertEquals(ConnectionStatus.Connected(ConnectionSlot.DIRECT), monitor.status.value)
    }

    @Test
    fun bothFailingIsReportedToTheMonitorWithBothCauses() {
        primaryServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        fallbackServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        val monitor = ConnectionMonitor()
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer, connectTimeoutMs = 300) },
            fallback = { clientFor(fallbackServer, connectTimeoutMs = 300) },
            monitor = monitor,
        )

        runCatching { runBlocking { fb.sessions() } }

        val failed = monitor.status.value as ConnectionStatus.Failed
        assertTrue("must name relay: ${failed.detail}", failed.detail.contains("Relay:"))
        assertTrue("must name direct: ${failed.detail}", failed.detail.contains("Direct:"))
    }

    /** Inside the penalty window the relay is never dialled, so a direct-only
     *  failure must still say so rather than implying the relay was fine. */
    @Test
    fun bothFailReportsRelayAsSkippedInsidePenaltyWindow() {
        primaryServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        fallbackServer.enqueue(MockResponse().setBody("""{"workspaces":[]}"""))
        fallbackServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        var clock = 1_000_000L
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer, connectTimeoutMs = 300) },
            fallback = { clientFor(fallbackServer, connectTimeoutMs = 300) },
            now = { clock },
        )

        runBlocking { fb.sessions() } // relay times out, falls back, sets the penalty
        clock += RelayHealth.DEFAULT_PENALTY_MS / 2 // still inside the window

        try {
            runBlocking { fb.sessions() }
            fail("expected an exception when the only attempted transport fails")
        } catch (e: IOException) {
            val both = e as? BothTransportsFailedException
                ?: fail("want BothTransportsFailedException, got $e") as Nothing
            assertNull("relay was never attempted this call", both.relayError)
            assertTrue("message must say relay was skipped: ${both.message}", both.message!!.contains("skipped"))
        }
        assertEquals("relay must not be dialled inside the window", 1, primaryServer.requestCount)
    }

    @Test
    fun onlyPrimaryConfiguredUsesPrimaryDirectly() {
        primaryServer.enqueue(MockResponse().setBody("""{"workspaces":[]}"""))
        val fb = FallbackBridgeClient(primary = { clientFor(primaryServer) }, fallback = { null })

        val result = runBlocking { fb.sessions() }

        assertEquals(0, result.size)
        assertEquals(1, primaryServer.requestCount)
    }

    @Test
    fun onlyPrimaryConfiguredPropagatesFailureWhenNoFallback() {
        primaryServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer, connectTimeoutMs = 300) },
            fallback = { null }
        )

        try {
            runBlocking { fb.sessions() }
            fail("expected the primary's failure to propagate when there's no fallback")
        } catch (e: IOException) {
            // expected
        }
    }

    @Test
    fun onlyFallbackConfiguredUsesFallbackDirectly() {
        fallbackServer.enqueue(MockResponse().setBody("""{"workspaces":[]}"""))
        val fb = FallbackBridgeClient(primary = { null }, fallback = { clientFor(fallbackServer) })

        val result = runBlocking { fb.sessions() }

        assertEquals(0, result.size)
        assertEquals(1, fallbackServer.requestCount)
    }

    @Test
    fun penaltyWindowSkipsPrimaryUntilItExpires() {
        primaryServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        primaryServer.enqueue(MockResponse().setBody("""{"workspaces":[]}""")) // must NOT be consumed by the 2nd call
        fallbackServer.enqueue(MockResponse().setBody("""{"workspaces":[]}"""))
        fallbackServer.enqueue(MockResponse().setBody("""{"workspaces":[]}"""))
        var clock = 1_000_000L
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer, connectTimeoutMs = 300) },
            fallback = { clientFor(fallbackServer) },
            now = { clock },
        )

        runBlocking { fb.sessions() } // primary times out, falls back, sets the penalty
        clock += RelayHealth.DEFAULT_PENALTY_MS / 2 // still inside the window
        runBlocking { fb.sessions() } // must skip primary entirely

        assertEquals(1, primaryServer.requestCount)
        assertEquals(2, fallbackServer.requestCount)
    }

    @Test
    fun primary4xxPropagatesWithoutFailoverOrPenalty() {
        // renameWorkspace is a write, so it's never subject to sessions()/
        // pendingFeed()'s not_paired retry (see the sessions*PairingRetry
        // tests below) -- that keeps this test isolated to the "4xx never
        // fails over" behavior it's actually named for. Deliberately NOT
        // registerDevice, which no longer routes through call() at all.
        primaryServer.enqueue(MockResponse().setResponseCode(409).setBody("""{"error":"stale_pairing"}"""))
        primaryServer.enqueue(MockResponse()) // must be consumed by the 2nd call
        val fb = FallbackBridgeClient(primary = { clientFor(primaryServer) }, fallback = { clientFor(fallbackServer) })

        try {
            runBlocking { fb.renameWorkspace("ws-1", "new title") }
            fail("expected the primary's 409 to propagate instead of failing over")
        } catch (e: BridgeException) {
            assertEquals(409, e.code)
        }
        assertEquals(0, fallbackServer.requestCount)

        // No penalty was set, so the second call must still try primary first.
        runBlocking { fb.renameWorkspace("ws-1", "new title") }

        assertEquals(2, primaryServer.requestCount)
        assertEquals(0, fallbackServer.requestCount)
    }

    @Test
    fun sessionsRetriesA409NotPairedThenSucceeds() {
        // The post-pairing race: a read right after pairing can hit the Mac
        // agent before it's derived the e2e session yet.
        primaryServer.enqueue(MockResponse().setResponseCode(409).setBody("""{"error":"not_paired"}"""))
        primaryServer.enqueue(MockResponse().setBody("""{"workspaces":[]}"""))
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer) },
            fallback = { null },
            pairingRetryDelayMs = 0L,
        )

        val result = runBlocking { fb.sessions() }

        assertEquals(0, result.size)
        assertEquals(2, primaryServer.requestCount)
    }

    @Test
    fun pendingFeedAlsoRetriesA409NotPaired() {
        primaryServer.enqueue(MockResponse().setResponseCode(409).setBody("""{"error":"not_paired"}"""))
        primaryServer.enqueue(MockResponse().setBody("""{"items":[]}"""))
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer) },
            fallback = { null },
            pairingRetryDelayMs = 0L,
        )

        val result = runBlocking { fb.pendingFeed() }

        assertEquals(0, result.size)
        assertEquals(2, primaryServer.requestCount)
    }

    @Test
    fun sessionsGivesUpAfterExhaustingPairingRetriesAndPropagates409() {
        repeat(3) { primaryServer.enqueue(MockResponse().setResponseCode(409).setBody("""{"error":"not_paired"}""")) }
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer) },
            fallback = { null },
            pairingRetryDelayMs = 0L,
        )

        try {
            runBlocking { fb.sessions() }
            fail("expected the 409 to propagate once pairing retries are exhausted")
        } catch (e: BridgeException) {
            assertEquals(409, e.code)
        }
        assertEquals(3, primaryServer.requestCount)
    }

    @Test
    fun sessionsDoesNotRetryANon409FourXx() {
        primaryServer.enqueue(MockResponse().setResponseCode(400).setBody("""{"error":"bad_request"}"""))
        primaryServer.enqueue(MockResponse().setBody("""{"workspaces":[]}""")) // must NOT be consumed
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer) },
            fallback = { null },
            pairingRetryDelayMs = 0L,
        )

        try {
            runBlocking { fb.sessions() }
            fail("expected a non-409 4xx to propagate without retry")
        } catch (e: BridgeException) {
            assertEquals(400, e.code)
        }
        assertEquals(1, primaryServer.requestCount)
    }

    @Test
    fun primary5xxFailsOverAndSetsPenalty() {
        primaryServer.enqueue(MockResponse().setResponseCode(503).setBody("""{"error":"tunnel_down"}"""))
        primaryServer.enqueue(MockResponse().setBody("""{"workspaces":[]}""")) // must NOT be consumed by the 2nd call
        fallbackServer.enqueue(MockResponse().setBody("""{"workspaces":[]}"""))
        fallbackServer.enqueue(MockResponse().setBody("""{"workspaces":[]}"""))
        var clock = 1_000_000L
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer) },
            fallback = { clientFor(fallbackServer) },
            now = { clock },
        )

        runBlocking { fb.sessions() } // primary 503s, falls back, sets the penalty
        clock += RelayHealth.DEFAULT_PENALTY_MS / 2 // still inside the window
        runBlocking { fb.sessions() } // must skip primary entirely

        assertEquals(1, primaryServer.requestCount)
        assertEquals(2, fallbackServer.requestCount)
    }

    @Test
    fun penaltyWindowExpiresAndRetriesPrimary() {
        primaryServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        primaryServer.enqueue(MockResponse().setBody("""{"workspaces":[]}"""))
        fallbackServer.enqueue(MockResponse().setBody("""{"workspaces":[]}"""))
        var clock = 1_000_000L
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer, connectTimeoutMs = 300) },
            fallback = { clientFor(fallbackServer) },
            now = { clock },
        )

        runBlocking { fb.sessions() } // primary times out, falls back, sets the penalty
        clock += RelayHealth.DEFAULT_PENALTY_MS + 1_000L // past the window

        runBlocking { fb.sessions() } // must retry primary, which now succeeds

        assertEquals(2, primaryServer.requestCount)
        assertEquals(1, fallbackServer.requestCount)
    }

    @Test
    fun registerDeviceRegistersOnEverySlotEvenWhenTheFirstOneSucceeds() {
        // cmux-app-vex. Relay and agent keep separate device tables, and the
        // relay answers /devices/register itself, so a primary success is no
        // evidence the direct slot has the token -- stopping there left
        // direct-mode push with nothing to send to, and the gap only showed
        // up during a relay outage, when direct was the only path left.
        primaryServer.enqueue(MockResponse())
        fallbackServer.enqueue(MockResponse())
        val fb = FallbackBridgeClient(primary = { clientFor(primaryServer) }, fallback = { clientFor(fallbackServer) })

        runBlocking { fb.registerDevice("fcm-token-abc") }

        assertEquals(1, primaryServer.requestCount)
        assertEquals("both slots must receive the token", 1, fallbackServer.requestCount)
    }

    @Test
    fun registerDeviceStillReachesTheOtherSlotWhenOneIsUnreachable() {
        primaryServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        fallbackServer.enqueue(MockResponse())
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer, connectTimeoutMs = 300) },
            fallback = { clientFor(fallbackServer) },
        )

        runBlocking { fb.registerDevice("fcm-token-abc") }

        assertEquals(1, primaryServer.requestCount)
        assertEquals(1, fallbackServer.requestCount)
    }

    @Test
    fun registerDeviceIsNotStoppedByARejectionFromOneSlot() {
        // A 4xx is fatal for the ordinary writes (see
        // primary4xxPropagatesWithoutFailoverOrPenalty) because re-running
        // them against another backend could double-apply. Registering a
        // token is idempotent per slot, and one slot rejecting it says
        // nothing about the other, so it must not deny the other its token.
        primaryServer.enqueue(MockResponse().setResponseCode(409).setBody("""{"error":"stale_pairing"}"""))
        fallbackServer.enqueue(MockResponse())
        val fb = FallbackBridgeClient(primary = { clientFor(primaryServer) }, fallback = { clientFor(fallbackServer) })

        runBlocking { fb.registerDevice("fcm-token-abc") }

        assertEquals(1, primaryServer.requestCount)
        assertEquals(1, fallbackServer.requestCount)
    }

    @Test
    fun registerDeviceThrowsOnlyWhenNoSlotAcceptedTheToken() {
        primaryServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        fallbackServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer, connectTimeoutMs = 300) },
            fallback = { clientFor(fallbackServer, connectTimeoutMs = 300) },
        )

        try {
            runBlocking { fb.registerDevice("fcm-token-abc") }
            fail("expected registerDevice to report that no slot took the token")
        } catch (e: IOException) {
            // expected
        }
        assertEquals(1, primaryServer.requestCount)
        assertEquals(1, fallbackServer.requestCount)
    }

    @Test
    fun registerDeviceUsesTheOnlyConfiguredSlot() {
        fallbackServer.enqueue(MockResponse())
        val fb = FallbackBridgeClient(primary = { null }, fallback = { clientFor(fallbackServer) })

        runBlocking { fb.registerDevice("fcm-token-abc") }

        assertEquals(1, fallbackServer.requestCount)
    }

    @Test
    fun registerDeviceThrowsWhenNoSlotIsConfigured() {
        val fb = FallbackBridgeClient(primary = { null }, fallback = { null })

        try {
            runBlocking { fb.registerDevice("fcm-token-abc") }
            fail("expected registerDevice to reject an unconfigured client")
        } catch (e: BridgeException) {
            assertEquals(0, e.code)
        }
    }

    private fun registerRecordingOutcomes(
        primary: () -> BridgeClient?,
        fallback: () -> BridgeClient?,
    ): Map<ConnectionSlot, RegistrationOutcome> {
        val outcomes = mutableMapOf<ConnectionSlot, RegistrationOutcome>()
        val fb = FallbackBridgeClient(
            primary = primary,
            fallback = fallback,
            onRegistrationOutcome = { slot, outcome -> outcomes[slot] = outcome },
        )
        runCatching { runBlocking { fb.registerDevice("fcm-token-abc") } }
        return outcomes
    }

    /** The exact signal that was collected on every launch for eight days and
     *  discarded (cmux-app-hr1): a 401 on one slot while the other is happily
     *  accepting the token. Asserts the reported outcomes, not just that the
     *  call returned -- returning is what it did before, too. */
    @Test
    fun registerDeviceReportsA401AsRejectedWhileTheOtherSlotIsAccepted() {
        primaryServer.enqueue(MockResponse())
        fallbackServer.enqueue(MockResponse().setResponseCode(401).setBody("""{"error":"unauthorized"}"""))

        val outcomes = registerRecordingOutcomes(
            primary = { clientFor(primaryServer) },
            fallback = { clientFor(fallbackServer) },
        )

        assertEquals(RegistrationOutcome.ACCEPTED, outcomes[ConnectionSlot.RELAY])
        assertEquals(RegistrationOutcome.REJECTED, outcomes[ConnectionSlot.DIRECT])
    }

    /** The false-alarm case, and the one that matters most: a server that
     *  cannot be reached says nothing whatsoever about the credential. */
    @Test
    fun registerDeviceReportsAnUnreachableSlotAsUnreachableNotRejected() {
        primaryServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        fallbackServer.enqueue(MockResponse())

        val outcomes = registerRecordingOutcomes(
            primary = { clientFor(primaryServer, connectTimeoutMs = 300) },
            fallback = { clientFor(fallbackServer) },
        )

        assertEquals(RegistrationOutcome.UNREACHABLE, outcomes[ConnectionSlot.RELAY])
        assertEquals(RegistrationOutcome.ACCEPTED, outcomes[ConnectionSlot.DIRECT])
    }

    @Test
    fun registerDeviceReportsA5xxAsUnreachableNotRejected() {
        primaryServer.enqueue(MockResponse().setResponseCode(503).setBody("""{"error":"tunnel_down"}"""))
        fallbackServer.enqueue(MockResponse())

        val outcomes = registerRecordingOutcomes(
            primary = { clientFor(primaryServer) },
            fallback = { clientFor(fallbackServer) },
        )

        assertEquals(RegistrationOutcome.UNREACHABLE, outcomes[ConnectionSlot.RELAY])
        assertEquals(RegistrationOutcome.ACCEPTED, outcomes[ConnectionSlot.DIRECT])
    }

    /** Reported through a callback rather than a return value precisely so
     *  this case still reports: no slot accepted, so the call throws, and this
     *  is the total lockout the rejections are the explanation for. */
    @Test
    fun registerDeviceStillReportsBothRejectionsWhenItThrows() {
        primaryServer.enqueue(MockResponse().setResponseCode(401).setBody("""{"error":"unauthorized"}"""))
        fallbackServer.enqueue(MockResponse().setResponseCode(401).setBody("""{"error":"unauthorized"}"""))
        val outcomes = mutableMapOf<ConnectionSlot, RegistrationOutcome>()
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer) },
            fallback = { clientFor(fallbackServer) },
            onRegistrationOutcome = { slot, outcome -> outcomes[slot] = outcome },
        )

        try {
            runBlocking { fb.registerDevice("fcm-token-abc") }
            fail("expected registerDevice to report that no slot took the token")
        } catch (e: IOException) {
            // expected
        }

        assertEquals(RegistrationOutcome.REJECTED, outcomes[ConnectionSlot.RELAY])
        assertEquals(RegistrationOutcome.REJECTED, outcomes[ConnectionSlot.DIRECT])
    }

    /** Marks the slot and nothing else: a 401 must leave [call]'s "4xx never
     *  fails over" rule exactly as it was, or a non-idempotent write could
     *  run twice on the way to being reported. */
    @Test
    fun a401FromPrimaryMarksRelayWithoutFailingOverOrSettingAPenalty() {
        primaryServer.enqueue(MockResponse().setResponseCode(401).setBody("""{"error":"unauthorized"}"""))
        primaryServer.enqueue(MockResponse()) // must be consumed by the 2nd call, i.e. no penalty was set
        val rejected = mutableListOf<ConnectionSlot>()
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer) },
            fallback = { clientFor(fallbackServer) },
            onCredentialRejected = { rejected += it },
        )

        try {
            runBlocking { fb.renameWorkspace("ws-1", "new title") }
            fail("expected the primary's 401 to propagate instead of failing over")
        } catch (e: BridgeException) {
            assertEquals(401, e.code)
        }
        runBlocking { fb.renameWorkspace("ws-1", "new title") }

        assertEquals(listOf(ConnectionSlot.RELAY), rejected)
        assertEquals("the write must never be re-run on the other slot", 0, fallbackServer.requestCount)
        assertEquals("a 401 is not a reachability failure, so no penalty", 2, primaryServer.requestCount)
    }

    /** The 2026-08-21 shape exactly: relay unreachable, direct answers 401.
     *  Both are reported, and the slot named is the one that answered. */
    @Test
    fun a401FromTheFallbackMarksDirect() {
        primaryServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        fallbackServer.enqueue(MockResponse().setResponseCode(401).setBody("""{"error":"unauthorized"}"""))
        val rejected = mutableListOf<ConnectionSlot>()
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer, connectTimeoutMs = 300) },
            fallback = { clientFor(fallbackServer) },
            onCredentialRejected = { rejected += it },
        )

        runCatching { runBlocking { fb.sessions() } }

        assertEquals(listOf(ConnectionSlot.DIRECT), rejected)
    }

    /** The same 401 arriving through the skip-primary branch, which picks its
     *  slot separately and so could get the attribution wrong on its own. */
    @Test
    fun a401OnTheOnlyAttemptedSlotMarksThatSlot() {
        fallbackServer.enqueue(MockResponse().setResponseCode(401).setBody("""{"error":"unauthorized"}"""))
        val rejected = mutableListOf<ConnectionSlot>()
        val fb = FallbackBridgeClient(
            primary = { null },
            fallback = { clientFor(fallbackServer) },
            onCredentialRejected = { rejected += it },
        )

        runCatching { runBlocking { fb.sessions() } }

        assertEquals(listOf(ConnectionSlot.DIRECT), rejected)
    }

    @Test
    fun aNon401FourXxMarksNothing() {
        primaryServer.enqueue(MockResponse().setResponseCode(409).setBody("""{"error":"stale_pairing"}"""))
        val rejected = mutableListOf<ConnectionSlot>()
        val fb = FallbackBridgeClient(
            primary = { clientFor(primaryServer) },
            fallback = { clientFor(fallbackServer) },
            onCredentialRejected = { rejected += it },
        )

        runCatching { runBlocking { fb.renameWorkspace("ws-1", "new title") } }

        assertEquals(emptyList<ConnectionSlot>(), rejected)
    }

    /** A 409 stale_pairing is not a destroyed credential -- only a 401 is. */
    @Test
    fun registerDeviceReportsANon401FourXxAsUnreachable() {
        primaryServer.enqueue(MockResponse().setResponseCode(409).setBody("""{"error":"stale_pairing"}"""))
        fallbackServer.enqueue(MockResponse())

        val outcomes = registerRecordingOutcomes(
            primary = { clientFor(primaryServer) },
            fallback = { clientFor(fallbackServer) },
        )

        assertEquals(RegistrationOutcome.UNREACHABLE, outcomes[ConnectionSlot.RELAY])
        assertEquals(RegistrationOutcome.ACCEPTED, outcomes[ConnectionSlot.DIRECT])
    }
}
