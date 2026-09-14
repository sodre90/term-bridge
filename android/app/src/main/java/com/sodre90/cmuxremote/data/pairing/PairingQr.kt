package com.sodre90.cmuxremote.data.pairing

import com.sodre90.cmuxremote.model.BridgeJson
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import java.net.URI
import java.time.Instant

/** The JSON payload rendered into the pairing QR by `term-bridge pair-device`
 *  (bridge/cmd/term-bridge/pair.go's pairingQR struct). */
@Serializable
data class PairingQr(
    @SerialName("pair_url") val pairUrl: String = "",
    val code: String = "",
    @SerialName("agent_pubkey") val agentPubkey: String = "",
    @SerialName("expires_at") val expiresAt: String = "",
    @SerialName("tenant_id") val tenantId: String = "",
)

/** Returns null for anything that isn't a valid pairing QR -- malformed
 *  JSON, JSON missing the fields this flow actually needs, or a [pairUrl]
 *  that doesn't look like one term-bridge's own pair-device flow would ever
 *  emit (see [hasSafePairUrl]). The camera scanner resumes scanning on null
 *  rather than crashing (a scanned code may simply be unrelated to cmux, or
 *  -- since this is arbitrary attacker-controlled input -- an attempt to
 *  redirect pairing to a malicious host). */
fun parsePairingQr(raw: String): PairingQr? {
    val qr = try {
        BridgeJson.decodeFromString(PairingQr.serializer(), raw)
    } catch (e: Exception) {
        return null
    }
    if (qr.pairUrl.isBlank() || qr.code.isBlank() || qr.agentPubkey.isBlank()) return null
    if (!qr.hasSafePairUrl()) return null
    return qr
}

/**
 * Requires an `https://` scheme with no userinfo (`user:pass@host`) and a
 * non-blank host -- the exact shape bridge/cmd/term-bridge/pair.go always
 * produces for `pair_url`. A scanned QR is untrusted input; without this,
 * a malicious QR could point the pairing POST (and the base URL later
 * derived from it, see PairingClient.baseUrlFromPairUrl) at an
 * attacker-controlled host, over plaintext, and/or smuggle credentials via
 * userinfo.
 */
fun PairingQr.hasSafePairUrl(): Boolean {
    val uri = try {
        URI(pairUrl)
    } catch (e: Exception) {
        return false
    }
    return uri.scheme.equals("https", ignoreCase = true) &&
        uri.userInfo == null &&
        !uri.host.isNullOrBlank()
}

/** Client-side expiry check so a stale/reused QR fails fast with a clear
 *  message instead of waiting on the server's 410. Unparseable timestamps
 *  are treated as not-expired -- the server is the authoritative check. */
fun PairingQr.isExpired(): Boolean = try {
    Instant.parse(expiresAt).isBefore(Instant.now())
} catch (e: Exception) {
    false
}
