package com.sodre90.cmuxremote.data

import com.goterl.lazysodium.LazySodiumJava
import com.goterl.lazysodium.SodiumJava
import com.sodre90.cmuxremote.data.e2e.Cipher
import com.sodre90.cmuxremote.data.e2e.DIR_AGENT_TO_DEVICE
import com.sodre90.cmuxremote.data.e2e.DIR_DEVICE_TO_AGENT
import com.sodre90.cmuxremote.data.e2e.PAYLOAD_DEFLATE
import com.sodre90.cmuxremote.data.e2e.PAYLOAD_DEFLATE_STREAM
import com.sodre90.cmuxremote.data.e2e.PAYLOAD_IDENTITY
import com.sodre90.cmuxremote.data.e2e.PairedSession
import com.sodre90.cmuxremote.data.e2e.ReplayRejectedException
import com.sodre90.cmuxremote.data.e2e.ReplayWindow
import com.sodre90.cmuxremote.data.e2e.nonce
import com.sodre90.cmuxremote.model.RenderGridDecoder
import com.sodre90.cmuxremote.model.TerminalDown
import com.sodre90.cmuxremote.model.TerminalUp
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeout
import kotlinx.coroutines.withTimeoutOrNull
import okhttp3.OkHttpClient
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okio.ByteString
import okio.ByteString.Companion.toByteString
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import java.io.ByteArrayOutputStream
import java.nio.ByteBuffer
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit
import java.util.zip.Deflater

/** Simple PairedSession double sharing one secret with independent counters,
 *  used to simulate "the other side" (a mock agent) in these WS tests. */
private class SharedSecretSession(private val secret: ByteArray) : PairedSession {
    private var sendCounter = 0L
    private var window = ReplayWindow()
    override fun sharedSecret(): ByteArray = secret
    override fun nextSendCounter(): Long = sendCounter++
    override fun <T> validateAndCommitRecvCounter(n: Long, decrypt: (ByteArray) -> T): T {
        if (!window.canAccept(n)) throw ReplayRejectedException(n, window.highestSeen)
        return decrypt(secret).also { window = window.commit(n) }
    }
}

class TerminalSocketTest {

    private lateinit var server: MockWebServer
    private val received = LinkedBlockingQueue<ByteString>()
    private val secret = ByteArray(32) { it.toByte() }
    private val cipher = Cipher(LazySodiumJava(SodiumJava()))

    @Before
    fun setUp() {
        server = MockWebServer().apply { start() }
    }

    @After
    fun tearDown() {
        server.shutdown()
    }

    @Test
    fun receivesReplayFrameAndSendsEncryptedInput() = runBlocking {
        val serverSession = SharedSecretSession(secret)
        server.enqueue(
            MockResponse().withWebSocketUpgrade(object : WebSocketListener() {
                override fun onOpen(webSocket: WebSocket, response: Response) {
                    val plaintext = """{"type":"replay","columns":3,"rows":1,"seq":1,
                        "grid":{"columns":3,"rows":1,"row_spans":[{"row":0,"column":0,"text":"hi"}]}}"""
                    val n = serverSession.nextSendCounter()
                    val ct = cipher.seal(secret, nonce(DIR_AGENT_TO_DEVICE, n), plaintext.toByteArray(Charsets.UTF_8))
                    val frame = ByteArray(8 + ct.size)
                    ByteBuffer.wrap(frame, 0, 8).putLong(n)
                    ct.copyInto(frame, 8)
                    webSocket.send(frame.toByteString())
                }

                override fun onMessage(webSocket: WebSocket, bytes: ByteString) {
                    received.add(bytes)
                }
            }),
        )

        val clientSession = SharedSecretSession(secret)
        val ts = TerminalSocket(OkHttpClient(), server.url("/").toString(), "surface-1", clientSession, cipher)

        withTimeout(5_000) {
            val first = CompletableDeferred<TerminalDown>()
            val job = launch(Dispatchers.IO) {
                ts.connect().collect { frame ->
                    if (!first.isCompleted) {
                        ts.send(TerminalUp(type = "input", text = "ls\n"))
                        first.complete(frame)
                    }
                }
            }

            val frame = first.await()
            assertEquals("replay", frame.type)
            // columns=3 so "hi" is padded with one trailing blank to full width.
            assertEquals("hi ", RenderGridDecoder.decode(frame.grid!!).lines[0].text)

            val gotBytes = withContext(Dispatchers.IO) { received.poll(5, TimeUnit.SECONDS) }
            assertNotNull(gotBytes)
            // Decode as the agent would: an 8-byte counter prefix, then open
            // with DIR_DEVICE_TO_AGENT (the phone's outgoing direction).
            val raw = gotBytes!!.toByteArray()
            val n = ByteBuffer.wrap(raw, 0, 8).long
            val opened = cipher.open(secret, nonce(DIR_DEVICE_TO_AGENT, n), raw.copyOfRange(8, raw.size))
            assertTrue(String(opened, Charsets.UTF_8).contains("\"type\":\"input\""))

            job.cancelAndJoin()
        }
    }

    /** Connects and returns whatever ended the flow, or null for a clean end. */
    private fun collectUntilClosedBy(closeCode: Int): Throwable? = runBlocking {
        server.enqueue(
            MockResponse().withWebSocketUpgrade(object : WebSocketListener() {
                override fun onOpen(webSocket: WebSocket, response: Response) {
                    webSocket.close(closeCode, "")
                }
            }),
        )
        val ts = TerminalSocket(
            OkHttpClient(),
            server.url("/").toString(),
            "surface-1",
            SharedSecretSession(secret),
            cipher,
        )
        withTimeout(5_000) {
            withContext(Dispatchers.IO) {
                runCatching { ts.connect().collect { } }.exceptionOrNull()
            }
        }
    }

    // cmux-app-34c: onClosing threw the close code away, so a surface the
    // bridge had just declared gone ended the flow exactly like a dropped
    // connection -- and the reconnect loop dutifully dialled it again.
    @Test
    fun surfaceGoneCloseCodeEndsTheFlowWithADistinguishableFailure() {
        assertTrue(
            "want SubscriptionGoneException",
            collectUntilClosedBy(CLOSE_SURFACE_GONE) is SubscriptionGoneException,
        )
    }

    // Any other close is still a plain end-of-stream, which the reconnect loop
    // treats as a disconnect and retries. That is the behaviour to preserve.
    @Test
    fun anOrdinaryCloseStillEndsTheFlowCleanly() {
        for (code in listOf(1000, 1001, 1011)) {
            assertNull("close $code must not look like a gone surface", collectUntilClosedBy(code))
        }
    }

    /**
     * Serves one grid frame, sealed, optionally deflated behind a codec tag,
     * and answers the upgrade with [confirmHeader] -- standing in for a bridge
     * that does or does not support compression. Returns the frame the app
     * decoded plus the request line the server saw.
     */
    private fun frameThroughBridge(
        deflate: Boolean,
        confirmHeader: Boolean,
        pollMs: Int = DEFAULT_TERMINAL_POLL_MS,
    ): Pair<TerminalDown, String> =
        runBlocking {
            val serverSession = SharedSecretSession(secret)
            val json = """{"type":"replay","columns":3,"rows":1,"seq":1,""" +
                """"grid":{"columns":3,"rows":1,"row_spans":[{"row":0,"column":0,"text":"hi"}]}}"""
            val upgrade = MockResponse().withWebSocketUpgrade(object : WebSocketListener() {
                override fun onOpen(webSocket: WebSocket, response: Response) {
                    val body = json.toByteArray(Charsets.UTF_8)
                    val payload = when {
                        deflate -> byteArrayOf(PAYLOAD_DEFLATE) + rawDeflate(body)
                        confirmHeader -> byteArrayOf(PAYLOAD_IDENTITY) + body
                        else -> body
                    }
                    val n = serverSession.nextSendCounter()
                    val ct = cipher.seal(secret, nonce(DIR_AGENT_TO_DEVICE, n), payload)
                    val frame = ByteArray(8 + ct.size)
                    ByteBuffer.wrap(frame, 0, 8).putLong(n)
                    ct.copyInto(frame, 8)
                    webSocket.send(frame.toByteString())
                }
            })
            if (confirmHeader) upgrade.setHeader(DEFLATE_HEADER, "1")
            server.enqueue(upgrade)

            val ts = TerminalSocket(
                OkHttpClient(),
                server.url("/").toString(),
                "surface-1",
                SharedSecretSession(secret),
                cipher,
                pollMs,
            )
            withTimeout(5_000) {
                val first = CompletableDeferred<TerminalDown>()
                val job = launch(Dispatchers.IO) {
                    ts.connect().collect { if (!first.isCompleted) first.complete(it) }
                }
                val frame = first.await()
                val request = withContext(Dispatchers.IO) { server.takeRequest().path!! }
                job.cancelAndJoin()
                frame to request
            }
        }

    private fun rawDeflate(body: ByteArray): ByteArray =
        Deflater(Deflater.DEFAULT_COMPRESSION, true).run {
            setInput(body)
            finish()
            val out = ByteArray(body.size + 64)
            val n = deflate(out)
            end()
            out.copyOfRange(0, n)
        }

    /** Per-row screens are asked for on every socket; a bridge that does not
     *  know the parameter sends whole screens, which decode as before. */
    @Test
    fun asksForTheScreenRowByRow() {
        val (_, path) = frameThroughBridge(deflate = false, confirmHeader = false)
        assertTrue("want ?rows=1 on the terminal URL, got $path", path.contains("rows=1"))
    }

    /** The bridge clamps the value it is given; this side's job is only to send
     *  the one the user chose, on every socket it opens. */
    @Test
    fun tellsTheBridgeHowOftenToCheckForOutput() {
        val (_, path) = frameThroughBridge(deflate = false, confirmHeader = false, pollMs = 2000)
        assertTrue("want ?poll_ms=2000 on the terminal URL, got $path", path.contains("poll_ms=2000"))
    }

    /**
     * An unconfigured app must still name an interval rather than leaving the
     * bridge to guess, so that the default is one number and not two. Asserted
     * against the constant rather than a literal: the default is deliberately
     * the metered one, and pinning a number here would just have to be edited
     * whenever that judgement changes.
     */
    @Test
    fun sendsTheDefaultIntervalWhenNothingWasChosen() {
        val (_, path) = frameThroughBridge(deflate = false, confirmHeader = false)
        assertTrue(
            "want poll_ms=$DEFAULT_TERMINAL_POLL_MS on the terminal URL, got $path",
            path.contains("poll_ms=$DEFAULT_TERMINAL_POLL_MS"),
        )
    }

    @Test
    fun asksTheBridgeForCompression() {
        val (_, path) = frameThroughBridge(deflate = false, confirmHeader = false)
        assertTrue("want ?deflate=1 on the terminal URL, got $path", path.contains("deflate=1"))
    }

    @Test
    fun asksTheBridgeForDeltaFrames() {
        val (_, path) = frameThroughBridge(deflate = false, confirmHeader = false)
        assertTrue("want delta=1 on the terminal URL, got $path", path.contains("delta=1"))
    }

    /**
     * Serves an unreadable frame followed by a good one. Returns what ended the
     * flow (null if it was still running) and the frame that got through, if any.
     */
    private fun badThenGoodFrame(
        deltaConfirmed: Boolean,
        streamConfirmed: Boolean = false,
    ): Pair<Throwable?, TerminalDown?> = runBlocking {
        val serverSession = SharedSecretSession(secret)
        val good = """{"type":"output","columns":3,"rows":1,""" +
            """"grid":{"columns":3,"rows":1,"row_spans":[{"row":0,"column":0,"text":"ok"}]}}"""
        val upgrade = MockResponse().withWebSocketUpgrade(object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) {
                // Sealed correctly, so it decrypts -- but it is not JSON, so it
                // fails where a truncated or corrupted frame would.
                for (plain in listOf("not json", good)) {
                    val n = serverSession.nextSendCounter()
                    val ct = cipher.seal(secret, nonce(DIR_AGENT_TO_DEVICE, n), plain.toByteArray(Charsets.UTF_8))
                    val frame = ByteArray(8 + ct.size)
                    ByteBuffer.wrap(frame, 0, 8).putLong(n)
                    ct.copyInto(frame, 8)
                    webSocket.send(frame.toByteString())
                }
            }
        })
        if (deltaConfirmed) upgrade.setHeader(DELTA_HEADER, "1")
        if (streamConfirmed) upgrade.setHeader(STREAM_HEADER, "1")
        server.enqueue(upgrade)

        val ts = TerminalSocket(
            OkHttpClient(),
            server.url("/").toString(),
            "surface-1",
            SharedSecretSession(secret),
            cipher,
        )
        val ended = CompletableDeferred<Throwable?>()
        val got = CompletableDeferred<TerminalDown>()
        val job = launch(Dispatchers.IO) {
            ended.complete(runCatching { ts.connect().collect { got.complete(it) } }.exceptionOrNull())
        }
        val endedWith = withTimeoutOrNull(3_000) { ended.await() }
        val frame = withTimeoutOrNull(500) { got.await() }
        job.cancelAndJoin()
        endedWith to frame
    }

    /**
     * The test the delta design rests on. A frame the app cannot read is no
     * longer survivable by skipping it: later frames complete themselves from
     * blocks this socket never saw, so the pane would carry a wrong scrollback
     * silently for as long as the socket stayed open. Ending the flow hands it
     * to the reconnect loop, which resyncs from a fresh full replay.
     */
    @Test
    fun anUnreadableFrameEndsADeltaStreamInsteadOfBeingSkipped() {
        val (endedWith, frame) = badThenGoodFrame(deltaConfirmed = true)
        assertTrue("want DesyncException so the reconnector resyncs", endedWith is DesyncException)
        assertNull("no frame may be delivered after the desync", frame)
    }

    /** Off a delta stream each frame still stands alone, so the old
     *  skip-and-carry-on behaviour is the right one and must be preserved. */
    @Test
    fun anUnreadableFrameIsStillSkippedWhenDeltasWereNotNegotiated() {
        val (endedWith, frame) = badThenGoodFrame(deltaConfirmed = false)
        assertNull("a non-delta stream must stay open through one bad frame", endedWith)
        assertEquals("output", frame?.type)
    }

    @Test
    fun inflatesFramesOnceTheBridgeConfirms() {
        val (frame, _) = frameThroughBridge(deflate = true, confirmHeader = true)
        assertEquals("replay", frame.type)
        assertEquals("hi ", RenderGridDecoder.decode(frame.grid!!).lines[0].text)
    }

    /**
     * An older bridge ignores ?deflate=1 and sends untagged JSON. Without the
     * confirming header the app must not strip a tag byte -- doing so would
     * eat the leading `{` and drop every frame, leaving a blank terminal.
     */
    @Test
    fun readsUntaggedFramesFromABridgeThatNeverConfirmed() {
        val (frame, _) = frameThroughBridge(deflate = false, confirmHeader = false)
        assertEquals("replay", frame.type)
        assertEquals("hi ", RenderGridDecoder.decode(frame.grid!!).lines[0].text)
    }

    /** A confirming bridge may still send a frame uncompressed when deflate
     *  would not shrink it, so the identity tag has to work on this path too. */
    @Test
    fun readsAnIdentityTaggedFrameOnANegotiatedSocket() {
        val (frame, _) = frameThroughBridge(deflate = false, confirmHeader = true)
        assertEquals("replay", frame.type)
        assertEquals("hi ", RenderGridDecoder.decode(frame.grid!!).lines[0].text)
    }

    @Test
    fun asksTheBridgeForASharedCompressionWindow() {
        val (_, path) = frameThroughBridge(deflate = false, confirmHeader = false)
        assertTrue("want stream=1 on the terminal URL, got $path", path.contains("stream=1"))
    }

    /**
     * Serves two frames down one shared window, the second compressed against
     * the first. Returns both as the app decoded them.
     *
     * The second frame is what this is for: it is unreadable on its own, so it
     * only arrives if the socket kept one decoder alive across frames instead of
     * building a fresh one per message.
     */
    private fun twoFramesThroughOneWindow(confirmStream: Boolean): List<TerminalDown> = runBlocking {
        val serverSession = SharedSecretSession(secret)
        val bodies = listOf("replay", "output").map { type ->
            """{"type":"$type","columns":3,"rows":1,""" +
                """"grid":{"columns":3,"rows":1,"row_spans":[{"row":0,"column":0,"text":"hi"}]}}"""
        }
        val upgrade = MockResponse().withWebSocketUpgrade(object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) {
                val deflater = Deflater(Deflater.DEFAULT_COMPRESSION, true)
                for (body in bodies) {
                    val payload = byteArrayOf(PAYLOAD_DEFLATE_STREAM) +
                        deflater.syncFlush(body.toByteArray(Charsets.UTF_8))
                    val n = serverSession.nextSendCounter()
                    val ct = cipher.seal(secret, nonce(DIR_AGENT_TO_DEVICE, n), payload)
                    val frame = ByteArray(8 + ct.size)
                    ByteBuffer.wrap(frame, 0, 8).putLong(n)
                    ct.copyInto(frame, 8)
                    webSocket.send(frame.toByteString())
                }
                deflater.end()
            }
        })
        // Deflate is always confirmed, so the unconfirmed case is the one that
        // actually matters: a bridge that compresses per frame but not against a
        // window. Its decoder must reject the stream tag rather than guess.
        upgrade.setHeader(DEFLATE_HEADER, "1")
        if (confirmStream) upgrade.setHeader(STREAM_HEADER, "1")
        server.enqueue(upgrade)

        val ts = TerminalSocket(
            OkHttpClient(),
            server.url("/").toString(),
            "surface-1",
            SharedSecretSession(secret),
            cipher,
        )
        val got = mutableListOf<TerminalDown>()
        val both = CompletableDeferred<Unit>()
        val job = launch(Dispatchers.IO) {
            runCatching {
                ts.connect().collect {
                    got += it
                    if (got.size == bodies.size) both.complete(Unit)
                }
            }
        }
        withTimeoutOrNull(3_000) { both.await() }
        job.cancelAndJoin()
        got
    }

    /** Compresses [body] as one chunk of an ongoing stream, ending it on a sync
     *  flush so the window survives -- what the Go side's StreamEncoder does. */
    private fun Deflater.syncFlush(body: ByteArray): ByteArray {
        setInput(body)
        val out = ByteArrayOutputStream(body.size + 64)
        val chunk = ByteArray(4096)
        do {
            val n = deflate(chunk, 0, chunk.size, Deflater.SYNC_FLUSH)
            out.write(chunk, 0, n)
        } while (n == chunk.size)
        return out.toByteArray()
    }

    @Test
    fun readsFramesCompressedAgainstEarlierOnesOnceTheBridgeConfirms() {
        val frames = twoFramesThroughOneWindow(confirmStream = true)
        assertEquals(listOf("replay", "output"), frames.map { it.type })
        assertEquals("hi ", RenderGridDecoder.decode(frames[1].grid!!).lines[0].text)
    }

    /**
     * The regression the third handshake exists for. A bridge that confirmed
     * only deflate must never be fed to the streaming decoder, and vice versa --
     * without the confirmation an app cannot tell a standalone frame from a
     * chunk of a stream, and reads every one of them wrong.
     */
    @Test
    fun doesNotTreatFramesAsAStreamWithoutTheConfirmingHeader() {
        assertTrue(
            "an unconfirmed socket must not decode stream chunks",
            twoFramesThroughOneWindow(confirmStream = false).isEmpty(),
        )
    }

    /**
     * A streamed socket has to resync on a bad frame for the same reason a delta
     * one does: every later chunk is compressed against the one that failed, so
     * skipping it leaves the decoder permanently behind. Asserted with deltas
     * off, so only the stream can be what ends the flow.
     */
    @Test
    fun anUnreadableFrameEndsAStreamedSocketEvenWithoutDeltas() {
        val (endedWith, frame) = badThenGoodFrame(deltaConfirmed = false, streamConfirmed = true)
        assertTrue("want DesyncException so the reconnector resyncs", endedWith is DesyncException)
        assertNull("no frame may be delivered after the desync", frame)
    }
}
