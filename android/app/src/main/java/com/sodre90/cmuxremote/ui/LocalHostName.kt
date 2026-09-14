package com.sodre90.cmuxremote.ui

import androidx.compose.runtime.compositionLocalOf

/**
 * The selected host's name, for copy that names the machine an action lands
 * on ("Show on home-server", "closed on Peters-MacBook"). Provided once by
 * [CmuxNavHost] for the whole graph, since every screen under it belongs to
 * that one host. Defaults to a neutral word so previews and tests read well.
 */
val LocalHostName = compositionLocalOf { "the host" }
