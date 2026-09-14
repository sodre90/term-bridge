package com.sodre90.cmuxremote.data

import android.content.Context
import android.content.SharedPreferences
import com.sodre90.cmuxremote.model.BridgeJson
import com.sodre90.cmuxremote.push.PendingTokenStore
import kotlinx.serialization.builtins.ListSerializer

/**
 * Persists connection settings and secrets for every paired host's
 * [ConnectionSlot]s, keyed by ([HostId], slot). Everything (including base URLs
 * and tokens) lives in [EncryptedSharedPreferences] so device certificates and
 * bearer tokens are encrypted at rest; nothing here is ever logged.
 *
 * Also the durable half of [HostRegistry] and of each host's
 * [SlotCredentialHealth]: the rest of that state is in-memory and starts over
 * each process, but whether the user has already been told about a rejection
 * has to outlive one (see [RejectionReportLog]).
 */
class Settings internal constructor(
    private val prefs: SharedPreferences,
) : PendingTokenStore, HostRegistryStore {

    constructor(context: Context) : this(encryptedPrefs(context, PREFS_NAME))

    init {
        // An upgrading install may still have the pre-pairing manual-setup
        // format's client-cert key on disk. Wipe the whole prefs file once
        // and force re-pairing. Self-terminating: nothing writes this key
        // again once cleared, so this branch never fires on later launches.
        if (prefs.contains(KEY_P12)) {
            prefs.edit().clear().apply()
        }
    }

    fun baseUrl(host: HostId, slot: ConnectionSlot): String? = prefs.getString(key(host, slot, KEY_BASE_URL), null)
    fun setBaseUrl(host: HostId, slot: ConnectionSlot, value: String) {
        prefs.edit().putString(key(host, slot, KEY_BASE_URL), value).apply()
    }

    fun deviceToken(host: HostId, slot: ConnectionSlot): String? = prefs.getString(key(host, slot, KEY_TOKEN), null)
    fun setDeviceToken(host: HostId, slot: ConnectionSlot, value: String) {
        prefs.edit().putString(key(host, slot, KEY_TOKEN), value).apply()
    }

    /**
     * An FCM token no host has accepted yet, kept so the retry can outlive the
     * process that failed.
     *
     * FCM hands the app a rotated token exactly once, through onNewToken. If the
     * bridge happens to be unreachable at that moment the token used to be
     * dropped on the floor and retried only if the user next opened the app --
     * so an update installed overnight left the server holding a dead token,
     * which FCM accepts with a 2xx while delivering nothing (cmux-app-2cm).
     *
     * Not cleared on [clearSlot]: the token belongs to the app on this device,
     * not to any host, and a re-pair should still find it waiting.
     */
    override fun pendingFcmToken(): String? = prefs.getString(KEY_PENDING_FCM_TOKEN, null)

    override fun setPendingFcmToken(token: String?) {
        prefs.edit().apply {
            if (token == null) remove(KEY_PENDING_FCM_TOKEN) else putString(KEY_PENDING_FCM_TOKEN, token)
        }.apply()
    }

    /**
     * The Firebase client config a bridge handed over at pairing, or null if
     * no pairing has supplied one. Not host- or slot-scoped -- see
     * [FcmClientConfig].
     *
     * Read on every launch before Firebase is touched, so a phone that has
     * never paired against a push-configured bridge simply never initialises
     * Firebase at all.
     */
    fun fcmClientConfig(): FcmClientConfig? {
        val projectId = prefs.getString(KEY_FCM_PROJECT_ID, null).orEmpty()
        val appId = prefs.getString(KEY_FCM_APP_ID, null).orEmpty()
        val apiKey = prefs.getString(KEY_FCM_API_KEY, null).orEmpty()
        val senderId = prefs.getString(KEY_FCM_SENDER_ID, null).orEmpty()
        if (projectId.isBlank() || appId.isBlank() || apiKey.isBlank() || senderId.isBlank()) return null
        return FcmClientConfig(projectId, appId, apiKey, senderId)
    }

    /**
     * Stores the config ([host], [slot])'s bridge supplied, or clears the
     * stored one when that bridge supplied none.
     *
     * The clear is deliberately narrow. Only the pairing that supplied the
     * stored config may clear it: slots and hosts are configured
     * independently, and direct push is documented as optional, so pairing a
     * push-less direct agent after a push-enabled relay would otherwise delete
     * a working config and kill push everywhere. Same reasoning as the FCM
     * token two blocks up -- what belongs to the app on this device does not
     * get thrown away by whichever pairing happened to land last.
     */
    fun setFcmClientConfig(host: HostId, slot: ConnectionSlot, config: FcmClientConfig?) {
        val owner = prefs.getString(KEY_FCM_SOURCE, null)
        val claimant = fcmConfigOwner(host, slot)
        if (config == null && !mayClearFcmConfig(owner, claimant)) return
        prefs.edit().apply {
            if (config == null) {
                remove(KEY_FCM_PROJECT_ID)
                remove(KEY_FCM_APP_ID)
                remove(KEY_FCM_API_KEY)
                remove(KEY_FCM_SENDER_ID)
                remove(KEY_FCM_SOURCE)
            } else {
                putString(KEY_FCM_PROJECT_ID, config.projectId)
                putString(KEY_FCM_APP_ID, config.appId)
                putString(KEY_FCM_API_KEY, config.apiKey)
                putString(KEY_FCM_SENDER_ID, config.senderId)
                putString(KEY_FCM_SOURCE, claimant)
            }
        }.apply()
    }

    /** [host]'s view of the rejection-report log, for its [SlotCredentialHealth]. */
    fun rejectionReportLog(host: HostId): RejectionReportLog = object : RejectionReportLog {
        override fun wasRejectionReported(slot: ConnectionSlot): Boolean =
            prefs.getBoolean(key(host, slot, KEY_REJECTION_REPORTED), false)

        override fun setRejectionReported(slot: ConnectionSlot, reported: Boolean) {
            prefs.edit().putBoolean(key(host, slot, KEY_REJECTION_REPORTED), reported).apply()
        }
    }

    /** Wipes ([host], [slot])'s stored base URL and device token -- used by
     *  "Forget" in ConnectionSettingsScreen. Every other slot is untouched. */
    fun clearSlot(host: HostId, slot: ConnectionSlot) {
        prefs.edit()
            .remove(key(host, slot, KEY_BASE_URL))
            .remove(key(host, slot, KEY_TOKEN))
            .remove(key(host, slot, KEY_REJECTION_REPORTED))
            .apply()
    }

    /** Assembles a [BridgeConfig] for ([host], [slot]), or null if that slot
     *  has never been paired. */
    fun bridgeConfig(host: HostId, slot: ConnectionSlot): BridgeConfig? {
        val url = baseUrl(host, slot)?.takeIf { it.isNotBlank() } ?: return null
        val token = deviceToken(host, slot)?.takeIf { it.isNotBlank() } ?: return null
        return BridgeConfig(baseUrl = url, deviceToken = token)
    }

    override fun loadHosts(): List<PairedHost> =
        prefs.getString(KEY_HOSTS, null)
            ?.let { runCatching { BridgeJson.decodeFromString(hostListSerializer, it) }.getOrNull() }
            .orEmpty()

    override fun saveHosts(hosts: List<PairedHost>) {
        prefs.edit().putString(KEY_HOSTS, BridgeJson.encodeToString(hostListSerializer, hosts)).apply()
    }

    private val hostListSerializer = ListSerializer(PairedHost.serializer())

    override fun loadSelectedHost(): HostId? = prefs.getString(KEY_SELECTED_HOST, null)?.let(::HostId)

    override fun saveSelectedHost(id: HostId?) {
        prefs.edit().apply {
            if (id == null) remove(KEY_SELECTED_HOST) else putString(KEY_SELECTED_HOST, id.value)
        }.apply()
    }

    private fun key(host: HostId, slot: ConnectionSlot, base: String) = hostSlotKey(host, slot, base)

    companion object {
        const val PREFS_NAME = "cmux_secure_prefs"
        const val KEY_BASE_URL = "base_url"
        const val KEY_TOKEN = "device_token"
        const val KEY_REJECTION_REPORTED = "credential_rejection_reported"
        private const val KEY_P12 = "client_p12_b64"

        private const val KEY_HOSTS = "hosts"
        private const val KEY_SELECTED_HOST = "selected_host"

        // Not host-scoped: one FCM token per app install, offered to every host.
        private const val KEY_PENDING_FCM_TOKEN = "pending_fcm_token"

        // Not host-scoped either: Firebase initialises once per process, so
        // one config serves every pairing (see FcmClientConfig).
        private const val KEY_FCM_PROJECT_ID = "fcm_project_id"
        private const val KEY_FCM_APP_ID = "fcm_app_id"
        private const val KEY_FCM_API_KEY = "fcm_api_key"
        private const val KEY_FCM_SENDER_ID = "fcm_sender_id"

        /** Which pairing's bridge supplied the stored config, so only that one
         *  can later clear it (see setFcmClientConfig). */
        const val KEY_FCM_SOURCE = "fcm_source_slot"
    }
}

/** The key prefix every per-pairing record in both encrypted prefs files
 *  shares: `<host>_<slot>_<field>`. */
internal fun hostSlotKey(host: HostId, slot: ConnectionSlot, base: String) =
    "${host.value}_${slot.name.lowercase()}_$base"

internal fun fcmConfigOwner(host: HostId, slot: ConnectionSlot) = "${host.value}:${slot.name}"

/**
 * Whether [claimant] is allowed to clear a stored FCM config owned by [owner]
 * (null meaning nobody has claimed one).
 *
 * A free function because the decision is worth testing on the JVM, and
 * [Settings] itself cannot be constructed without Android Keystore.
 */
internal fun mayClearFcmConfig(owner: String?, claimant: String): Boolean =
    owner == null || owner == claimant
