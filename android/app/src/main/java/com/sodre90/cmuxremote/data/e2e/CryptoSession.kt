package com.sodre90.cmuxremote.data.e2e

import android.content.SharedPreferences
import android.util.Base64
import com.sodre90.cmuxremote.data.ConnectionSlot
import com.sodre90.cmuxremote.data.HostId
import com.sodre90.cmuxremote.data.hostSlotKey
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking

/** The subset of [CryptoSession] that [encryptBody]/[decryptBody]/[encryptFrame]/
 *  [decryptFrame] need -- lets tests substitute an in-memory fake. */
interface PairedSession {
    fun sharedSecret(): ByteArray?
    fun nextSendCounter(): Long

    /**
     * Runs [decrypt] only once [n] has passed the replay window, and records
     * [n] as seen only once [decrypt] has returned normally -- all three
     * steps under one lock. Mirrors the agent's ValidateAndCommitRecvCounter
     * (bridge/internal/e2e/store.go); holding the two sides to the same shape
     * is what keeps the invariant checkable on both.
     *
     * One call rather than a check followed by a commit, because the gap
     * between them was exploitable (cmux-app-uvh): a single session backs the
     * events socket, the terminal socket, the HTTP interceptor and the push
     * decoder at once, so two of them could both clear the window for the
     * same counter before either committed it -- and a frame the untrusted
     * relay duplicated would then be processed twice. Closing the gap also
     * means a re-pairing can no longer land mid-sequence and be overwritten
     * by an in-flight commit (cmux-app-a3g), since setPairing takes this
     * same lock.
     *
     * [decrypt] is handed the shared secret rather than reading it itself,
     * because that read has to happen under this same lock. A caller that
     * captured the secret first could otherwise decrypt a superseded
     * pairing's frame -- which still authenticates under the secret it
     * captured -- and commit its far-ahead counter into the window a
     * re-pairing had just reset, stranding the new pairing exactly as
     * cmux-app-a3g did.
     *
     * @throws ReplayRejectedException if the window refuses [n]; [decrypt] is
     *   then never invoked, so a forged counter cannot burn a counter the
     *   legitimate sender has yet to use.
     * @throws NotPairedException if no shared secret is on file.
     */
    fun <T> validateAndCommitRecvCounter(n: Long, decrypt: (sharedSecret: ByteArray) -> T): T
}

/**
 * One paired-agent session for ([host], [slot]): the derived shared secret, a
 * durable monotonic send counter, and the sliding-window receive gate. The
 * phone holds exactly one session per host and slot at a time -- re-pairing
 * that slot overwrites its own record, but every other session is untouched
 * (all of them share one prefs file, distinguished only by prefix).
 */
class CryptoSession(
    private val prefs: SharedPreferences,
    private val host: HostId,
    private val slot: ConnectionSlot,
) : PairedSession {

    // In-memory mirror of the send counter and replay window, so the hot
    // path (once per outbound keystroke / inbound frame) never re-decrypts
    // these from EncryptedSharedPreferences. Every method that touches
    // either field -- including setPairing/clear, not
    // just the hot-path nextSendCounter/validateAndCommitRecvCounter --
    // synchronizes on `this` and routes its prefs write through writeScope
    // (persist() or the blocking persistDurably()), so (a) callers on
    // different threads (Compose's main thread for sends, an OkHttp callback
    // thread for recvs, the pairing/forget flows) never see a torn read, and
    // (b) the on-disk value always converges to match whichever write was
    // most recently applied in memory -- a queued write from before a reset
    // can't land after it and resurrect stale counters, since writeScope is
    // single-threaded FIFO regardless of which of the two helpers enqueued
    // it. nextSendCounter/validateAndCommitRecvCounter use persistDurably
    // specifically because their value is handed out (as an AEAD nonce
    // input) before this class can know it was ever durably written;
    // setPairing/clear reset both fields to fixed constants, so a lost write
    // there just reappears identically on the next mutation, not as a
    // reusable nonce.
    //
    // peer_pubkey/shared_secret are not cached in memory -- sharedSecret()
    // reads straight from prefs -- but their writes DO take this same lock,
    // because the secret and the replay window have to change together: a
    // decrypt that paired one pairing's window with another's key is what
    // cmux-app-a3g was.
    private var sendCounter: Long = prefs.getLong(key(KEY_SEND_COUNTER), 0L)
    private var replayWindow: ReplayWindow =
        ReplayWindow(prefs.getLong(key(KEY_RECV_HIGHEST), -1L), prefs.getLong(key(KEY_RECV_WINDOW_BITS), 0L))

    // Single-threaded so writes are applied in the same order they were
    // committed in memory -- Dispatchers.IO's shared pool alone would not
    // guarantee that for independently launched coroutines.
    private val writeScope = CoroutineScope(SupervisorJob() + Dispatchers.IO.limitedParallelism(1))

    private fun persist(block: SharedPreferences.Editor.() -> Unit) {
        writeScope.launch {
            val editor = prefs.edit()
            editor.block()
            editor.apply()
        }
    }

    // Send/recv counters back the AEAD nonce, so a lost write is a security
    // bug, not just a stale cache: apply() only queues the disk write and can
    // be dropped entirely by a process death (crash/OOM-kill/force-stop)
    // before it lands, unlike commit(), which blocks until the write is on
    // disk. Blocking here (via runBlocking) keeps the same writeScope
    // ordering persist() relies on elsewhere -- still one queue, one writer
    // -- while guaranteeing the counter this call just handed out can never
    // be re-issued after a restart. The callers (encryptFrame/decryptFrame,
    // on the terminal socket's send path and OkHttp's WS reader thread; the
    // e2e HTTP interceptor, on OkHttp's dispatcher pool) are already
    // throttled well above single-digit-millisecond disk-write latency by
    // DeliveryTracker's one-RPC-in-flight gate and OkHttp's own connection
    // handling, so this doesn't introduce user-visible jank.
    private fun persistDurably(block: SharedPreferences.Editor.() -> Unit) {
        val job = writeScope.launch {
            val editor = prefs.edit()
            editor.block()
            editor.commit()
        }
        runBlocking { job.join() }
    }

    fun isPaired(): Boolean = prefs.contains(key(KEY_SHARED_SECRET))

    override fun sharedSecret(): ByteArray? =
        prefs.getString(key(KEY_SHARED_SECRET), null)?.let { Base64.decode(it, Base64.NO_WRAP) }

    /** Called once, by [com.sodre90.cmuxremote.data.pairing.PairingClient] after a
     *  successful pairing handshake. Resets counters and the replay window --
     *  a fresh pairing means a fresh shared secret, so old counter state is
     *  meaningless (and reusing it would incorrectly reject the first messages). */
    fun setPairing(peerPublicKey: ByteArray, sharedSecret: ByteArray) {
        // Secret and window move together under the one lock
        // validateAndCommitRecvCounter holds, so an in-flight decrypt can
        // never pair this pairing's fresh window with the previous one's
        // secret (cmux-app-a3g).
        synchronized(this) {
            prefs.edit()
                .putString(key(KEY_PEER_PUBLIC_KEY), Base64.encodeToString(peerPublicKey, Base64.NO_WRAP))
                .putString(key(KEY_SHARED_SECRET), Base64.encodeToString(sharedSecret, Base64.NO_WRAP))
                .apply()
            sendCounter = 0L
            replayWindow = ReplayWindow()
        }
        persist {
            putLong(key(KEY_SEND_COUNTER), 0L)
            putLong(key(KEY_RECV_HIGHEST), -1L)
            putLong(key(KEY_RECV_WINDOW_BITS), 0L)
        }
    }

    /** Durable -- the bump is on disk before this returns, never reset across
     *  reconnects -- so a value handed out here can never be handed out
     *  again after a crash/kill, which would otherwise reuse an AEAD nonce. */
    override fun nextSendCounter(): Long {
        val n = synchronized(this) {
            val current = sendCounter
            sendCounter = current + 1
            current
        }
        persistDurably { putLong(key(KEY_SEND_COUNTER), n + 1) }
        return n
    }

    /** The write is durable before this returns, so a crash/kill can't
     *  re-widen the window back to a point that would re-accept an
     *  already-processed (captured) frame. It happens under the lock rather
     *  than after it so two commits cannot land on disk in the opposite
     *  order they were applied in memory, which would regress the window to
     *  the older of the two -- the agent holds s.mu across its own UPDATE for
     *  the same reason. [decrypt] must not call back into this session. */
    override fun <T> validateAndCommitRecvCounter(n: Long, decrypt: (ByteArray) -> T): T = synchronized(this) {
        val secret = sharedSecret() ?: throw NotPairedException()
        if (!replayWindow.canAccept(n)) throw ReplayRejectedException(n, replayWindow.highestSeen)
        val plaintext = decrypt(secret)
        val updated = replayWindow.commit(n)
        replayWindow = updated
        persistDurably {
            putLong(key(KEY_RECV_HIGHEST), updated.highestSeen)
            putLong(key(KEY_RECV_WINDOW_BITS), updated.windowBits)
        }
        plaintext
    }

    /** Wipes this session only -- used when forgetting this slot. Every other
     *  session (sharing the same prefs file) is untouched. */
    fun clear() {
        synchronized(this) {
            prefs.edit()
                .remove(key(KEY_PEER_PUBLIC_KEY))
                .remove(key(KEY_SHARED_SECRET))
                .apply()
            sendCounter = 0L
            replayWindow = ReplayWindow()
        }
        persist {
            remove(key(KEY_SEND_COUNTER))
            remove(key(KEY_RECV_HIGHEST))
            remove(key(KEY_RECV_WINDOW_BITS))
        }
    }

    private fun key(base: String) = hostSlotKey(host, slot, base)

    companion object {
        const val PREFS_NAME = "cmux_e2e_session"
        const val KEY_PEER_PUBLIC_KEY = "device_public_key_b64"
        const val KEY_SHARED_SECRET = "shared_secret_b64"
        const val KEY_SEND_COUNTER = "send_counter"
        const val KEY_RECV_HIGHEST = "recv_highest"
        const val KEY_RECV_WINDOW_BITS = "recv_window_bits"
    }
}
