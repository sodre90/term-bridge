package com.sodre90.cmuxremote.data

import kotlinx.serialization.Serializable
import java.security.MessageDigest

/**
 * Identifies one paired agent across both of its [ConnectionSlot]s: the hex
 * of the first 16 bytes of SHA-256 over the agent's X25519 identity key. The
 * agent uses one identity for relay and direct pairing alike
 * (bridge/cmd/term-bridge/pair.go loads a single identity_key), so the two
 * slots of one machine land under one id, and two machines never collide.
 *
 * Derived from the agent key alone -- unlike the SAS fingerprint, which mixes
 * in the phone's per-pairing key and so changes on every re-pair.
 */
@JvmInline
@Serializable
value class HostId(val value: String) {
    companion object {
        /** The stand-in a process with no paired host runs against, so every
         *  slot-keyed store still has a prefix to hang unpaired state on. */
        val NONE = HostId("none")
    }
}

private const val HOST_ID_BYTES = 16

fun hostIdOf(agentPublicKey: ByteArray): HostId {
    val digest = MessageDigest.getInstance("SHA-256").digest(agentPublicKey)
    return HostId(digest.take(HOST_ID_BYTES).joinToString("") { "%02x".format(it) })
}

/** One paired machine as the phone knows it. [name] and [kind] start as
 *  placeholders at pairing and are replaced by what the host reports about
 *  itself on the first successful GET /sessions (see [HostRegistry.describe]). */
@Serializable
data class PairedHost(
    val id: HostId,
    val name: String,
    val kind: String,
)
