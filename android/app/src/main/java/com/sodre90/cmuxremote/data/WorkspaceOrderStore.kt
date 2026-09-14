package com.sodre90.cmuxremote.data

import android.content.Context

/**
 * Persists the phone-local display preferences for the sessions list -- the
 * custom drag order (per host -- workspace ids mean nothing across machines)
 * and the "Waiting first" sort toggle (one answer for the whole app). Both are
 * display preferences only: not synced to the bridge, not visible from any
 * other device, and unrelated to cmux's own sidebar order on the Mac.
 * Workspace ids that later disappear from the saved order are simply dropped
 * by [SessionsLogic.applyCustomOrder]; nothing here needs to track that.
 */
class WorkspaceOrderStore(context: Context) {
    private val prefs = context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)

    /** Workspace ids in their last-saved custom order, oldest-set-first. */
    fun load(host: HostId): List<String> =
        prefs.getString(orderKey(host), null)?.split(SEPARATOR)?.filter { it.isNotEmpty() } ?: emptyList()

    fun save(host: HostId, order: List<String>) {
        prefs.edit().putString(orderKey(host), order.joinToString(SEPARATOR)).apply()
    }

    /** Hands the pre-multi-host order to [host], the one machine it could have
     *  belonged to. Self-terminating like the credential migration it follows. */
    fun adoptLegacyOrder(host: HostId) {
        val legacy = prefs.getString(KEY_ORDER, null) ?: return
        prefs.edit().putString(orderKey(host), legacy).remove(KEY_ORDER).apply()
    }

    private fun orderKey(host: HostId) = "${host.value}_$KEY_ORDER"

    fun loadSortByAttention(): Boolean = prefs.getBoolean(KEY_SORT_BY_ATTENTION, false)

    fun saveSortByAttention(sortByAttention: Boolean) {
        prefs.edit().putBoolean(KEY_SORT_BY_ATTENTION, sortByAttention).apply()
    }

    private companion object {
        const val PREFS_NAME = "cmux_workspace_prefs"
        const val KEY_ORDER = "order"
        const val KEY_SORT_BY_ATTENTION = "sort_by_attention"
        const val SEPARATOR = ","
    }
}
