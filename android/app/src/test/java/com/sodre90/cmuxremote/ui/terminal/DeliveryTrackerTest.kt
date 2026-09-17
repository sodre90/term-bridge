package com.sodre90.cmuxremote.ui.terminal

import com.sodre90.cmuxremote.model.AttachRefusal
import com.sodre90.cmuxremote.model.TerminalUp
import com.sodre90.cmuxremote.model.TerminalUpType
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The tracker is fully synchronous (the ViewModel drives it from the main
 * dispatcher), so these are plain JUnit tests: a recording send lambda plays
 * the socket, [clock] plays the wall clock, and connection lifecycle is
 * simulated by calling onConnected/onDisconnected/onAck directly.
 */
class DeliveryTrackerTest {

    private var clock = 1_000_000L
    private var socketUp = true
    private val sent = mutableListOf<TerminalUp>()

    private fun newTracker() = DeliveryTracker(
        send = { up ->
            if (socketUp) {
                sent.add(up)
                true
            } else {
                false
            }
        },
        now = { clock },
    )

    @Test
    fun sentButUnackedInputIsNeverResentAcrossAReconnect() {
        val tracker = newTracker()
        tracker.sendText("rm -rf tmp") // non-idempotent: a resend would be destructive
        assertEquals(1, sent.size)

        tracker.onDisconnected() // its ack can never arrive
        tracker.onConnected()

        assertEquals(1, sent.size)
        assertTrue(tracker.lostInputNotice.value)
    }

    @Test
    fun neverSentMessagesFlushInOrderOnReconnectAndExactlyOnce() {
        socketUp = false
        val tracker = newTracker()
        tracker.sendText("a") // dispatched immediately, fails -> never-sent queue
        tracker.resize(80, 24) // bypasses the input gate, fails -> never-sent queue
        assertEquals(0, sent.size)

        socketUp = true
        tracker.onConnected()

        assertEquals(listOf("input" to 1L, "resize" to 2L), sent.map { it.type to it.seq })
        assertEquals("a", sent[0].text)

        tracker.onConnected() // a second reconnect must not replay them
        assertEquals(2, sent.size)
    }

    @Test
    fun neverSentMessagesThatFailAgainStayQueuedForTheNextConnect() {
        socketUp = false
        val tracker = newTracker()
        tracker.sendText("a")

        tracker.onConnected() // socket died again before the flush
        assertEquals(0, sent.size)

        socketUp = true
        tracker.onConnected()
        assertEquals(1, sent.size)
        assertEquals(1L, sent.single().seq)
    }

    @Test
    fun textTypedWhileAnAckIsInFlightCoalescesIntoTheNextMessage() {
        val tracker = newTracker()
        tracker.sendText("a")
        tracker.sendText("b")
        tracker.sendText("c")
        assertEquals(1, sent.size) // "b"/"c" wait on "a"'s ack

        tracker.onAck(seq = 1L, ok = true)

        assertEquals(2, sent.size)
        assertEquals("bc", sent[1].text)
        assertEquals(2L, sent[1].seq)
    }

    @Test
    fun textTypedWhileDisconnectedFlushesAsOneNewMessageOnReconnect() {
        val tracker = newTracker()
        tracker.sendText("a")
        tracker.sendText("b") // gated behind "a"'s in-flight ack

        tracker.onDisconnected() // "a" is dropped (unknowable fate), gate cleared
        tracker.onConnected()

        assertEquals(listOf("a", "b"), sent.map { it.text })
        assertEquals(listOf(1L, 2L), sent.map { it.seq }) // "a" was not resent under a new seq
    }

    @Test
    fun aPasteIsItsOwnMessageAndNeverMergesWithTypedInput() {
        val tracker = newTracker()
        tracker.paste("one\ntwo")
        tracker.sendText("x") // waits on the paste's ack like any input

        assertEquals(listOf("paste"), sent.map { it.type })
        assertEquals("one\ntwo", sent.single().text)

        tracker.onAck(seq = 1L, ok = true)
        assertEquals(listOf("paste" to "one\ntwo", "input" to "x"), sent.map { it.type to it.text })
    }

    @Test
    fun textTypedBeforeAPasteReachesThePaneFirstEvenWhileGated() {
        val tracker = newTracker()
        tracker.sendText("a")
        tracker.sendText("b") // gated behind "a"'s in-flight ack
        tracker.paste("one\ntwo")

        assertEquals(listOf("a", "b", "one\ntwo"), sent.map { it.text })
        assertEquals(listOf("input", "input", "paste"), sent.map { it.type })
    }

    @Test
    fun anEmptyPasteSendsNothing() {
        val tracker = newTracker()
        tracker.paste("")
        assertEquals(0, sent.size)
    }

    @Test
    fun resizeBypassesTheInputGate() {
        val tracker = newTracker()
        tracker.sendText("a")
        tracker.resize(80, 24)

        assertEquals(listOf("input", "resize"), sent.map { it.type })
    }

    @Test
    fun emptyTextSendsNothing() {
        val tracker = newTracker()
        tracker.sendText("")
        assertEquals(0, sent.size)
    }

    @Test
    fun disconnectWithoutPendingAcksLeavesTheLostInputNoticeUnset() {
        val tracker = newTracker()
        tracker.sendText("a")
        tracker.onAck(seq = 1L, ok = true)

        tracker.onDisconnected()

        assertFalse(tracker.lostInputNotice.value)
    }

    @Test
    fun lostInputNoticeIsDismissable() {
        val tracker = newTracker()
        tracker.sendText("a")
        tracker.onDisconnected()
        assertTrue(tracker.lostInputNotice.value)

        tracker.dismissLostInputNotice()

        assertFalse(tracker.lostInputNotice.value)
    }

    @Test
    fun deliveryStatusTracksTheAckLifecycle() {
        val tracker = newTracker()
        tracker.recomputeDeliveryStatus()
        assertEquals(DeliveryStatus.CONFIRMED, tracker.deliveryStatus.value)

        tracker.sendText("a")
        tracker.recomputeDeliveryStatus()
        assertEquals(DeliveryStatus.SENDING, tracker.deliveryStatus.value)

        clock += 1_501 // past ACK_STALE_MS: in flight for too long
        tracker.recomputeDeliveryStatus()
        assertEquals(DeliveryStatus.DELAYED, tracker.deliveryStatus.value)

        tracker.onAck(seq = 1L, ok = true)
        tracker.recomputeDeliveryStatus()
        assertEquals(DeliveryStatus.CONFIRMED, tracker.deliveryStatus.value)
    }

    @Test
    fun aFailedAckShowsDelayedUntilTheFailureDisplayWindowLapses() {
        val tracker = newTracker()
        tracker.sendText("a")
        tracker.onAck(seq = 1L, ok = false)

        tracker.recomputeDeliveryStatus()
        assertEquals(DeliveryStatus.DELAYED, tracker.deliveryStatus.value)

        clock += 3_000 // FAILURE_DISPLAY_MS
        tracker.recomputeDeliveryStatus()
        assertEquals(DeliveryStatus.CONFIRMED, tracker.deliveryStatus.value)
    }

    @Test
    fun queuedNeverSentInputShowsDelayed() {
        socketUp = false
        val tracker = newTracker()
        tracker.sendText("a")

        tracker.recomputeDeliveryStatus()

        assertEquals(DeliveryStatus.DELAYED, tracker.deliveryStatus.value)
    }

    @Test
    fun aFailedAckUnblocksTheCoalescingGate() {
        val tracker = newTracker()
        tracker.sendText("a")
        tracker.sendText("b")

        tracker.onAck(seq = 1L, ok = false)

        assertEquals(2, sent.size) // "b" still flushes; the failure only affects status display
        assertEquals("b", sent[1].text)
    }

    // --- attachments ---

    private val aPhoto = "A".repeat(400_000) // ~300 KB of JPEG once decoded

    @Test
    fun anAttachGoesOutAsOneFrameWithItsImageAndName() {
        val tracker = newTracker()
        tracker.attach(aPhoto, "IMG_2041.jpg")
        assertEquals(1, sent.size)
        assertEquals(TerminalUpType.ATTACH, sent[0].type)
        assertEquals(aPhoto, sent[0].image)
        assertEquals("IMG_2041.jpg", sent[0].name)
        assertNull(tracker.attachOutcome.value)
    }

    @Test
    fun aLargeUploadIsStillSendingWhereAKeystrokeWouldBeDelayed() {
        val tracker = newTracker()
        tracker.attach(aPhoto, "a.jpg")
        clock += 3_000 // well past the keystroke threshold of 1.5 s
        tracker.recomputeDeliveryStatus()
        assertEquals(DeliveryStatus.SENDING, tracker.deliveryStatus.value)

        clock += staleAfterMs(aPhoto.length)
        tracker.recomputeDeliveryStatus()
        assertEquals(DeliveryStatus.DELAYED, tracker.deliveryStatus.value)
    }

    @Test
    fun aKeystrokeNextToAnUploadKeepsItsOwnShortThreshold() {
        val tracker = newTracker()
        tracker.attach(aPhoto, "a.jpg")
        tracker.sendText("x")
        clock += 3_000
        tracker.recomputeDeliveryStatus()
        assertEquals(DeliveryStatus.DELAYED, tracker.deliveryStatus.value)
    }

    @Test
    fun theAckCarriesTheBridgesReasonToTheOutcome() {
        val tracker = newTracker()
        tracker.attach(aPhoto, "a.jpg")
        tracker.onAck(sent[0].seq, ok = false, reason = AttachRefusal.TOO_LARGE)
        assertEquals(AttachOutcome(ok = false, reason = AttachRefusal.TOO_LARGE), tracker.attachOutcome.value)
        tracker.dismissAttachOutcome()
        assertNull(tracker.attachOutcome.value)
    }

    @Test
    fun aSuccessfulAttachReportsSuccessAndAKeystrokeAckDoesNot() {
        val tracker = newTracker()
        tracker.sendText("x")
        tracker.attach(aPhoto, "a.jpg")
        tracker.onAck(sent[0].seq, ok = true)
        assertNull(tracker.attachOutcome.value)
        tracker.onAck(sent[1].seq, ok = true)
        assertEquals(AttachOutcome(ok = true), tracker.attachOutcome.value)
    }

    /** An attach the socket could not take is reported failed at once and is
     *  NOT queued for the next socket: the user will retry, and a replay on
     *  top of that retry would paste the path twice. */
    @Test
    fun anAttachWhileDisconnectedFailsNowAndIsNeverReplayed() {
        val tracker = newTracker()
        socketUp = false
        tracker.attach(aPhoto, "a.jpg")
        assertEquals(AttachOutcome(ok = false), tracker.attachOutcome.value)
        assertTrue(sent.isEmpty())

        socketUp = true
        tracker.onConnected()
        assertTrue(sent.isEmpty())
    }

    @Test
    fun anAttachWhoseSocketDropsMidFlightIsReportedFailed() {
        val tracker = newTracker()
        tracker.attach(aPhoto, "a.jpg")
        tracker.onDisconnected()
        assertEquals(AttachOutcome(ok = false), tracker.attachOutcome.value)
        tracker.onConnected()
        assertEquals(1, sent.size)
    }
}
