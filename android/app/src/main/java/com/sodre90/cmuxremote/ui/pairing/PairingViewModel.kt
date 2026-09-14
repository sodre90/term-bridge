package com.sodre90.cmuxremote.ui.pairing

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.sodre90.cmuxremote.data.ConnectionSlot
import com.sodre90.cmuxremote.data.PairingGateway
import com.sodre90.cmuxremote.data.pairing.PairingCodeInvalidException
import com.sodre90.cmuxremote.data.pairing.PairingNotAnsweredException
import com.sodre90.cmuxremote.data.pairing.PairingQr
import com.sodre90.cmuxremote.data.pairing.PairingRefusedException
import com.sodre90.cmuxremote.data.pairing.isExpired
import com.sodre90.cmuxremote.data.pairing.parsePairingQr
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

sealed interface PairingUiState {
    data object Scanning : PairingUiState

    /** The SAS fingerprint is computed (no network call yet) -- a human
     *  compares it against the CLI's printed fingerprint before either side
     *  commits. See docs/superpowers/specs/2026-07-10-pairing-mitm-fingerprint-design.md. */
    data class AwaitingConfirmation(val fingerprint: String) : PairingUiState
    data object Pairing : PairingUiState

    /** The pairing code is redeemed and the phone is waiting for the agent's
     *  operator to accept or refuse. It keeps the fingerprint on screen
     *  because this is exactly the window in which that comparison is being
     *  made at the Mac -- hiding it here would be actively unhelpful.
     *  Nothing is persisted while in this state (cmux-app-gmo). */
    data class AwaitingOperator(val fingerprint: String) : PairingUiState

    /** Carries the same fingerprint [AwaitingConfirmation] showed -- the CLI
     *  can't print its own fingerprint until it sees this device's POST
     *  land, so it necessarily appears *after* the phone's screen already
     *  moved past AwaitingConfirmation. Keeping the code here (until the
     *  human dismisses it, rather than auto-navigating away) is what makes
     *  a real comparison against the CLI's printout possible. */
    data class Success(val fingerprint: String) : PairingUiState
    data class Error(val message: String) : PairingUiState
}

/**
 * Backs the QR-scan onboarding screen. [onQrScanned] is called by the
 * camera analyzer on every decoded barcode payload -- most calls are
 * ignored (foreign QR content, or a scan while already mid-pairing).
 *
 * The error-message parameters are pre-resolved `strings.xml` text passed in
 * by the caller (see CmuxNavHost) rather than resolved here: a ViewModel has
 * no @Composable context to call `stringResource()` itself. [codeInvalidScanAgainMessage]
 * and [codeInvalidAskFreshMessage] read differently on purpose: the QR path
 * suggests scanning again (the user is already looking at the scanner), the
 * manual-entry path suggests asking for a fresh code instead.
 */
class PairingViewModel(
    private val pairing: PairingGateway,
    private val slot: ConnectionSlot,
    private val codeExpiredMessage: String,
    private val codeInvalidScanAgainMessage: String,
    private val codeInvalidAskFreshMessage: String,
    private val pairingFailedMessage: String,
    private val pairingRefusedMessage: String,
    private val pairingNotAnsweredMessage: String,
) : ViewModel() {

    private val _state = MutableStateFlow<PairingUiState>(PairingUiState.Scanning)
    val state: StateFlow<PairingUiState> = _state.asStateFlow()

    // Set while state is AwaitingConfirmation, consumed by onConfirmed/
    // onRejected. Not part of PairingUiState itself: the confirmation
    // screen only needs the fingerprint to display, not the QR payload.
    private var pendingQr: PairingQr? = null
    private var pendingInvalidCodeMessage: String = pairingFailedMessage

    fun onQrScanned(raw: String) {
        if (_state.value !is PairingUiState.Scanning) return
        val qr = parsePairingQr(raw) ?: return
        if (qr.isExpired()) {
            _state.value = PairingUiState.Error(codeExpiredMessage)
            return
        }
        prepareAndAwaitConfirmation(qr, invalidCodeMessage = codeInvalidScanAgainMessage)
    }

    /** Manual-entry fallback for when scanning isn't possible (no camera, or
     *  pairing remotely): resolves the server URL + the code `term-bridge
     *  pair-device` also prints, into the same shape a scanned QR produces. */
    fun onManualEntrySubmitted(serverUrl: String, code: String) {
        if (_state.value !is PairingUiState.Scanning) return
        if (serverUrl.isBlank() || code.isBlank()) return
        _state.value = PairingUiState.Pairing
        viewModelScope.launch {
            try {
                val qr = pairing.pairingClient(slot).resolveManualCode(serverUrl.trim(), code.trim())
                prepareAndAwaitConfirmation(qr, invalidCodeMessage = codeInvalidAskFreshMessage)
            } catch (e: PairingCodeInvalidException) {
                _state.value = PairingUiState.Error(codeInvalidAskFreshMessage)
            } catch (e: Exception) {
                _state.value = PairingUiState.Error(e.message ?: pairingFailedMessage)
            }
        }
    }

    /** Computes the SAS fingerprint for [qr] (no network call) and moves to
     *  AwaitingConfirmation, or straight to Error if [qr] fails the
     *  https:// check [com.sodre90.cmuxremote.data.pairing.PairingSession.prepare]
     *  also performs. */
    private fun prepareAndAwaitConfirmation(qr: PairingQr, invalidCodeMessage: String) {
        val fingerprint = try {
            pairing.pairingClient(slot).prepare(qr)
        } catch (e: Exception) {
            _state.value = PairingUiState.Error(e.message ?: pairingFailedMessage)
            return
        }
        pendingQr = qr
        pendingInvalidCodeMessage = invalidCodeMessage
        _state.value = PairingUiState.AwaitingConfirmation(fingerprint)
    }

    /** The human confirmed the fingerprint matches the CLI's: redeem the
     *  code, then wait for the operator at the Mac to answer the same
     *  comparison before anything is persisted. */
    fun onConfirmed() {
        val awaiting = _state.value as? PairingUiState.AwaitingConfirmation ?: return
        val qr = pendingQr ?: return
        _state.value = PairingUiState.Pairing
        viewModelScope.launch {
            try {
                pairing.pairingClient(slot).commit(qr) {
                    _state.value = PairingUiState.AwaitingOperator(awaiting.fingerprint)
                }
                _state.value = PairingUiState.Success(awaiting.fingerprint)
            } catch (e: PairingCodeInvalidException) {
                _state.value = PairingUiState.Error(pendingInvalidCodeMessage)
            } catch (e: PairingRefusedException) {
                _state.value = PairingUiState.Error(pairingRefusedMessage)
            } catch (e: PairingNotAnsweredException) {
                _state.value = PairingUiState.Error(pairingNotAnsweredMessage)
            } catch (e: Exception) {
                _state.value = PairingUiState.Error(e.message ?: pairingFailedMessage)
            } finally {
                pendingQr = null
            }
        }
    }

    /** The human rejected the fingerprint (or backed out): [prepare] never
     *  made a network call, so this returns to scanning with nothing
     *  consumed server-side. */
    fun onRejected() {
        if (_state.value !is PairingUiState.AwaitingConfirmation) return
        pendingQr = null
        _state.value = PairingUiState.Scanning
    }

    /** Returns to the scanning state after an error. */
    fun retry() {
        pendingQr = null
        _state.value = PairingUiState.Scanning
    }
}
