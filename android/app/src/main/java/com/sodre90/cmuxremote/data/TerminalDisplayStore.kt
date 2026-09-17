package com.sodre90.cmuxremote.data

import android.content.Context

/**
 * Persists the phone-local terminal display preferences -- the pinch-zoom
 * multiplier over the fit-to-width baseline (see
 * [com.sodre90.cmuxremote.ui.terminal.TerminalScreen]'s userZoom) and the
 * output poll cadence. Not
 * synced to the bridge, not visible from any other device, and shared by
 * every terminal surface: it's a "how big do you like your text" setting,
 * not a per-session one.
 */
class TerminalDisplayStore(context: Context) {
    private val prefs = context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)

    fun loadFontZoom(): Float = prefs.getFloat(KEY_FONT_ZOOM, DEFAULT_FONT_ZOOM)

    fun saveFontZoom(zoom: Float) {
        prefs.edit().putFloat(KEY_FONT_ZOOM, zoom).apply()
    }

    fun loadTerminalPollMs(metered: Boolean): Int =
        nearestPollChoice(
            prefs.getInt(
                pollKey(metered),
                inheritedPollDefault(metered, prefs.getInt(KEY_POLL_MS_BOTH_LINKS, 0)),
            ),
        )

    fun saveTerminalPollMs(metered: Boolean, ms: Int) {
        prefs.edit().putInt(pollKey(metered), ms).apply()
    }

    private fun pollKey(metered: Boolean) =
        if (metered) KEY_POLL_MS_METERED else KEY_POLL_MS_UNMETERED

    private companion object {
        const val PREFS_NAME = "cmux_terminal_display_prefs"
        const val KEY_FONT_ZOOM = "font_zoom"
        const val DEFAULT_FONT_ZOOM = 1f
        const val KEY_POLL_MS_UNMETERED = "terminal_poll_ms_unmetered"
        const val KEY_POLL_MS_METERED = "terminal_poll_ms_metered"
        const val KEY_POLL_MS_BOTH_LINKS = "terminal_poll_ms"
    }
}
