package com.sodre90.cmuxremote.data

import android.content.SharedPreferences
import com.sodre90.cmuxremote.data.e2e.CryptoSession
import com.sodre90.cmuxremote.model.HostKind
import java.net.URI
import java.util.Base64

/**
 * One-time move of every pre-multi-host record onto `<host>_<slot>_` keys, run
 * by [AppContainer] over both encrypted prefs files before any [Settings] or
 * [CryptoSession] is built on them -- CryptoSession loads its counters in its
 * constructor, so nothing may rewrite them underneath one.
 *
 * Two earlier layouts exist on upgrading phones, and both are folded here:
 * the original single pairing (unprefixed keys, routed by [inferLegacySlot])
 * and the dual-pairing one (`relay_`/`direct_` prefixes). A slot's host is the
 * one whose identity key its e2e record stores; a slot with credentials but no
 * e2e record cannot be attributed to any host and is dropped -- the phone was
 * about to fail every request on it anyway, and re-pairing is the fix either
 * way.
 *
 * Returns the hosts the records now belong to, RELAY-attributed first, so the
 * caller can register them (empty on a fresh install or a later launch).
 * Self-terminating: every legacy key is removed the first time it is seen.
 */
internal fun migrateToHostKeyed(secure: SharedPreferences, e2e: SharedPreferences): List<PairedHost> {
    foldSinglePairingIntoSlots(secure, e2e)
    if (ConnectionSlot.entries.none { hasSlotPrefixedRecord(secure, e2e, it) }) return emptyList()
    val hosts = mutableListOf<PairedHost>()
    val secureEdit = secure.edit()
    val e2eEdit = e2e.edit()
    val fcmOwner = secure.getString(Settings.KEY_FCM_SOURCE, null)
    for (slot in ConnectionSlot.entries) {
        val slotKey = { base: String -> "${slot.name.lowercase()}_$base" }
        val agentKeyB64 = e2e.getString(slotKey(CryptoSession.KEY_PEER_PUBLIC_KEY), null)
        val baseUrl = secure.getString(slotKey(Settings.KEY_BASE_URL), null)
        if (agentKeyB64 != null) {
            val host = hostIdOf(Base64.getDecoder().decode(agentKeyB64))
            val hostKey = { base: String -> hostSlotKey(host, slot, base) }
            E2E_STRING_FIELDS.forEach { e2eEdit.moveString(e2e, slotKey(it), hostKey(it)) }
            E2E_LONG_FIELDS.forEach { e2eEdit.moveLong(e2e, slotKey(it), hostKey(it)) }
            secureEdit.moveString(secure, slotKey(Settings.KEY_BASE_URL), hostKey(Settings.KEY_BASE_URL))
            secureEdit.moveString(secure, slotKey(Settings.KEY_TOKEN), hostKey(Settings.KEY_TOKEN))
            secureEdit.moveBoolean(
                secure,
                slotKey(Settings.KEY_REJECTION_REPORTED),
                hostKey(Settings.KEY_REJECTION_REPORTED),
            )
            if (hosts.none { it.id == host }) hosts += PairedHost(host, placeholderHostName(baseUrl), HostKind.CMUX)
            if (fcmOwner == slot.name) secureEdit.putString(Settings.KEY_FCM_SOURCE, fcmConfigOwner(host, slot))
        } else {
            secureEdit.remove(slotKey(Settings.KEY_BASE_URL))
                .remove(slotKey(Settings.KEY_TOKEN))
                .remove(slotKey(Settings.KEY_REJECTION_REPORTED))
            if (fcmOwner == slot.name) secureEdit.remove(Settings.KEY_FCM_SOURCE)
        }
    }
    e2eEdit.commit()
    secureEdit.commit()
    return hosts
}

/** The very first layout held exactly one pairing under unprefixed keys; this
 *  rewrites it as the dual-pairing layout so the host pass above sees one shape. */
private fun foldSinglePairingIntoSlots(secure: SharedPreferences, e2e: SharedPreferences) {
    val unprefixed = listOf(Settings.KEY_BASE_URL, Settings.KEY_TOKEN).any(secure::contains) ||
        (E2E_STRING_FIELDS + E2E_LONG_FIELDS).any(e2e::contains)
    if (!unprefixed) return
    val baseUrl = secure.getString(Settings.KEY_BASE_URL, null)?.takeIf { it.isNotBlank() }
    val token = secure.getString(Settings.KEY_TOKEN, null)?.takeIf { it.isNotBlank() }
    val slot = baseUrl?.let(::inferLegacySlot) ?: ConnectionSlot.RELAY
    val slotKey = { base: String -> "${slot.name.lowercase()}_$base" }
    val secureEdit = secure.edit().remove(Settings.KEY_BASE_URL).remove(Settings.KEY_TOKEN)
    if (baseUrl != null && token != null) {
        secureEdit.putString(slotKey(Settings.KEY_BASE_URL), baseUrl).putString(slotKey(Settings.KEY_TOKEN), token)
    }
    secureEdit.commit()
    val e2eEdit = e2e.edit()
    if (e2e.contains(CryptoSession.KEY_SHARED_SECRET)) {
        E2E_STRING_FIELDS.forEach { e2eEdit.moveString(e2e, it, slotKey(it)) }
        E2E_LONG_FIELDS.forEach { e2eEdit.moveLong(e2e, it, slotKey(it)) }
    } else {
        (E2E_STRING_FIELDS + E2E_LONG_FIELDS).forEach { e2eEdit.remove(it) }
    }
    e2eEdit.commit()
}

private fun hasSlotPrefixedRecord(secure: SharedPreferences, e2e: SharedPreferences, slot: ConnectionSlot): Boolean {
    val prefix = "${slot.name.lowercase()}_"
    return secure.contains(prefix + Settings.KEY_BASE_URL) || secure.contains(prefix + Settings.KEY_TOKEN) ||
        (E2E_STRING_FIELDS + E2E_LONG_FIELDS).any { e2e.contains(prefix + it) }
}

private val E2E_STRING_FIELDS = listOf(CryptoSession.KEY_PEER_PUBLIC_KEY, CryptoSession.KEY_SHARED_SECRET)
private val E2E_LONG_FIELDS =
    listOf(CryptoSession.KEY_SEND_COUNTER, CryptoSession.KEY_RECV_HIGHEST, CryptoSession.KEY_RECV_WINDOW_BITS)

/** What a host is called until its first GET /sessions says otherwise. */
internal fun placeholderHostName(baseUrl: String?): String =
    baseUrl?.let { runCatching { URI(it).host }.getOrNull() } ?: baseUrl ?: "Host"

private fun SharedPreferences.Editor.moveString(from: SharedPreferences, key: String, to: String) {
    from.getString(key, null)?.let { putString(to, it) }
    remove(key)
}

private fun SharedPreferences.Editor.moveLong(from: SharedPreferences, key: String, to: String) {
    if (from.contains(key)) putLong(to, from.getLong(key, 0L))
    remove(key)
}

private fun SharedPreferences.Editor.moveBoolean(from: SharedPreferences, key: String, to: String) {
    if (from.contains(key)) putBoolean(to, from.getBoolean(key, false))
    remove(key)
}
