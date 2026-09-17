package com.sodre90.cmuxremote.data

/**
 * The phone-local terminal font-size preference surface
 * [TerminalViewModel][com.sodre90.cmuxremote.ui.terminal.TerminalViewModel]
 * and
 * [ConnectionSettingsViewModel][com.sodre90.cmuxremote.ui.pairing.ConnectionSettingsViewModel]
 * consume -- see [TerminalDisplayStore], which [AppContainer] delegates to.
 */
interface TerminalDisplayGateway {
    /** The persisted pinch-zoom multiplier over the fit-to-width baseline -- 1x by default. */
    fun loadFontZoom(): Float
    fun saveFontZoom(zoom: Float)

    /**
     * How often the bridge re-reads an open pane for output, in milliseconds,
     * kept separately for metered and unmetered links -- see
     * [DEFAULT_POLL_MS_METERED] for what the dial actually trades.
     *
     * Read when a terminal socket is opened, so a change takes effect on the
     * next connect rather than on a pane already on screen. Which of the two is
     * read is decided by [NetworkCost] at that same moment.
     */
    fun loadTerminalPollMs(metered: Boolean): Int
    fun saveTerminalPollMs(metered: Boolean, ms: Int)
}
