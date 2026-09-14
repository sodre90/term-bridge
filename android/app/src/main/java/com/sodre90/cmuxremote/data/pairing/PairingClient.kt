package com.sodre90.cmuxremote.data.pairing

import com.sodre90.cmuxremote.data.ConnectionSlot
import com.sodre90.cmuxremote.data.FcmClientConfig
import com.sodre90.cmuxremote.data.HostConnections
import com.sodre90.cmuxremote.data.HostId
import com.sodre90.cmuxremote.data.Settings
import com.sodre90.cmuxremote.data.e2e.deriveSharedSecret
import com.sodre90.cmuxremote.data.e2e.generateX25519KeyPair
import com.sodre90.cmuxremote.data.e2e.pairingFingerprint
import com.sodre90.cmuxremote.data.hostIdOf
import com.sodre90.cmuxremote.model.BridgeJson
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.withContext
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.SerializationException
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import java.io.IOException
import java.util.Base64

/** The pairing code was not found, already redeemed, or expired -- the relay's
 *  RedeemPairingCode doesn't distinguish these (see the Go spec's error
 *  handling section), so neither does this. */
class PairingCodeInvalidException : Exception("pairing_code_invalid")

/** The operator answered no at the agent's fingerprint prompt. Distinct from
 *  [PairingNotAnsweredException] because the two are not interchangeable to a
 *  user: this one means a person decided, and re-pairing means asking them
 *  again. */
class PairingRefusedException : Exception("pairing_refused")

/** Nobody answered the agent's fingerprint prompt before
 *  [PAIRING_CONFIRM_TTL_MILLIS] elapsed -- the absence of a decision rather
 *  than a decision, so the user's next move is to go find out whether the
 *  agent is even running. */
class PairingNotAnsweredException : Exception("pairing_not_answered")

/** Mirrors wire.PairingConfirmTTL (bridge/internal/wire/pairing.go), which is
 *  what the server enforces on its own side -- past it a redeemed pairing
 *  reads refused there too, so polling any longer would only ever confirm a
 *  refusal we already have to assume. */
internal const val PAIRING_CONFIRM_TTL_MILLIS = 5L * 60 * 1000

/** Matches the agent's own poll period for the mirror-image wait. */
internal const val PAIR_STATUS_POLL_MILLIS = 2000L

@Serializable
private data class DevicePairRequest(
    val code: String,
    val name: String,
    @SerialName("device_pubkey") val devicePubkey: String,
)

@Serializable
private data class DevicePairResponse(
    val token: String = "",
    @SerialName("tenant_id") val tenantId: String = "",
    val fcm: FcmClientConfigDto? = null,
)

/** Mirrors wire.FCMClientConfig. Absent whenever the bridge has no push
 *  configured, which is why every field defaults and the block itself is
 *  nullable: a bridge older than this feature simply never sends it. */
@Serializable
internal data class FcmClientConfigDto(
    @SerialName("project_id") val projectId: String = "",
    @SerialName("app_id") val appId: String = "",
    @SerialName("api_key") val apiKey: String = "",
    @SerialName("sender_id") val senderId: String = "",
) {
    /** Firebase rejects partial options, so anything short of the full set is
     *  treated as no config at all -- mirrors wire.FCMClientConfig.Configured. */
    fun isComplete(): Boolean =
        projectId.isNotBlank() && appId.isNotBlank() && apiKey.isNotBlank() && senderId.isNotBlank()

    fun toDomain(): FcmClientConfig =
        FcmClientConfig(projectId = projectId, appId = appId, apiKey = apiKey, senderId = senderId)
}

/** Mirrors wire.PairStatusResp. Defaults to pending so a response that is
 *  missing or malformed keeps the phone waiting rather than reading as an
 *  answer nobody gave. */
@Serializable
private data class PairStatusResponse(
    val state: String = PAIR_STATE_PENDING,
)

private const val PAIR_STATE_PENDING = "pending"
private const val PAIR_STATE_CONFIRMED = "confirmed"
private const val PAIR_STATE_REFUSED = "refused"

@Serializable
private data class PairingCodeInfoResponse(
    @SerialName("agent_pubkey") val agentPubkey: String = "",
    @SerialName("expires_at") val expiresAt: String = "",
    @SerialName("tenant_id") val tenantId: String = "",
)

private val JSON_MEDIA = "application/json; charset=utf-8".toMediaType()

/**
 * The prepare/commit/resolveManualCode surface [PairingViewModel]
 * [com.sodre90.cmuxremote.ui.pairing.PairingViewModel] consumes -- narrow
 * enough to fake in a plain JVM test, unlike [PairingClient] itself (whose
 * constructor needs a real Android-Keystore-backed CryptoSession/Settings). [PairingClient] is the sole real implementation.
 */
interface PairingSession {
    /** Computes the SAS fingerprint for [qr]'s agent key against this
     *  phone's own key -- no network call. */
    fun prepare(qr: PairingQr): String

    /** Redeems the pairing code, then waits for the agent's operator to
     *  accept or refuse the fingerprint before persisting anything.
     *  [onAwaitingOperator] fires once the redemption lands and the wait
     *  begins, which is the only moment the caller can distinguish from the
     *  outside. */
    suspend fun commit(qr: PairingQr, onAwaitingOperator: () -> Unit = {})

    /** Resolves a manually-entered server URL + pairing code (the CLI's
     *  fallback for when scanning isn't possible) into the same [PairingQr]
     *  shape a scanned QR produces. */
    suspend fun resolveManualCode(serverUrl: String, code: String): PairingQr
}

/**
 * The phone's X25519 keypair for a single pairing attempt, minted by [begin]
 * and surrendered by [consume].
 *
 * One keypair per pairing is what keeps each pairing's AEAD key distinct.
 * The agent's identity key is persistent and the HKDF has no per-pairing
 * salt, so a phone key reused across pairings derives a bit-identical key
 * while each new device row restarts its counters at zero -- reusing nonces
 * (cmux-app-1fx, see
 * docs/superpowers/specs/2026-08-10-per-pairing-key-separation-design.md).
 *
 * [consume] is single-use for the same reason: two pairings completed from
 * one keypair would share a key just as surely as a persisted one. It also
 * refuses to invent a keypair when none is pending, because the SAS
 * fingerprint the user confirmed must belong to the key actually submitted
 * -- a key generated at commit time was never shown to anyone.
 */
internal class PairingKeys(
    private val generate: () -> Pair<ByteArray, ByteArray> = ::generateX25519KeyPair,
) {
    private var pending: Pair<ByteArray, ByteArray>? = null

    @Synchronized
    fun begin(): Pair<ByteArray, ByteArray> = generate().also { pending = it }

    @Synchronized
    fun consume(): Pair<ByteArray, ByteArray> =
        (pending ?: throw IOException("pairing failed: confirm the fingerprint again"))
            .also { pending = null }
}

/**
 * Completes self-service pairing against POST /devices/pair: submits the
 * phone's e2e public key alongside the scanned code, and on success derives
 * the shared secret via ECDH and persists everything -- bearer token + base
 * URL into Settings, e2e session into CryptoSession. Mirrors
 * bridge/cmd/term-bridge/pair.go's agent-side half of the same handshake.
 *
 * [prepare]/[commit] are split (rather than one atomic call) so the UI can
 * show a fingerprint confirmation screen between them -- see
 * docs/superpowers/specs/2026-07-10-pairing-mitm-fingerprint-design.md.
 * [prepare] makes no network call, so a rejected fingerprint costs nothing
 * server-side; only [commit] actually redeems the pairing code.
 *
 * [prepare] also mints the keypair this pairing will use (see [PairingKeys]),
 * so re-running it after the user backs out replaces the pending key and the
 * fingerprint on screen always describes the key [commit] will submit. One
 * instance per [slot], so relay and direct never share pending state.
 *
 * Which host a pairing belongs to is only known at [commit], from the agent
 * key the QR carries -- so the per-host state it writes into is resolved
 * there through [bindHost], never fixed at construction. Everything about the
 * pairing being replaced (the credential to retire, the sockets to drop, the
 * rejection to clear) is looked up on that same host, so scanning a second
 * machine's QR into a slot never disturbs the first machine's pairing.
 *
 * [onPairingStored] fires once a pairing is confirmed and stored, with the
 * host it landed on and the base URL it reaches it at.
 */
class PairingClient(
    private val http: OkHttpClient,
    private val slot: ConnectionSlot,
    private val settings: Settings,
    private val bindHost: (HostId) -> HostConnections,
    private val onPairingStored: (host: HostId, baseUrl: String) -> Unit = { _, _ -> },
) : PairingSession {
    private val keys = PairingKeys()

    override fun prepare(qr: PairingQr): String = prepareInternal(qr, keys.begin().second)

    override suspend fun commit(qr: PairingQr, onAwaitingOperator: () -> Unit) {
        val (privateKey, publicKey) = keys.consume()
        val hostId = hostIdOf(Base64.getDecoder().decode(qr.agentPubkey))
        val host = bindHost(hostId)
        // Read before commitInternal writes over it. Every pairing mints a
        // fresh key and a fresh device row (cmux-app-1fx), so without this
        // the row this one replaces would stay authenticating forever
        // against a secret the agent has already dropped (cmux-app-bys).
        val superseded = host.bridgeConfig(slot)
        commitInternal(
            http = http,
            qr = qr,
            phonePrivateKey = privateKey,
            phonePublicKey = publicKey,
            onAwaitingOperator = onAwaitingOperator,
            onSetPairing = host.session(slot)::setPairing,
            onSetBaseUrl = { settings.setBaseUrl(hostId, slot, it) },
            onSetToken = { settings.setDeviceToken(hostId, slot, it) },
            onSetFcmClientConfig = { settings.setFcmClientConfig(hostId, slot, it) },
            onPairingStored = { onPairingStored(hostId, baseUrlFromPairUrl(qr.pairUrl)) },
            // Sockets still running on the old token map to the agent's
            // previous device row, whose key no longer matches this
            // session, so every frame they carry is dropped (cmux-app-smu).
            onCredentialsReplaced = {
                host.slotCredentials.invalidate(slot)
                // Immediately, not at the next launch probe: registerDevice
                // won't run again until then, so a stale "rejected" would
                // still be demanding a re-pair on the screen the user just
                // re-paired from.
                host.slotCredentialHealth.reset(slot)
                superseded?.let { host.retireCredential(slot, it) }
            },
        )
    }

    override suspend fun resolveManualCode(serverUrl: String, code: String): PairingQr =
        resolvePairingCode(http, serverUrl, code)
}

/** GET /devices/pair-info/{code}: the manual-entry counterpart to scanning a
 *  QR. The QR carries {pair_url, code, agent_pubkey, expires_at, tenant_id}
 *  directly; a phone that can't scan (no camera, or pairing remotely) has
 *  only the server URL and the code the CLI also prints, so it resolves the
 *  rest from the relay instead. Everything downstream of this call
 *  (prepareInternal/commitInternal) is identical to the QR path. */
internal suspend fun resolvePairingCode(http: OkHttpClient, serverUrl: String, code: String): PairingQr =
    withContext(Dispatchers.IO) {
        val base = serverUrl.trimEnd('/')
        val request = Request.Builder().url("$base/devices/pair-info/$code").get().build()
        http.newCall(request).execute().use { response ->
            if (response.code == 410) throw PairingCodeInvalidException()
            if (!response.isSuccessful) throw IOException("pair-info failed: HTTP ${response.code}")
            val body = BridgeJson.decodeFromString(
                PairingCodeInfoResponse.serializer(),
                response.body?.string().orEmpty(),
            )
            PairingQr(
                pairUrl = "$base/devices/pair",
                code = code,
                agentPubkey = body.agentPubkey,
                expiresAt = body.expiresAt,
                tenantId = body.tenantId,
            )
        }
    }

/** Local-only: computes the SAS fingerprint for [qr]'s agent key against
 *  [phonePublicKey] -- no network call, nothing persisted. Validates
 *  [PairingQr.hasSafePairUrl] up front too, so a malformed manual-entry URL
 *  fails before the user ever sees a confirmation screen, not just later at
 *  [commitInternal]. Free function so PairingClientTest can exercise it
 *  without constructing a real CryptoSession. */
internal fun prepareInternal(qr: PairingQr, phonePublicKey: ByteArray): String {
    if (!qr.hasSafePairUrl()) throw IOException("pairing failed: server address must be https://")
    val agentPublicKey = Base64.getDecoder().decode(qr.agentPubkey)
    return pairingFingerprint(phonePublicKey, agentPublicKey)
}

/** What a completed redemption yields, held in memory until the operator
 *  accepts it. Nothing here is persisted until then: the phone used to
 *  declare the slot paired at redemption, which happens before anyone has
 *  agreed to the pairing (cmux-app-gmo). */
private class RedeemedPairing(
    val token: String,
    val agentPublicKey: ByteArray,
    val sharedSecret: ByteArray,
    val fcm: FcmClientConfigDto?,
)

/** Free function (not a CryptoSession/Settings method) so PairingClientTest can
 *  exercise the real handshake logic via plain callbacks -- see Task 11's
 *  Step 1 note on why CryptoSession/Settings can't be constructed in a
 *  local JVM unit test. */
internal suspend fun commitInternal(
    http: OkHttpClient,
    qr: PairingQr,
    phonePrivateKey: ByteArray,
    phonePublicKey: ByteArray,
    onSetPairing: (peerPublicKey: ByteArray, sharedSecret: ByteArray) -> Unit,
    onSetBaseUrl: (String) -> Unit,
    onSetToken: (String) -> Unit,
    onSetFcmClientConfig: (FcmClientConfig?) -> Unit,
    onCredentialsReplaced: () -> Unit,
    onPairingStored: () -> Unit = {},
    onAwaitingOperator: () -> Unit = {},
    pollPeriodMillis: Long = PAIR_STATUS_POLL_MILLIS,
    confirmTimeoutMillis: Long = PAIRING_CONFIRM_TTL_MILLIS,
): Unit = withContext(Dispatchers.IO) {
    // The manual-entry path (resolvePairingCode) builds its own PairingQr
    // from a locally-constructed URL and never goes through
    // parsePairingQr's validation, so this is the one choke point both the
    // QR-scan and manual-entry flows share before the pairing POST.
    if (!qr.hasSafePairUrl()) throw IOException("pairing failed: server address must be https://")
    val payload = DevicePairRequest(
        code = qr.code,
        name = "phone",
        devicePubkey = Base64.getEncoder().encodeToString(phonePublicKey),
    )
    val request = Request.Builder()
        .url(qr.pairUrl)
        .post(BridgeJson.encodeToString(DevicePairRequest.serializer(), payload).toRequestBody(JSON_MEDIA))
        .build()

    val redeemed = http.newCall(request).execute().use { response ->
        if (response.code == 410) throw PairingCodeInvalidException()
        if (!response.isSuccessful) throw IOException("pairing failed: HTTP ${response.code}")
        val body = BridgeJson.decodeFromString(
            DevicePairResponse.serializer(),
            response.body?.string().orEmpty(),
        )
        val agentPublicKey = Base64.getDecoder().decode(qr.agentPubkey)
        RedeemedPairing(
            token = body.token,
            agentPublicKey = agentPublicKey,
            sharedSecret = deriveSharedSecret(phonePrivateKey, agentPublicKey),
            fcm = body.fcm?.takeIf { it.isComplete() },
        )
    }

    val baseUrl = baseUrlFromPairUrl(qr.pairUrl)
    onAwaitingOperator()
    awaitOperatorConfirmation(http, baseUrl, qr.code, pollPeriodMillis, confirmTimeoutMillis)

    onSetPairing(redeemed.agentPublicKey, redeemed.sharedSecret)
    onSetBaseUrl(baseUrl)
    onSetToken(redeemed.token)
    // Only ever on a confirmed pairing, like the token: a refused one must
    // leave no trace, and a null here clears any config an earlier pairing
    // left behind rather than stranding the app on a stale project.
    onSetFcmClientConfig(redeemed.fcm?.toDomain())
    // Last, and only on a pairing the operator accepted: sockets woken by
    // this have to find the new credentials already stored, and it tears down
    // a live connection on the slot -- which a refused pairing has no
    // business doing.
    onCredentialsReplaced()
    // Last of all, and only on a confirmed pairing: push is brought up here so
    // a phone that just received its Firebase config does not sit dead until
    // the next cold start. Ordered after onCredentialsReplaced so the token
    // registration it triggers finds the new credentials already live.
    onPairingStored()
}

/** Blocks until the agent's operator answers, mirroring the agent's own poll
 *  loop on the other side of the same prompt. A transport error is retried
 *  rather than read as a refusal -- so is a 404, which is what an old relay
 *  without this route answers -- because only an explicit "refused" is an
 *  answer. The deadline is what ends the wait either way. */
private suspend fun awaitOperatorConfirmation(
    http: OkHttpClient,
    baseUrl: String,
    code: String,
    pollPeriodMillis: Long,
    timeoutMillis: Long,
) {
    val deadline = System.nanoTime() + timeoutMillis * 1_000_000
    while (true) {
        when (fetchPairStatus(http, baseUrl, code)) {
            PAIR_STATE_CONFIRMED -> return
            PAIR_STATE_REFUSED -> throw PairingRefusedException()
        }
        if (System.nanoTime() >= deadline) throw PairingNotAnsweredException()
        delay(pollPeriodMillis)
    }
}

/** Returns null for anything that isn't a clean answer, which the caller
 *  retries: this route carries no credential and no key material, so there is
 *  nothing to distinguish among its failure modes -- only the explicit
 *  states mean anything. */
private fun fetchPairStatus(http: OkHttpClient, baseUrl: String, code: String): String? = try {
    val request = Request.Builder().url("$baseUrl/devices/pair-status/$code").get().build()
    http.newCall(request).execute().use { response ->
        if (!response.isSuccessful) {
            null
        } else {
            BridgeJson.decodeFromString(
                PairStatusResponse.serializer(),
                response.body?.string().orEmpty(),
            ).state
        }
    }
} catch (e: IOException) {
    null
} catch (e: SerializationException) {
    null
}

/** "https://host/devices/pair" -> "https://host" -- the same main vhost the
 *  bridge's other endpoints live on (bridge/cmd/term-bridge/pair.go derives
 *  the QR's pair_url this same way, in reverse). */
private fun baseUrlFromPairUrl(pairUrl: String): String = pairUrl.removeSuffix("/devices/pair")
