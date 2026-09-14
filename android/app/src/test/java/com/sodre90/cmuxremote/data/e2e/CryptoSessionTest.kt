package com.sodre90.cmuxremote.data.e2e

import android.app.Application
import android.content.Context
import com.sodre90.cmuxremote.data.ConnectionSlot
import com.sodre90.cmuxremote.data.HostId
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.annotation.Config
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicReference

/**
 * Covers what only a real [CryptoSession] can show: that the counter and
 * replay-window mutations really are serialized against each other and
 * against re-pairing (cmux-app-uvh, cmux-app-a3g).
 *
 * Robolectric supplies the real android.util.Base64 and a working
 * SharedPreferences; it does not supply an AndroidKeyStore, so the session is
 * built on plain prefs through the internal constructor. What that leaves
 * uncovered is EncryptedSharedPreferences' encryption at rest, which is
 * AndroidX's code and not what this class's locking is about (cmux-app-fdl).
 *
 * The default application is overridden because the real one builds the whole
 * AppContainer, and that does need a keystore.
 */
@RunWith(RobolectricTestRunner::class)
@Config(application = Application::class)
class CryptoSessionTest {

    private val oldSecret = ByteArray(32) { 0x11 }
    private val newSecret = ByteArray(32) { 0x22 }

    // One prefs file for both slots, as in the app: they are separated by key
    // prefix alone.
    private fun session(slot: ConnectionSlot = ConnectionSlot.RELAY): CryptoSession {
        val prefs = RuntimeEnvironment.getApplication()
            .getSharedPreferences("cmux_e2e_session_test", Context.MODE_PRIVATE)
        return CryptoSession(prefs, HostId("test-host"), slot)
    }

    private fun paired(slot: ConnectionSlot = ConnectionSlot.RELAY): CryptoSession =
        session(slot).also { it.setPairing(ByteArray(32) { 0x33 }, oldSecret) }

    /**
     * The gap cmux-app-uvh closed: two callers arriving at the same counter
     * used to be able to both pass the window check before either committed,
     * so a frame the untrusted relay duplicated got processed twice.
     */
    @Test
    fun onlyOneOfTwoThreadsRacingTheSameCounterGetsToDecryptIt() {
        val session = paired()
        val bothInside = CountDownLatch(2)
        val decrypts = java.util.concurrent.atomic.AtomicInteger()
        val rejections = java.util.concurrent.atomic.AtomicInteger()

        val racers = (1..2).map {
            Thread {
                bothInside.countDown()
                bothInside.await(5, TimeUnit.SECONDS)
                try {
                    session.validateAndCommitRecvCounter(7L) { decrypts.incrementAndGet() }
                } catch (e: ReplayRejectedException) {
                    rejections.incrementAndGet()
                }
            }.also { it.start() }
        }
        racers.forEach { it.join(5_000) }

        assertEquals("exactly one caller may decrypt counter 7", 1, decrypts.get())
        assertEquals("the loser must be told it is a replay", 1, rejections.get())
    }

    /** A rejected counter must not be burned: the window is only advanced by
     *  a decrypt that returned, so a forged early counter cannot consume one
     *  the legitimate sender has yet to send. */
    @Test
    fun aDecryptThatThrowsLeavesTheCounterStillAvailable() {
        val session = paired()
        runCatching { session.validateAndCommitRecvCounter(3L) { error("bad tag") } }

        var decrypted = false
        session.validateAndCommitRecvCounter(3L) { decrypted = true }
        assertTrue("a failed decrypt must not consume the counter", decrypted)
    }

    /**
     * The cmux-app-a3g race: a decrypt already inside the lock commits its
     * counter, then a re-pairing lands. The new pairing's window must win --
     * if the in-flight commit were applied after the reset, the fresh session
     * would start with its window somewhere up at the old counter and reject
     * everything the newly paired agent sends.
     */
    @Test
    fun aRePairingIsNotOvertakenByADecryptThatWasAlreadyInFlight() {
        val session = paired()
        val decryptStarted = CountDownLatch(1)
        val secretSeenByDecrypt = AtomicReference<ByteArray>()

        val decrypting = Thread {
            session.validateAndCommitRecvCounter(500L) { secret ->
                secretSeenByDecrypt.set(secret.copyOf())
                decryptStarted.countDown()
                Thread.sleep(200)
            }
        }.also { it.start() }

        assertTrue(decryptStarted.await(5, TimeUnit.SECONDS))
        val rePairing = Thread { session.setPairing(ByteArray(32) { 0x44 }, newSecret) }.also { it.start() }
        decrypting.join(5_000)
        rePairing.join(5_000)

        assertArrayEquals(
            "the in-flight decrypt has to keep the secret its window belonged to",
            oldSecret,
            secretSeenByDecrypt.get(),
        )
        assertArrayEquals("the new pairing's secret must be the one on file", newSecret, session.sharedSecret())

        var decryptedUnderNewPairing = false
        session.validateAndCommitRecvCounter(0L) { decryptedUnderNewPairing = true }
        assertTrue(
            "the window must be back at the start of the new pairing, not left at 500",
            decryptedUnderNewPairing,
        )
    }

    /** Send counters back the AEAD nonce, so concurrent senders must never be
     *  handed the same one. */
    @Test
    fun concurrentSendersNeverShareANonce() {
        val session = paired()
        val perThread = 50
        val threads = 4
        val start = CountDownLatch(threads)
        val issued = java.util.Collections.synchronizedList(mutableListOf<Long>())

        (1..threads).map {
            Thread {
                start.countDown()
                start.await(5, TimeUnit.SECONDS)
                repeat(perThread) { issued.add(session.nextSendCounter()) }
            }.also { it.start() }
        }.forEach { it.join(10_000) }

        assertEquals(threads * perThread, issued.size)
        assertEquals("every send counter must be handed out once", issued.size, issued.toSet().size)
    }

    /** Both slots share one prefs file, keyed only by prefix, so clearing one
     *  must not disturb the other -- that separation is the whole basis of
     *  dual pairing surviving a re-pair of a single slot. */
    @Test
    fun clearingOneSlotLeavesTheOtherPaired() {
        val relay = paired(ConnectionSlot.RELAY)
        val direct = paired(ConnectionSlot.DIRECT)

        relay.clear()

        assertNull(relay.sharedSecret())
        assertArrayEquals(oldSecret, direct.sharedSecret())
    }
}
