package com.sodre90.cmuxremote.ui.terminal

import com.sodre90.cmuxremote.model.TerminalUp
import com.sodre90.cmuxremote.model.TerminalUpType
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

// How long a sent-but-unacknowledged message can stay pending before the UI
// treats it as stuck rather than merely in flight. Keystrokes are a few bytes,
// so this is a round trip plus the bridge's subprocess spawn and little else.
private const val ACK_STALE_MS = 1_500L

// An attachment is not a few bytes. Its ack cannot arrive before the upload
// does, so its own stale threshold grows with its size at a pessimistic
// mobile uplink -- 50 KB/s -- rather than showing "delayed" for every photo
// sent over a weak signal.
private const val UPLOAD_BYTES_PER_MS = 50L

internal fun staleAfterMs(payloadBytes: Int): Long = ACK_STALE_MS + payloadBytes / UPLOAD_BYTES_PER_MS

// How long an explicit ack failure (ok == false) keeps deliveryStatus at
// DELAYED before fading back to whatever pendingAcks/neverSentQueue implies.
private const val FAILURE_DISPLAY_MS = 3_000L

/** Whether recently-sent input is confirmed delivered, still in flight, or
 *  stuck (sent-but-unacked past its stale threshold, provably unsent because
 *  the socket was down, or explicitly failed per the bridge's ack). */
enum class DeliveryStatus { CONFIRMED, SENDING, DELAYED }

/** How an attach ended: acked fine, refused by the bridge for [reason] (one
 *  of [com.sodre90.cmuxremote.model.AttachRefusal]), failed at cmux (null
 *  reason), or never left the phone. One-shot; the UI clears it once shown. */
data class AttachOutcome(val ok: Boolean, val reason: String? = null)

private class Pending(val sentAt: Long, val staleAfterMs: Long)

/**
 * The terminal's delivery-reliability (seq/ack) bookkeeping, owned by
 * [TerminalViewModel] and driven by its connection lifecycle ([onConnected]/
 * [onDisconnected]/[onAck]) plus a periodic [recomputeDeliveryStatus] poll.
 * [send] enqueues one message into whatever socket is currently active,
 * returning false if it provably never left the phone.
 *
 * Everything here is only ever touched from the main dispatcher (Compose
 * callbacks and viewModelScope's default Main.immediate coroutines), so no
 * explicit synchronization is needed.
 */
class DeliveryTracker(
    private val send: (TerminalUp) -> Boolean,
    private val now: () -> Long = System::currentTimeMillis,
    private val log: (String) -> Unit = {},
) {
    private var nextSeq = 1L // 0 means "unset" on the wire; never used.

    // Messages that provably never left the phone (socket was null/closed at
    // send time) -- safe to replay verbatim once a new socket connects, since
    // there's no risk of double-delivery.
    private val neverSentQueue = mutableListOf<TerminalUp>()

    // Messages that did enqueue into the socket, keyed by seq, awaiting an
    // "ack" frame. Deliberately never auto-resent: typed input isn't
    // idempotent, so an ambiguous (sent-but-unconfirmed) message is reported
    // to the user instead of risking a duplicate command.
    private val pendingAcks = mutableMapOf<Long, Pending>()

    // Seqs of attaches still awaiting their ack, so the outcome can be told
    // apart from a keystroke's and surfaced with its reason.
    private val pendingAttaches = mutableSetOf<Long>()
    private var lastFailureAt: Long = 0L

    private val pendingOutbound = StringBuilder()

    // Gates outbound input RPCs to one in flight at a time: each `cmux rpc`
    // call is a subprocess spawn (~150ms round trip observed live), far
    // slower than key-repeat (~30-50ms) can produce keystrokes. A fixed
    // debounce window can't keep pace with that; gating on the real
    // bottleneck (the ack) means anything typed while a request is in
    // flight coalesces into the next one, so the backlog can't grow
    // unboundedly the way it did with a timer-based flush.
    private var inFlightInputSeq: Long? = null

    private val _deliveryStatus = MutableStateFlow(DeliveryStatus.CONFIRMED)
    val deliveryStatus: StateFlow<DeliveryStatus> = _deliveryStatus.asStateFlow()

    // One-shot: set when a disconnect drops non-empty pendingAcks (their fate
    // is unknowable), cleared by the UI once shown.
    private val _lostInputNotice = MutableStateFlow(false)
    val lostInputNotice: StateFlow<Boolean> = _lostInputNotice.asStateFlow()

    fun dismissLostInputNotice() {
        _lostInputNotice.value = false
    }

    private val _attachOutcome = MutableStateFlow<AttachOutcome?>(null)
    val attachOutcome: StateFlow<AttachOutcome?> = _attachOutcome.asStateFlow()

    fun dismissAttachOutcome() {
        _attachOutcome.value = null
    }

    /** Sends an image for the bridge to land on disk and paste the path of.
     *  Not coalesced with typed input: it is one message, and the bridge
     *  handles the socket's frames in order, so text typed after it still
     *  arrives after the pasted path. Unlike input it is never queued for a
     *  later socket: the user is told it failed right away, and a replay
     *  after they had already retried would paste the path twice. */
    fun attach(imageBase64: String, name: String) {
        val stamped = TerminalUp(type = TerminalUpType.ATTACH, image = imageBase64, name = name, seq = nextSeq++)
        val sent = send(stamped)
        log("dispatch seq=${stamped.seq} type=${stamped.type} sent=$sent bytes=${imageBase64.length}")
        if (sent) {
            pendingAcks[stamped.seq] = Pending(now(), staleAfterMs(imageBase64.length))
            pendingAttaches.add(stamped.seq)
        } else {
            _attachOutcome.value = AttachOutcome(ok = false)
        }
    }

    /** Queues [text] for delivery. Coalesces rapid chunks (typed diffs,
     *  key-bar taps, paste) into one input message per bridge round trip --
     *  gated on the previous input's ack, not a fixed timer, since the
     *  bottleneck is the bridge's per-RPC subprocess spawn, not anything
     *  client-side. */
    fun sendText(text: String) {
        if (text.isEmpty()) return
        pendingOutbound.append(text)
        flushPendingOutboundIfIdle()
    }

    private fun flushPendingOutboundIfIdle() {
        if (inFlightInputSeq != null || pendingOutbound.isEmpty()) return
        flushPendingOutbound()
    }

    private fun flushPendingOutbound() {
        val text = pendingOutbound.toString()
        pendingOutbound.clear()
        inFlightInputSeq = dispatch(TerminalUp(type = TerminalUpType.INPUT, text = text))
    }

    /** Sends [text] as one paste message, never merged into typed input:
     *  the host delivers a paste differently from keystrokes. Anything typed
     *  before it is flushed first, in-flight gate or not, so the pane sees
     *  the two in the order the user produced them; typing after it waits
     *  on the paste's ack like any other in-flight input. */
    fun paste(text: String) {
        if (text.isEmpty()) return
        if (pendingOutbound.isNotEmpty()) flushPendingOutbound()
        inFlightInputSeq = dispatch(TerminalUp(type = TerminalUpType.PASTE, text = text))
    }

    fun resize(columns: Int, rows: Int) {
        dispatch(TerminalUp(type = TerminalUpType.RESIZE, columns = columns, rows = rows))
    }

    private fun dispatch(up: TerminalUp): Long {
        val stamped = up.copy(seq = nextSeq++)
        val sent = send(stamped)
        log("dispatch seq=${stamped.seq} type=${stamped.type} sent=$sent text=${stamped.text?.let(::describeForLog)}")
        if (sent) {
            pendingAcks[stamped.seq] = Pending(now(), ACK_STALE_MS)
        } else {
            neverSentQueue.add(stamped)
        }
        return stamped.seq
    }

    /** A new socket just delivered its first frame: replays messages that
     *  never actually left the phone (the socket was null/closed when they
     *  were dispatched) -- safe to replay verbatim, they're provably not
     *  duplicates -- then flushes anything typed while disconnected. */
    fun onConnected() {
        flushNeverSent()
        flushPendingOutboundIfIdle()
    }

    private fun flushNeverSent() {
        if (neverSentQueue.isEmpty()) return
        val queued = neverSentQueue.toList()
        neverSentQueue.clear()
        queued.forEach { up ->
            val sent = send(up)
            if (sent) pendingAcks[up.seq] = Pending(now(), ACK_STALE_MS) else neverSentQueue.add(up)
        }
    }

    /** The connection ended, gracefully or not: any still-unacked messages
     *  have an unknowable fate now -- drop them rather than risk a duplicate
     *  resend, and flag it. */
    fun onDisconnected() {
        if (pendingAcks.isNotEmpty()) {
            pendingAcks.clear()
            _lostInputNotice.value = true
        }
        if (pendingAttaches.isNotEmpty()) {
            pendingAttaches.clear()
            _attachOutcome.value = AttachOutcome(ok = false)
        }
        // The in-flight gate's ack (if any) can never arrive now that the
        // socket is gone -- clear it so a new connection isn't stuck
        // refusing to flush pendingOutbound forever.
        inFlightInputSeq = null
    }

    fun onAck(seq: Long, ok: Boolean, reason: String? = null) {
        log("ack seq=$seq ok=$ok reason=$reason")
        pendingAcks.remove(seq)
        if (pendingAttaches.remove(seq)) _attachOutcome.value = AttachOutcome(ok, reason)
        if (!ok) lastFailureAt = now()
        if (seq == inFlightInputSeq) {
            inFlightInputSeq = null
            flushPendingOutboundIfIdle()
        }
    }

    fun recomputeDeliveryStatus() {
        val now = now()
        val anyStale = pendingAcks.values.any { now - it.sentAt > it.staleAfterMs }
        _deliveryStatus.value = when {
            now - lastFailureAt < FAILURE_DISPLAY_MS -> DeliveryStatus.DELAYED
            neverSentQueue.isNotEmpty() -> DeliveryStatus.DELAYED
            pendingAcks.isEmpty() -> DeliveryStatus.CONFIRMED
            anyStale -> DeliveryStatus.DELAYED
            else -> DeliveryStatus.SENDING
        }
    }
}
