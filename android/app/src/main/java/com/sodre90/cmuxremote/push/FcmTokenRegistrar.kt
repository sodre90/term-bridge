package com.sodre90.cmuxremote.push

import com.sodre90.cmuxremote.data.FallbackBridgeClient

/** Where a token that no slot has accepted yet is kept between attempts.
 *  `Settings` is the real implementation; tests use an in-memory one so this
 *  file stays constructible without Android. */
interface PendingTokenStore {
    fun pendingFcmToken(): String?
    fun setPendingFcmToken(token: String?)
}

/** What [FcmTokenRegistrar.registerPending] concluded, and therefore whether the
 *  caller should be asked back. */
enum class RegistrationAttempt {
    /** Every paired host took the token; nothing is outstanding. */
    DONE,

    /** Nothing was outstanding to begin with. */
    NOTHING_PENDING,

    /** No host is paired yet. Pairing registers the token itself (see
     *  PairingClient), so waiting is right and retrying is not -- there is
     *  nothing to retry against. */
    NOT_CONFIGURED,

    /** Some host has yet to accept the token and the attempt should be
     *  repeated. A host that already accepted it is asked again on the retry:
     *  registration is an upsert on every server, so that costs one request
     *  and keeps this a single yes/no rather than a per-host ledger. */
    RETRY,
}

/**
 * Owns the one job of getting the current FCM token onto the bridge, and keeps
 * trying until it lands.
 *
 * FCM delivers a rotated token exactly once, to onNewToken. Both callers used to
 * fire a coroutine that swallowed every failure and left the retry to whenever
 * the user next opened the app -- so an app update installed overnight, or one
 * whose token rotated while the relay was down, left the server holding a token
 * FCM accepts with a 2xx and delivers nothing to. Push was silently dead until
 * the next launch (cmux-app-2cm, diagnosed live 2026-08-19).
 *
 * Every paired host gets the token, not just the selected one: each keeps its
 * own device table and any of them may be the one that later pushes.
 *
 * The retry itself is WorkManager's job (see [FcmTokenRegistrationWorker]);
 * this type holds the decision so it can be tested without Android.
 */
class FcmTokenRegistrar(
    private val store: PendingTokenStore,
    private val pairedBridges: () -> List<FallbackBridgeClient>,
) {

    /** Records [token] as outstanding. Idempotent for a token already pending,
     *  and it deliberately overwrites a different pending token: only the newest
     *  one FCM issued is worth registering. */
    fun onTokenIssued(token: String) {
        store.setPendingFcmToken(token)
    }

    suspend fun registerPending(): RegistrationAttempt {
        val token = store.pendingFcmToken() ?: return RegistrationAttempt.NOTHING_PENDING
        val bridges = pairedBridges().ifEmpty { return RegistrationAttempt.NOT_CONFIGURED }
        return try {
            bridges.forEach { it.registerDevice(token) }
            // Only after a slot has accepted it. Clearing on the attempt rather
            // than the acceptance would reintroduce the whole bug.
            store.setPendingFcmToken(null)
            RegistrationAttempt.DONE
        } catch (_: Exception) {
            RegistrationAttempt.RETRY
        }
    }
}
