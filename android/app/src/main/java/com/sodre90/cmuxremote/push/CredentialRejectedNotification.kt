package com.sodre90.cmuxremote.push

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import androidx.core.app.NotificationCompat
import com.sodre90.cmuxremote.MainActivity
import com.sodre90.cmuxremote.R
import com.sodre90.cmuxremote.data.ConnectionSlot

/**
 * Tells the user that [slot]'s server on [hostName] no longer recognises this
 * device, while the other slot is (usually) still serving -- which is the only useful moment
 * to say so. A banner that appears once you are already locked out is worth
 * nothing; that state is what this exists to prevent.
 *
 * Fired on the transition into rejected, never on the state, and the
 * once-per-rejection rule is enforced by [com.sodre90.cmuxremote.data.SlotCredentialHealth].
 *
 * Reuses the attention channel rather than adding one, and creates it here
 * because that channel otherwise only comes into existence once a push has
 * arrived -- which, on a phone whose credentials are failing, it may not have.
 */
fun showCredentialRejectedNotification(context: Context, hostName: String, slot: ConnectionSlot) {
    val nm = context.getSystemService(NotificationManager::class.java) ?: return
    nm.createNotificationChannel(
        NotificationChannel(
            CmuxMessagingService.CHANNEL_ID,
            "Agent attention",
            NotificationManager.IMPORTANCE_HIGH,
        ),
    )

    val slotLabel = context.getString(
        if (slot == ConnectionSlot.RELAY) R.string.connection_slot_relay else R.string.connection_slot_direct,
    )
    val body = context.getString(
        if (slot == ConnectionSlot.RELAY) {
            R.string.credential_rejected_body_relay
        } else {
            R.string.credential_rejected_body_direct
        },
    )

    val notificationId = credentialRejectedNotificationId(hostName, slot)
    val pending = PendingIntent.getActivity(
        context,
        notificationId,
        Intent(context, MainActivity::class.java).apply {
            flags = Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TOP
        },
        PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
    )

    nm.notify(
        notificationId,
        NotificationCompat.Builder(context, CmuxMessagingService.CHANNEL_ID)
            .setSmallIcon(android.R.drawable.ic_dialog_alert)
            .setContentTitle(context.getString(R.string.credential_rejected_title, slotLabel, hostName))
            .setContentText(body)
            .setStyle(NotificationCompat.BigTextStyle().bigText(body))
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setContentIntent(pending)
            .setAutoCancel(true)
            .build(),
    )
}

/** Stable per slot, and disjoint from [attentionNotificationId]'s workspace
 *  hashes, so a repeat for the same slot replaces its own tile instead of
 *  stacking or displacing an attention notification. */
private fun credentialRejectedNotificationId(hostName: String, slot: ConnectionSlot): Int =
    "credential_rejected_${hostName}_${slot.name}".hashCode()
