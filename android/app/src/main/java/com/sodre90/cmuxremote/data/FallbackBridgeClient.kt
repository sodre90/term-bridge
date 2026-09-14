package com.sodre90.cmuxremote.data

import com.sodre90.cmuxremote.model.FeedReply
import com.sodre90.cmuxremote.model.HostInfo
import com.sodre90.cmuxremote.model.PendingFeedItem
import com.sodre90.cmuxremote.model.WorkspacesResponse
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import java.io.IOException

/**
 * Wraps a primary (relay) and fallback (direct) [BridgeClient] supplier.
 * Every call tries primary first and transparently retries against
 * fallback on a genuine reachability failure -- a transport-level
 * [IOException], or a [BridgeException] with a 5xx code (relay reachable,
 * but its tunnel to the Mac agent is broken) -- remembering the failure in
 * [relayHealth] so a dead relay isn't re-tried on every single call. By
 * default each instance gets its own private [RelayHealth] (a fresh process
 * always tries primary first again); [AppContainer] passes one shared
 * instance so this and [SocketReconnector] learn "relay is down" once.
 *
 * A 4xx [BridgeException] from primary is an application-level error (bad
 * request, auth, stale pairing, etc.), not a reachability problem: it is
 * NOT treated as a failover trigger and propagates immediately with no
 * penalty set, so mutating calls (replyFeed/renameWorkspace/setYoloMode/
 * registerDevice) are never silently re-executed against the wrong backend.
 * A 401 is *reported* to [onCredentialRejected] on top of that -- it names
 * the one slot whose credential is gone -- but changes none of the above.
 *
 * [registerDevice] is the one exception to the try-primary-then-fallback
 * shape: it fans out to BOTH slots instead, because each keeps its own
 * device table and either may be the connection a push later arrives
 * through. See its own doc for why "whichever is reachable" was not enough.
 * Because it touches both slots on every launch it is also the app's only
 * routine check on the standby credential, which it reports per slot through
 * [onRegistrationOutcome].
 *
 * [sessions] and [pendingFeed] additionally retry a 409 not_paired through
 * [retryingNotPaired] -- see its doc for why. Every caller of these two reads
 * inherits that retry automatically; nothing else needs to duplicate it.
 */
class FallbackBridgeClient(
    private val primary: () -> BridgeClient?,
    private val fallback: () -> BridgeClient?,
    private val now: () -> Long = System::currentTimeMillis,
    private val relayHealth: RelayHealth = RelayHealth(),
    private val pairingRetryDelayMs: Long = NOT_PAIRED_RETRY_DELAY_MS,
    private val monitor: ConnectionMonitor = ConnectionMonitor(),
    private val onRegistrationOutcome: (ConnectionSlot, RegistrationOutcome) -> Unit = { _, _ -> },
    private val onCredentialRejected: (ConnectionSlot) -> Unit = {},
    private val onHostInfo: (HostInfo) -> Unit = {},
) {
    /**
     * Reports a 401 as that slot's credential being gone. On DIRECT that is
     * exact -- the agent's auth.Require is the only thing that can say it.
     *
     * On RELAY there is one other source, and it is a known limitation rather
     * than a bug worth machinery: the relay proxies these routes to the agent's
     * TrustedHandler, whose RequireRelayToken (server/trusted.go) answers 401
     * for a mismatched relay<->agent shared token, and the relay forwards that
     * verbatim. So a misconfigured shared token would be reported as "re-pair
     * the relay", which would not fix it. Rare -- that token is set once at
     * setup and everything else is broken too when it is wrong -- and the
     * report still points a human at the right machine. The launch probe is
     * unaffected: /devices/register is not mounted on the trusted route set.
     *
     * Reporting only. A 401 must not change what [call] does, or the
     * "4xx never fails over" rule that keeps a non-idempotent write from
     * running twice would have a hole in it. It also sets no [relayHealth]
     * penalty: a server that answered is not a server that is unreachable.
     */
    private fun reportIfRejected(slot: ConnectionSlot, e: IOException) {
        if (e is BridgeException && e.code == HTTP_UNAUTHORIZED) onCredentialRejected(slot)
    }

    private suspend fun <T> call(block: suspend (BridgeClient) -> T): T {
        val primaryClient = primary()
        val fallbackClient = fallback()

        // Skip a doomed primary attempt if it's not configured at all, or
        // we recently confirmed it's down (still inside the penalty window).
        val skipPrimary = primaryClient == null || relayHealth.isDown(now())
        if (skipPrimary) {
            val only = fallbackClient ?: primaryClient ?: throw BridgeException(0, "not configured")
            val slot = if (only === primaryClient) ConnectionSlot.RELAY else ConnectionSlot.DIRECT
            if (slot == ConnectionSlot.DIRECT && primaryClient != null) monitor.fallingBack(null)
            return try {
                block(only).also {
                    if (slot == ConnectionSlot.RELAY) relayHealth.markUp()
                    monitor.connected(slot)
                }
            } catch (e: IOException) {
                reportIfRejected(slot, e)
                // Report the skip: "direct failed" alone reads as though relay
                // was fine, when in fact it was never tried this time round.
                if (primaryClient != null && only !== primaryClient) {
                    throw BothTransportsFailedException(relayError = null, directError = e)
                        .also { monitor.failed(it.message.orEmpty()) }
                }
                throw e
            }
        }

        monitor.connecting(ConnectionSlot.RELAY)
        return try {
            block(primaryClient).also {
                // Proof the relay works, which is what wakes a socket
                // subscription still parked on DIRECT -- see RelayHealth.recoveries.
                relayHealth.markUp()
                monitor.connected(ConnectionSlot.RELAY)
            }
        } catch (e: IOException) {
            reportIfRejected(ConnectionSlot.RELAY, e)
            if (fallbackClient == null) throw e
            if (e is BridgeException && e.code in 400..499) throw e
            relayHealth.markDown(now())
            monitor.fallingBack(e.describeForUser())
            try {
                block(fallbackClient).also { monitor.connected(ConnectionSlot.DIRECT) }
            } catch (fallbackError: IOException) {
                reportIfRejected(ConnectionSlot.DIRECT, fallbackError)
                throw BothTransportsFailedException(relayError = e, directError = fallbackError)
                    .also { monitor.failed(it.message.orEmpty()) }
            }
        }
    }

    /**
     * Retries [block] a few times when it throws a 409 not_paired
     * [BridgeException]. Right after pairing completes, the phone can call a
     * read endpoint before the Mac agent's pair-device poll loop has derived
     * and stored the e2e session -- the relay authenticates the device's
     * token fine, but the agent replies 409 not_paired for that narrow
     * window. Deliberately not applied to the mutating calls below: a
     * retried write could double-apply once the race clears, so
     * replyFeed/renameWorkspace/setYoloMode/registerDevice propagate a 409
     * immediately, same as any other 4xx (see [call]'s doc).
     */
    private suspend fun <T> retryingNotPaired(block: suspend () -> T): T {
        repeat(NOT_PAIRED_RETRY_ATTEMPTS - 1) {
            try {
                return block()
            } catch (e: BridgeException) {
                if (e.code != 409) throw e
                delay(pairingRetryDelayMs)
            }
        }
        return block()
    }

    private val _hostInfo = MutableStateFlow(HostInfo())

    /** What the last successful [sessions] fetch said about the host behind
     *  this pairing; a cmux host until the first fetch answers. The same
     *  answer also goes to [onHostInfo], which is how the host registry learns
     *  a host's name and kind without a pairing-time wire change. */
    val hostInfo: StateFlow<HostInfo> = _hostInfo.asStateFlow()

    suspend fun sessions(): WorkspacesResponse = retryingNotPaired {
        call { it.sessions() }.also { publishHostInfo(it.host) }
    }

    private fun publishHostInfo(info: HostInfo) {
        if (_hostInfo.value == info) return
        _hostInfo.value = info
        onHostInfo(info)
    }
    suspend fun pendingFeed(): List<PendingFeedItem> = retryingNotPaired { call { it.pendingFeed() } }
    suspend fun replyFeed(feedId: String, reply: FeedReply) = call { it.replyFeed(feedId, reply) }
    suspend fun renameWorkspace(id: String, title: String) = call { it.renameWorkspace(id, title) }
    suspend fun setYoloMode(id: String, mode: String) = call { it.setYoloMode(id, mode) }
    suspend fun createWorkspace(cwd: String, title: String?) = call { it.createWorkspace(cwd, title) }
    suspend fun createPane(workspaceId: String, surfaceId: String, placement: String) =
        call { it.createPane(workspaceId, surfaceId, placement) }
    suspend fun layout(workspaceId: String) = call { it.layout(workspaceId) }
    suspend fun selectWorkspace(workspaceId: String, surfaceId: String? = null) =
        call { it.selectWorkspace(workspaceId, surfaceId) }
    suspend fun closeWorkspace(workspaceId: String) = call { it.closeWorkspace(workspaceId) }
    suspend fun closeSurface(workspaceId: String, surfaceId: String) = call { it.closeSurface(workspaceId, surfaceId) }

    /**
     * Registers on EVERY configured slot, not just whichever answers first.
     *
     * The relay and the agent keep separate device tables, and neither can
     * fill the other's in: the relay answers /devices/register itself
     * (bridge/internal/relay/relay.go) and the agent serves it only on its
     * direct route set, never on the relay-tunneled one (see the note in
     * bridge/internal/server/trusted.go). Registering just the first
     * reachable slot therefore left the direct slot with no token at all
     * while the relay was up -- the relay answers 200 whenever its own
     * process is alive, even with its tunnel to the agent dead, so [call]
     * never had cause to fail over -- and direct-mode push was silently
     * dead exactly when a relay outage made it the only path left
     * (cmux-app-vex).
     *
     * Deliberately not routed through [call]: that helper stops at the first
     * success, and would read this endpoint's 200 as evidence the relay is
     * healthy when it evidences only that the relay process is running.
     *
     * Best effort per slot -- one slot failing must not deny the other its
     * token -- so this throws only when no slot accepted the token.
     *
     * Every slot's answer is handed to [onRegistrationOutcome] as it resolves.
     * That is this call's second job, and the reason the outcomes are reported
     * through a callback rather than returned: a slot's 401 here is the only
     * routine evidence the app gets that its credential on that server no
     * longer exists (cmux-app-hr1), and it must survive the all-slots-failed
     * throw below -- which is precisely the lockout the evidence explains.
     */
    suspend fun registerDevice(fcmToken: String) {
        val targets = listOfNotNull(
            primary()?.let { ConnectionSlot.RELAY to it },
            fallback()?.let { ConnectionSlot.DIRECT to it },
        )
        if (targets.isEmpty()) throw BridgeException(0, "not configured")
        var registered = false
        var lastFailure: IOException? = null
        for ((slot, target) in targets) {
            val outcome = try {
                target.registerDevice(fcmToken)
                registered = true
                RegistrationOutcome.ACCEPTED
            } catch (e: IOException) {
                lastFailure = e
                e.registrationOutcome()
            }
            onRegistrationOutcome(slot, outcome)
        }
        if (!registered) throw lastFailure ?: BridgeException(0, "not configured")
    }

    /** Same fallback philosophy as [registerDevice]: whichever slot is
     *  actually reachable handles the test push, since either one may be
     *  the connection real pushes later arrive through. */
    suspend fun version(): String = retryingNotPaired { call { it.version() } }
    suspend fun sendTestPush() = call { it.sendTestPush() }

    private companion object {
        const val NOT_PAIRED_RETRY_ATTEMPTS = 3
        const val NOT_PAIRED_RETRY_DELAY_MS = 500L
    }
}

/**
 * Raised when neither transport could serve a call. Both causes are kept and
 * both appear in [message], because they usually fail for unrelated reasons --
 * the relay being unreachable (or its tunnel to the Mac being down) says
 * nothing about whether Tailscale is up on this phone, and collapsing them
 * into one "connection failed" hides which half to go and fix. ViewModels
 * surface [message] verbatim, so no call site needs to know this type exists.
 *
 * [relayError] is null when the relay was inside [RelayHealth]'s penalty
 * window and so was never attempted for this call.
 */
class BothTransportsFailedException(
    val relayError: IOException?,
    val directError: IOException,
) : IOException(
    "Relay: ${relayError?.describeForUser() ?: "skipped (recently unreachable)"} • " +
        "Direct: ${directError.describeForUser()}",
)

internal fun IOException.describeForUser(): String =
    (message ?: this::class.simpleName ?: "unreachable").take(MAX_CAUSE_CHARS)

private const val MAX_CAUSE_CHARS = 120

/**
 * What one server said when this device offered its bearer token to
 * [FallbackBridgeClient.registerDevice].
 *
 * The three are not interchangeable and must never be collapsed. [UNREACHABLE]
 * in particular says nothing at all about the credential: a relay outage read
 * as "the relay credential is gone" would send the user off to re-pair a slot
 * that is perfectly fine. It is the client-side twin of the rule the agent's
 * shared-secret reaper already follows when it abandons a whole round rather
 * than mistake an unanswered server for an empty one.
 */
enum class RegistrationOutcome {
    /** The credential is live on that server. */
    ACCEPTED,

    /** 401: that server has no device row for this token any more -- it was
     *  revoked, or its store was rebuilt. */
    REJECTED,

    /** The server could not be asked (transport error, or a 5xx). */
    UNREACHABLE,
}

/** Deliberately only 401, never 403: neither server issues a 403 on these
 *  routes today, so treating a hypothetical future one as "your credential
 *  was destroyed" would be a guess dressed up as a diagnosis. */
private fun IOException.registrationOutcome(): RegistrationOutcome =
    if (this is BridgeException && code == HTTP_UNAUTHORIZED) {
        RegistrationOutcome.REJECTED
    } else {
        RegistrationOutcome.UNREACHABLE
    }

private const val HTTP_UNAUTHORIZED = 401
