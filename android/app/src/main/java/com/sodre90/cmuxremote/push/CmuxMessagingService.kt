package com.sodre90.cmuxremote.push

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import android.util.Log
import androidx.core.app.NotificationCompat
import com.google.firebase.messaging.FirebaseMessagingService
import com.google.firebase.messaging.RemoteMessage
import com.sodre90.cmuxremote.CmuxApp
import com.sodre90.cmuxremote.MainActivity
import com.sodre90.cmuxremote.R
import com.sodre90.cmuxremote.data.AppContainer
import com.sodre90.cmuxremote.data.ConnectionSlot
import com.sodre90.cmuxremote.data.HostId
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel

/**
 * Receives FCM pushes. On a data message with `type=attention` it posts a
 * notification that deep-links to the workspace that needs attention (resolved
 * to its exact terminal by CmuxNavHost, since cmux never tells us the pane).
 * `type=test` (see ConnectionSettingsScreen's "Send test notification") posts
 * the same way but with no workspace to deep-link to. On a new token it
 * registers the device with the bridge. Firebase only initialises when
 * `app/google-services.json` is present — without it these callbacks never
 * fire, so the app still builds and runs with push inactive.
 */
class CmuxMessagingService : FirebaseMessagingService() {

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    override fun onDestroy() {
        super.onDestroy()
        scope.cancel()
    }

    // FCM issues a rotated token exactly once. Record it durably and hand the
    // delivery to WorkManager rather than to a coroutine that dies with this
    // service: this callback is the app's only notice, and if the bridge happens
    // to be unreachable right now there is no second push to try again on --
    // push stays dead until someone opens the app (cmux-app-2cm).
    override fun onNewToken(token: String) {
        val container = (application as? CmuxApp)?.container ?: return
        FcmTokenRegistrar(container.settings, container::pairedBridges).onTokenIssued(token)
        enqueueFcmTokenRegistration(applicationContext)
    }

    override fun onMessageReceived(message: RemoteMessage) {
        val type = message.data["type"]
        if (type != "attention" && type != "test") return
        val container = (application as? CmuxApp)?.container
        val decrypted = decryptContent(container, message.data)
        val workspaceId = message.data["workspace_id"]?.takeIf { it.isNotBlank() }
        val surfaceId = message.data["surface_id"]?.takeIf { it.isNotBlank() }
        val notificationId = attentionNotificationId(workspaceId, surfaceId)
        if (decrypted == null && genericFallbackWouldHideContent(shownTitle(notificationId))) return
        val title = decrypted?.title ?: GENERIC_TITLE
        val body = decrypted?.body ?: GENERIC_BODY
        showNotification(title, body, decrypted?.host, workspaceId, surfaceId, notificationId)
    }

    /** What a push said once some host's session opened it, and which host
     *  that was -- the payload carries a slot but no host, so the deep link
     *  has to learn the host from the key that authenticated the message. */
    private class DecryptedPush(val host: HostId, val title: String, val body: String)

    /** The title of the notification currently on screen under [notificationId],
     *  or null if nothing is showing there. */
    private fun shownTitle(notificationId: Int): CharSequence? =
        getSystemService(NotificationManager::class.java)
            .activeNotifications
            .firstOrNull { it.id == notificationId }
            ?.notification?.extras?.getCharSequence(Notification.EXTRA_TITLE)

    /**
     * Decrypts the per-device e2e payload the bridge/relay embed under
     * `data["e2e"]` -- the bridge never sends real title/body in the clear (see
     * bridge/internal/server/push.go's buildEncryptedPush), so this is the only
     * way to recover the actual notification content. Returns null on anything
     * that isn't a clean decrypt: no session/key material yet, wrong slot,
     * corrupt blob, or an unpaired/wiped local session. The caller falls back to
     * one generic, content-free notification in every such case.
     */
    private fun decryptContent(container: AppContainer?, data: Map<String, String>): DecryptedPush? {
        val container = container ?: return null
        val blobB64 = data["e2e"] ?: run {
            Log.w(TAG, "push carried no e2e payload; sender had no session for this device")
            return null
        }
        val slot = data["slot"]?.let { name -> ConnectionSlot.entries.find { it.name.equals(name, ignoreCase = true) } }
            ?: return null
        // Every paired host holds its own session for the slot, and the wrong
        // one simply fails to authenticate the AEAD -- the receive counter is
        // only committed on a successful decrypt (validateAndCommitRecvCounter),
        // so trying the others first costs nothing.
        for (host in container.pairedHosts()) {
            val payload = try {
                decryptPushPayload(host.session(slot), container.cipher, blobB64)
            } catch (e: Exception) {
                Log.w(TAG, "push did not decrypt on host ${host.host.value} ${slot.name}: ${e::class.simpleName}")
                continue
            }
            return DecryptedPush(host.host, payload.title, payload.body)
        }
        return null
    }

    private fun showNotification(
        title: String,
        body: String,
        host: HostId?,
        workspaceId: String?,
        surfaceId: String?,
        notificationId: Int,
    ) {
        val nm = getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(
            NotificationChannel(CHANNEL_ID, "Agent attention", NotificationManager.IMPORTANCE_HIGH),
        )

        val pending =
            deepLinkIntent(ACTION_OPEN_PANE, notificationId, host, workspaceId, surfaceId, openInbox = false)
        val openInbox =
            deepLinkIntent(ACTION_OPEN_INBOX, notificationId, host, workspaceId, surfaceId, openInbox = true)

        val notification = NotificationCompat.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_stat_cmux)
            .setColor(getColor(R.color.notification_accent))
            .setContentTitle(title)
            .setContentText(body)
            // A real question runs well past the single collapsed line -- one
            // observed push was cut to "How...", which tells the user only that
            // they have to open the app to find out whether it needs them.
            .setStyle(NotificationCompat.BigTextStyle().bigText(body))
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setContentIntent(pending)
            .addAction(R.drawable.ic_stat_cmux, getString(R.string.notification_open_inbox), openInbox)
            .setAutoCancel(true)
            // Updating the tile must not buzz again. Repeat pushes for one
            // pending prompt are ordinary here -- cmux emits more than one
            // NeedsAttention frame for it, and a phone paired on both slots
            // gets the relay's copy and the agent's own, out of two stores
            // neither of which can see the other (cmux-app-17r). On a
            // high-importance channel every one of those re-alerted: one
            // prompt, a burst of buzzes. This suppresses only the re-alert of
            // a notification already on screen; once it is dismissed or
            // tapped, the next push alerts normally.
            .setOnlyAlertOnce(true)
            .build()

        nm.notify(notificationId, notification)
    }

    /**
     * A PendingIntent into [MainActivity] carrying the deep-link extras.
     *
     * [action] is what keeps the two intents apart. PendingIntent reuse is
     * decided by Intent.filterEquals, which compares action/data/type/component
     * and ignores extras entirely -- so without distinct actions, FLAG_UPDATE_CURRENT
     * would hand the second request the first one's intent and the "Open inbox"
     * button would open the pane instead.
     */
    private fun deepLinkIntent(
        action: String,
        notificationId: Int,
        host: HostId?,
        workspaceId: String?,
        surfaceId: String?,
        openInbox: Boolean,
    ): PendingIntent {
        val intent = Intent(this, MainActivity::class.java).apply {
            this.action = action
            flags = Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TOP
            putExtra(MainActivity.EXTRA_HOST_ID, host?.value)
            putExtra(MainActivity.EXTRA_WORKSPACE_ID, workspaceId)
            putExtra(MainActivity.EXTRA_SURFACE_ID, surfaceId)
            putExtra(MainActivity.EXTRA_OPEN_INBOX, openInbox)
        }
        return PendingIntent.getActivity(
            this,
            notificationId,
            intent,
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
    }

    companion object {
        const val CHANNEL_ID = "agent_attention"
        private const val TAG = "CmuxMessagingService"
        private const val ACTION_OPEN_PANE = "com.sodre90.cmuxremote.OPEN_PANE"
        private const val ACTION_OPEN_INBOX = "com.sodre90.cmuxremote.OPEN_INBOX"
    }
}

internal const val GENERIC_TITLE = "cmux needs your attention"
internal const val GENERIC_BODY = "Open the app to see what's happening"

/**
 * Whether posting the content-free fallback would replace something better.
 *
 * The relay and the Mac agent push the same attention event independently,
 * out of separate device stores that cannot see each other, and both copies
 * land on the one id [attentionNotificationId] returns. Whichever arrives
 * last is what the user is left looking at. So a copy that could not be
 * decrypted -- one whose sender had no session for this device, or one
 * encrypted for a session this device has since replaced -- would otherwise
 * overwrite the copy that decrypted fine with "cmux needs your attention"
 * (cmux-app-17r).
 *
 * Only this direction is guarded. Real content arriving after the fallback
 * still replaces it, because that path never consults this.
 */
internal fun genericFallbackWouldHideContent(shownTitle: CharSequence?): Boolean =
    shownTitle != null && shownTitle.toString() != GENERIC_TITLE

/**
 * The notification id an attention/test push is filed under -- shared between
 * the poster ([CmuxMessagingService.showNotification]) and anything that later
 * needs to cancel it (e.g. CmuxNavHost, once the workspace it points at is
 * actually opened) so the two never drift apart.
 */
fun attentionNotificationId(workspaceId: String?, surfaceId: String?): Int =
    (workspaceId ?: surfaceId ?: "attention").hashCode()
