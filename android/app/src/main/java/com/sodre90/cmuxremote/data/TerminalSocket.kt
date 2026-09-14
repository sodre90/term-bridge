package com.sodre90.cmuxremote.data

import com.sodre90.cmuxremote.data.e2e.Cipher
import com.sodre90.cmuxremote.data.e2e.PairedSession
import com.sodre90.cmuxremote.data.e2e.StreamPayloadDecoder
import com.sodre90.cmuxremote.data.e2e.decodePayload
import com.sodre90.cmuxremote.data.e2e.decryptFrame
import com.sodre90.cmuxremote.data.e2e.encryptFrame
import com.sodre90.cmuxremote.model.BridgeJson
import com.sodre90.cmuxremote.model.TerminalDown
import com.sodre90.cmuxremote.model.TerminalUp
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okio.ByteString
import okio.ByteString.Companion.toByteString

/**
 * Bidirectional `WS /terminal/{surfaceId}`: [connect] streams server frames
 * (replay snapshot then output updates), [send] pushes input/paste/resize.
 * Every frame is XChaCha20-Poly1305-encrypted binary (see data/e2e/Frame.kt) --
 * plaintext JSON-over-WS is no longer the wire format.
 */
/**
 * The bridge's `wire.CloseSurfaceGone`: this surface does not exist and never
 * will again, so stop reconnecting to it. Mirrored from
 * `bridge/internal/wire/terminal.go` -- keep the two in step.
 *
 * 4404 is in RFC 6455's private application range. It arrives with an empty
 * reason string on purpose; the number carries the whole message.
 */
internal const val CLOSE_SURFACE_GONE = 4404

/**
 * The response header the bridge sets on the 101 to confirm it accepted the
 * `?deflate=1` request. Mirrors `deflateHeader` in
 * bridge/internal/server/terminal.go.
 *
 * Asking is not enough to start stripping codec tags: an older bridge ignores
 * the query and keeps sending untagged JSON, whose leading `{` would be read as
 * a tag and drop every frame. Compression is armed only once the bridge has
 * said it is compressing, which makes all four app/bridge version pairings
 * work.
 */
internal const val DEFLATE_HEADER = "X-Cmux-Deflate"

/**
 * The bridge's confirmation that it will send delta frames, leaving out
 * render-grid blocks the socket already carries. Mirrors `deltaHeader` in
 * bridge/internal/server/terminal.go.
 *
 * Negotiated for the same reason as [DEFLATE_HEADER], and one more: a frame
 * with `scrollback_spans` left out is indistinguishable, to an app that does
 * not know about deltas, from a pane whose scrollback just emptied.
 */
internal const val DELTA_HEADER = "X-Cmux-Delta"

/**
 * The bridge's confirmation that it will compress the socket's frames against
 * one shared window rather than each on its own. Mirrors `streamHeader` in
 * bridge/internal/server/terminal.go.
 *
 * Asked for alongside `?deflate=1` but negotiated separately, because it is a
 * strictly stronger promise: a bridge that only confirmed deflate sends frames
 * this side can still read one at a time, while a chunk of a stream is readable
 * only in order and only by the decoder that saw every chunk before it.
 */
internal const val STREAM_HEADER = "X-Cmux-Deflate-Stream"

/**
 * A frame arrived but could not be turned into a [TerminalDown].
 *
 * Ends the flow instead of skipping the frame. While every frame was
 * self-contained, dropping one was survivable -- the next full grid healed the
 * screen a moment later. Delta frames are not self-contained: a lost frame that
 * changed the scrollback is followed by frames that omit the scrollback as
 * "unchanged", and the pane would then carry the wrong history for as long as
 * the socket stayed open, with nothing to notice it. Closing hands the problem
 * to the reconnect loop, which reopens and gets a fresh full replay -- and
 * resets the bridge's own record of what it has sent along with it.
 */
class DesyncException(cause: Throwable) : Exception("terminal frame undecodable", cause)

class TerminalSocket(
    private val http: OkHttpClient,
    baseUrl: String,
    surfaceId: String,
    private val session: PairedSession,
    private val cipher: Cipher,
    pollMs: Int = DEFAULT_TERMINAL_POLL_MS,
) {
    // poll_ms and rows need no confirming response header the way deflate and
    // delta do: frames decode identically whatever the interval, and a frame
    // without rows_changed is a whole screen, so there is nothing for this side
    // to arm. A bridge too old to know either parameter ignores it.
    private val url =
        "${baseUrl.trimEnd('/')}/terminal/$surfaceId?deflate=1&delta=1&stream=1&rows=1&poll_ms=$pollMs"

    @Volatile
    private var socket: WebSocket? = null

    // Set from onOpen, which OkHttp guarantees runs before any onMessage, so
    // no frame is ever read before either is known.
    @Volatile
    private var deflated = false

    @Volatile
    private var delta = false

    @Volatile
    private var streamed = false

    /** [onOpen] fires on the WebSocket upgrade, before any frame -- see
     *  [EventsSocket.connect] for why that can't come through the flow. */
    fun connect(onOpen: () -> Unit = {}): Flow<TerminalDown> = callbackFlow {
        val request = Request.Builder().url(url).build()
        // One decoder for this socket's whole life. It holds the window every
        // frame after the first is compressed against, so it belongs to the
        // connection, not to a frame -- and it is released in awaitClose rather
        // than after each one.
        val stream = StreamPayloadDecoder()
        val ws = http.newWebSocket(
            request,
            object : WebSocketListener() {
                override fun onOpen(webSocket: WebSocket, response: Response) {
                    deflated = response.header(DEFLATE_HEADER) == "1"
                    delta = response.header(DELTA_HEADER) == "1"
                    streamed = response.header(STREAM_HEADER) == "1"
                    onOpen()
                }

                override fun onMessage(webSocket: WebSocket, bytes: ByteString) {
                    runCatching { decryptFrame(session, cipher, bytes.toByteArray()) }
                        .mapCatching {
                            when {
                                streamed -> stream.decode(it)
                                deflated -> decodePayload(it)
                                else -> it
                            }
                        }
                        .mapCatching {
                            BridgeJson.decodeFromString(
                                TerminalDown.serializer(),
                                it.toString(Charsets.UTF_8)
                            )
                        }
                        .onSuccess { trySend(it) }
                        // On a delta stream a frame the app cannot read is not
                        // survivable by skipping it: later frames complete
                        // themselves from one this socket never saw. Reconnect
                        // and resync from a full replay instead. Decrypt
                        // failures come through here too -- they are a desync
                        // by any other name, and the reconnect is the same
                        // remedy. A shared compression window makes the same
                        // demand for the same reason -- a chunk this socket
                        // never decoded leaves every later chunk unreadable --
                        // so it forces a reconnect too. Off both, the old
                        // behaviour stands, because each frame still stands
                        // alone.
                        .onFailure {
                            android.util.Log.w("TerminalSocket", "dropped frame: ${it.message}")
                            if (delta || streamed) close(DesyncException(it))
                        }
                }

                override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
                    webSocket.close(code, reason)
                    // Closing with a cause rather than gracefully is what makes
                    // the reconnect loop see this at all: a flow that simply
                    // completes is indistinguishable from a dropped connection,
                    // which is how the phone retried a dead surface every 5s
                    // behind a spinner (cmux-app-34c).
                    close(if (code == CLOSE_SURFACE_GONE) SubscriptionGoneException() else null)
                }

                override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                    close(t)
                }
            }
        )
        socket = ws
        awaitClose {
            ws.cancel()
            stream.close()
            if (socket === ws) socket = null
        }
    }

    /**
     * Sends a client->server message. Returns false (a no-op) if the socket
     * isn't open -- the caller can use that to know the message definitely
     * never left the phone, as opposed to having been sent but not yet (or
     * never) acknowledged.
     */
    fun send(up: TerminalUp): Boolean {
        val plaintext = BridgeJson.encodeToString(TerminalUp.serializer(), up).toByteArray(Charsets.UTF_8)
        return socket?.send(encryptFrame(session, cipher, plaintext).toByteString()) ?: false
    }

    fun close() {
        socket?.close(1000, null)
        socket = null
    }
}
