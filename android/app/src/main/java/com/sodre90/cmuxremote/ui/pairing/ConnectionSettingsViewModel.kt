package com.sodre90.cmuxremote.ui.pairing

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.sodre90.cmuxremote.data.BridgeGateway
import com.sodre90.cmuxremote.data.TerminalDisplayGateway
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

/** State of the "Send test notification" button on [ConnectionSettingsScreen] --
 *  a debugging tool, so [Error] deliberately carries the real failure message
 *  (see [BridgeException][com.sodre90.cmuxremote.data.BridgeException]'s
 *  `bodyText`) rather than a generic string. */
sealed interface TestPushUiState {
    data object Idle : TestPushUiState
    data object Sending : TestPushUiState
    data object Success : TestPushUiState
    data class Error(val message: String) : TestPushUiState
}

/** What the Connections screen knows about the *agent's* version. The app's own
 *  version needs no state of its own -- it is a compile-time constant.
 *
 *  [Unavailable] deliberately collapses every way of not knowing (no slot
 *  configured, the agent unreachable, an agent too old to serve `GET /version`)
 *  into one: they are indistinguishable to the user, who can act on none of
 *  them from this screen, and the surrounding connection cards already say
 *  which slots are paired and healthy. */
sealed interface BridgeVersionUiState {
    data object Loading : BridgeVersionUiState
    data class Known(val version: String) : BridgeVersionUiState
    data object Unavailable : BridgeVersionUiState
}

/** Backs [ConnectionSettingsScreen]'s "Send test notification" button --
 *  see bridge/internal/server/test_push.go and
 *  bridge/internal/relay/testpush.go's `POST /devices/test-push`.
 *
 *  The error-message parameters are pre-resolved `strings.xml` text passed in
 *  by the caller (see CmuxNavHost) rather than resolved here: a ViewModel has
 *  no @Composable context to call `stringResource()` itself. */
class ConnectionSettingsViewModel(
    private val bridge: BridgeGateway,
    private val terminalDisplay: TerminalDisplayGateway,
    private val bridgeNotConfiguredMessage: String,
    private val testPushFailedMessage: String,
) : ViewModel() {

    private val _testPushState = MutableStateFlow<TestPushUiState>(TestPushUiState.Idle)
    val testPushState: StateFlow<TestPushUiState> = _testPushState.asStateFlow()

    private val _bridgeVersion = MutableStateFlow<BridgeVersionUiState>(BridgeVersionUiState.Loading)
    val bridgeVersion: StateFlow<BridgeVersionUiState> = _bridgeVersion.asStateFlow()

    /** Fetches the agent's version once. Called when the screen appears rather
     *  than in `init`: it is a network round trip for a line of text, and the
     *  screen is also the first destination on an unpaired install, where there
     *  is nothing to ask. */
    fun loadBridgeVersion() {
        val client = bridge.activeBridge()
        if (client == null) {
            _bridgeVersion.value = BridgeVersionUiState.Unavailable
            return
        }
        viewModelScope.launch {
            _bridgeVersion.value = try {
                // An agent too old for the route 404s, and one that answers
                // without the field decodes to "": both are "we don't know",
                // not a version worth printing.
                client.version().takeIf { it.isNotBlank() }
                    ?.let { BridgeVersionUiState.Known(it) }
                    ?: BridgeVersionUiState.Unavailable
            } catch (_: Exception) {
                BridgeVersionUiState.Unavailable
            }
        }
    }

    fun loadFontZoom(): Float = terminalDisplay.loadFontZoom()

    fun loadWheelScrolling(): Boolean = terminalDisplay.loadWheelScrolling()

    fun saveWheelScrolling(enabled: Boolean) = terminalDisplay.saveWheelScrolling(enabled)

    fun loadTerminalPollMs(metered: Boolean): Int = terminalDisplay.loadTerminalPollMs(metered)

    fun saveTerminalPollMs(metered: Boolean, ms: Int) = terminalDisplay.saveTerminalPollMs(metered, ms)
    fun saveFontZoom(zoom: Float) = terminalDisplay.saveFontZoom(zoom)

    fun sendTestPush() {
        if (_testPushState.value is TestPushUiState.Sending) return
        val client = bridge.activeBridge()
        if (client == null) {
            _testPushState.value = TestPushUiState.Error(bridgeNotConfiguredMessage)
            return
        }
        _testPushState.value = TestPushUiState.Sending
        viewModelScope.launch {
            try {
                client.sendTestPush()
                _testPushState.value = TestPushUiState.Success
            } catch (e: Exception) {
                _testPushState.value = TestPushUiState.Error(e.message ?: testPushFailedMessage)
            }
        }
    }
}
