package com.sodre90.cmuxremote.data

import android.content.SharedPreferences
import com.sodre90.cmuxremote.data.e2e.Cipher
import com.sodre90.cmuxremote.data.e2e.CryptoSession
import com.sodre90.cmuxremote.data.e2e.E2eInterceptor
import com.sodre90.cmuxremote.model.HostInfo
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch
import okhttp3.OkHttpClient
import java.util.concurrent.TimeUnit

/**
 * Everything the app holds for one paired [host]: an e2e session and an
 * [OkHttpClient] per [ConnectionSlot] (bearer token + opt-in e2e encryption),
 * the slot-scoped bridge clients/sockets built on them, and the per-host
 * health/credential state those share. [AppContainer] keeps one of these per
 * paired host and points the ViewModel-facing gateways at the selected one.
 *
 * Nothing in here is shared between hosts on purpose: a relay penalty, a
 * credential rejection or a forgotten slot on one machine says nothing about
 * another.
 */
class HostConnections(
    val host: HostId,
    private val settings: Settings,
    e2ePrefs: SharedPreferences,
    private val cipher: Cipher,
    private val terminalPollMs: () -> Int,
    onNewRejection: (ConnectionSlot) -> Unit,
    onHostInfo: (HostInfo) -> Unit,
) {
    private val sessions: Map<ConnectionSlot, CryptoSession> =
        ConnectionSlot.entries.associateWith { CryptoSession(e2ePrefs, host, it) }

    private val clients = mutableMapOf<ConnectionSlot, Pair<String, OkHttpClient>>()

    @Synchronized
    private fun httpClient(slot: ConnectionSlot, cfg: BridgeConfig): OkHttpClient {
        val session = sessions.getValue(slot)
        val key = "${cfg.baseUrl}|${cfg.deviceToken}|${session.isPaired()}"
        clients[slot]?.let { (cachedKey, cachedClient) -> if (cachedKey == key) return cachedClient }

        var builder = Mtls.client(cfg).newBuilder()
        if (slot == ConnectionSlot.RELAY) {
            // Only the connect phase needs a short leash: an unreachable
            // relay fails the TCP handshake almost immediately, so 3s is
            // generous for a real failure while still keeping the UI from
            // stalling on a dead home server. A slow-but-reachable response
            // (cmux itself being slow) is a different problem and must not
            // trip a spurious failover, so read/write timeouts stay at
            // OkHttp's defaults. Direct has no second fallback to race
            // against, so it keeps the normal (longer) connect timeout too.
            builder = builder.connectTimeout(3, TimeUnit.SECONDS)
        }
        var built = builder.build()
        if (session.isPaired()) {
            built = built.newBuilder()
                .addInterceptor(E2eInterceptor(session, cipher, slot == ConnectionSlot.RELAY))
                .build()
        }
        clients[slot] = key to built
        return built
    }

    fun bridgeConfig(slot: ConnectionSlot): BridgeConfig? = settings.bridgeConfig(host, slot)

    fun isConfigured(): Boolean = ConnectionSlot.entries.any { bridgeConfig(it) != null }

    /** The paired session for [slot] -- used by CmuxMessagingService to decrypt
     *  an incoming push, which arrives tagged with the slot that sent it rather
     *  than going through the usual bridgeClient()/eventsSocket() request path. */
    fun session(slot: ConnectionSlot): CryptoSession = sessions.getValue(slot)

    /** Clears [slot]'s stored bridge config and e2e session, and evicts its
     *  cached [OkHttpClient] -- used by "Forget" in ConnectionSettingsScreen.
     *  The other slot is untouched. Re-pairing this slot afterwards behaves
     *  exactly like pairing it for the first time. Also asks the server to
     *  retire this device's token, best-effort; see
     *  [releaseCredentialOnServer]. */
    @Synchronized
    fun forgetSlot(slot: ConnectionSlot) {
        releaseCredentialOnServer(slot)
        settings.clearSlot(host, slot)
        sessions.getValue(slot).clear()
        clients.remove(slot)
        // Storage alone doesn't reach a socket that is already connected on
        // the credentials just cleared -- see [SlotCredentials].
        slotCredentials.invalidate(slot)
        // Forgotten by intent is not the same fact as rejected by a server,
        // and the Connections screen must not read the two the same way.
        slotCredentialHealth.reset(slot)
    }

    // Fire-and-forget on purpose, and the local clear above never waits on
    // it: Forget has to work with the server unreachable, and a phone stuck
    // paired to a bridge it can't dial would be the worse failure. What the
    // server misses here an operator can still revoke by hand, and the agent
    // reaps the orphaned shared secret on its own timer either way. The
    // client is captured before settings.clearSlot because it holds this
    // slot's bearer token by value (see Mtls.BearerInterceptor).
    private fun releaseCredentialOnServer(slot: ConnectionSlot) {
        bridgeConfig(slot)?.let { retireCredential(slot, it) }
    }

    /** Takes the config rather than reading it, because the re-pair path calls
     *  this once the NEW credentials are already stored -- it has to name the
     *  ones being replaced. The client is built here, not inside the
     *  coroutine, for the same reason. */
    fun retireCredential(slot: ConnectionSlot, cfg: BridgeConfig) {
        val client = BridgeClient(httpClient(slot, cfg), cfg.baseUrl)
        selfRevokeScope.launch { runCatching { client.selfRevoke() } }
    }

    private val selfRevokeScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    fun bridgeClient(slot: ConnectionSlot): BridgeClient? =
        bridgeConfig(slot)?.let { BridgeClient(httpClient(slot, it), it.baseUrl) }

    fun eventsSocket(slot: ConnectionSlot): EventsSocket? =
        bridgeConfig(slot)?.let { EventsSocket(httpClient(slot, it), it.baseUrl, sessions.getValue(slot), cipher) }

    fun terminalSocket(slot: ConnectionSlot, surfaceId: String): TerminalSocket? =
        bridgeConfig(slot)?.let {
            TerminalSocket(
                httpClient(slot, it),
                it.baseUrl,
                surfaceId,
                sessions.getValue(slot),
                cipher,
                // Resolved per socket, not once at startup: the reconnect that
                // follows a Wi-Fi/mobile switch picks up the other interval.
                terminalPollMs(),
            )
        }

    // Shared with the bridge below and handed out via BridgeGateway.relayHealth
    // so every reconnecting socket subscription and the REST fallback path
    // learn "relay is down" once, from the same instance.
    val relayHealth = RelayHealth()

    // Shared for the same reason as relayHealth: the REST path and every
    // socket subscription are all reporting on the same two transports.
    val connectionMonitor = ConnectionMonitor()

    // Shared for the same reason again: forgetting or re-pairing a slot has
    // to reach every socket subscription running on it, wherever it was
    // started from.
    val slotCredentials = SlotCredentials()

    // Shared for the same reason again, and written from exactly one place:
    // the registerDevice fan-out, which is the only call that asks both slots
    // about this device on every launch. Settings supplies the one bit that
    // must survive process death -- see RejectionReportLog.
    val slotCredentialHealth = SlotCredentialHealth(
        reportLog = settings.rejectionReportLog(host),
        onNewRejection = onNewRejection,
    )

    /** A single shared instance (not rebuilt per call) so the 30s "primary is
     *  down" penalty window actually persists across repeated calls. */
    val bridge = FallbackBridgeClient(
        primary = { bridgeClient(ConnectionSlot.RELAY) },
        fallback = { bridgeClient(ConnectionSlot.DIRECT) },
        relayHealth = relayHealth,
        monitor = connectionMonitor,
        onRegistrationOutcome = slotCredentialHealth::record,
        onCredentialRejected = slotCredentialHealth::recordRejection,
        onHostInfo = onHostInfo,
    )
}
