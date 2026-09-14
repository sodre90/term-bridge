package com.sodre90.cmuxremote

import android.Manifest
import android.content.Intent
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.lifecycleScope
import androidx.lifecycle.repeatOnLifecycle
import com.google.firebase.FirebaseApp
import com.sodre90.cmuxremote.data.AppContainer
import com.sodre90.cmuxremote.data.HostId
import com.sodre90.cmuxremote.push.activatePush
import com.sodre90.cmuxremote.ui.CmuxNavHost
import com.sodre90.cmuxremote.ui.theme.CmuxTheme
import kotlinx.coroutines.flow.drop
import kotlinx.coroutines.launch

class MainActivity : ComponentActivity() {

    private val requestNotifications =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) { }

    // Backed by Compose state (not plain vals read once in onCreate) so a
    // notification tap while this task is already running - the common case,
    // since singleTask launchMode reuses the instance via onNewIntent instead
    // of a fresh onCreate - still reaches CmuxNavHost's deep-link resolution.
    private var pendingWorkspaceId by mutableStateOf<String?>(null)
    private var pendingSurfaceId by mutableStateOf<String?>(null)

    // Set by the notification's "Open inbox" action, which wants the prompt
    // list rather than the pane the body came from -- answering is what the
    // user came to do, and the inbox is where they can do it.
    private var pendingOpenInbox by mutableStateOf(false)

    // Bumped on every applyDeepLink call, independent of whether the ids
    // above actually changed value. A repeat notification for the same
    // workspace (e.g. the same agent pinging again) would otherwise leave
    // pendingWorkspaceId equal to its previous value, and CmuxNavHost's
    // LaunchedEffect only restarts when a keyed value changes - so without
    // this token, re-tapping such a notification silently did nothing.
    private var pendingDeepLinkToken by mutableIntStateOf(0)

    // Held so onStart/onStop can flip the process-wide foreground flag the
    // streaming subscriptions pause on (see AppContainer.setAppForeground).
    private lateinit var container: AppContainer

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()

        // Before anything asks whether Firebase is configured: the config now
        // arrives at pairing rather than being compiled in, so it has to be
        // read off Settings and applied first. Needs the container, which is
        // why this moved above the permission prompt.
        container = (application as CmuxApp).container
        activatePush(applicationContext, container.settings, container::pairedBridges)
        requestNotificationsIfPushIsUp()
        promptOnPushActivatedByPairing()

        // Only on a genuine start. A configuration change recreates the Activity
        // with the SAME intent, so re-applying it re-navigates -- rotating in a
        // pane threw the user back to whatever the notification had pointed at,
        // however far they had navigated since. Later taps arrive via
        // onNewIntent, which is the only other place a deep link comes from.
        if (savedInstanceState == null) applyDeepLink(intent)
        setContent {
            CmuxTheme {
                CmuxNavHost(
                    container,
                    pendingWorkspaceId = pendingWorkspaceId,
                    pendingSurfaceId = pendingSurfaceId,
                    pendingOpenInbox = pendingOpenInbox,
                    pendingDeepLinkToken = pendingDeepLinkToken,
                )
            }
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        applyDeepLink(intent)
    }

    override fun onStart() {
        super.onStart()
        container.setAppForeground(true)
    }

    // Pauses every streaming socket subscription and its event-driven
    // refetches for as long as the user is away -- the single biggest lever
    // on cellular data usage, since viewModelScope alone keeps them running
    // with the screen off. Push notifications cover attention while paused.
    override fun onStop() {
        super.onStop()
        container.setAppForeground(false)
    }

    private fun applyDeepLink(intent: Intent) {
        // Before the ids below are published: CmuxNavHost is keyed on the
        // selected host, so switching first means the navigation that follows
        // runs inside the new host's screens rather than the old one's.
        intent.getStringExtra(EXTRA_HOST_ID)?.let { container.selectHost(HostId(it)) }
        pendingWorkspaceId = intent.getStringExtra(EXTRA_WORKSPACE_ID)
        pendingSurfaceId = intent.getStringExtra(EXTRA_SURFACE_ID)
        pendingOpenInbox = intent.getBooleanExtra(EXTRA_OPEN_INBOX, false)
        pendingDeepLinkToken++
    }

    /**
     * Asks for the notification permission once push is actually up.
     *
     * Gated on Firebase rather than launched unconditionally because without
     * push there is nothing to notify about, and a permission dialog nothing
     * will ever use is a prompt the user can only get wrong.
     */
    private fun requestNotificationsIfPushIsUp() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU && isFirebaseConfigured()) {
            requestNotifications.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    /**
     * Re-runs that prompt when a pairing brings push up mid-session.
     *
     * The Firebase config arrives at pairing now, so [onCreate]'s prompt runs
     * before there is anything to prompt about and correctly stays silent.
     * Pairing then registers an FCM token and the bridge starts sending --
     * into a phone that was never asked, which on API 33+ drops every one of
     * them silently until the next launch (cmux-app-snt).
     *
     * [drop] skips the value replayed on each restart, so only genuinely new
     * activations prompt; STARTED means a pairing that completes while the
     * user is elsewhere is picked up when they come back.
     */
    private fun promptOnPushActivatedByPairing() {
        lifecycleScope.launch {
            repeatOnLifecycle(Lifecycle.State.STARTED) {
                container.pushActivations().drop(1).collect { requestNotificationsIfPushIsUp() }
            }
        }
    }

    /**
     * Whether a FirebaseApp exists: either from a compiled-in
     * `google-services.json` or from a config a pairing delivered (see
     * [activatePush]). Without one [FirebaseApp.getInstance] throws.
     */
    private fun isFirebaseConfigured(): Boolean = try {
        FirebaseApp.getInstance()
        true
    } catch (_: Throwable) {
        false
    }

    companion object {
        const val EXTRA_HOST_ID = "cmux.host_id"
        const val EXTRA_WORKSPACE_ID = "cmux.workspace_id"
        const val EXTRA_SURFACE_ID = "cmux.surface_id"
        const val EXTRA_OPEN_INBOX = "cmux.open_inbox"
        private const val TAG = "MainActivity"
    }
}
