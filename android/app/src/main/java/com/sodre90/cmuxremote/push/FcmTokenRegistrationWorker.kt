package com.sodre90.cmuxremote.push

import android.content.Context
import android.util.Log
import androidx.work.BackoffPolicy
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingWorkPolicy
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkRequest
import androidx.work.WorkerParameters
import com.sodre90.cmuxremote.CmuxApp
import java.util.concurrent.TimeUnit

private const val TAG = "FcmTokenRegistration"
private const val WORK_NAME = "fcm-token-registration"

/**
 * Retries FCM token registration until a slot accepts it, across process death
 * and reboots.
 *
 * This is the part of cmux-app-2cm that could not be done in the app's own
 * process: if push is dead, no push arrives to wake the app, so nothing short
 * of a scheduled job gets the token registered without the user happening to
 * open the app. WorkManager waits for a network, backs off exponentially, and
 * survives the reboot that a "retry on next launch" policy could not.
 */
class FcmTokenRegistrationWorker(
    appContext: Context,
    params: WorkerParameters,
) : CoroutineWorker(appContext, params) {

    override suspend fun doWork(): Result {
        val container = (applicationContext as? CmuxApp)?.container ?: return Result.success()
        val registrar = FcmTokenRegistrar(container.settings, container::pairedBridges)
        // The token itself is never logged anywhere below: it is the routing
        // credential for this device's notifications.
        return when (registrar.registerPending()) {
            RegistrationAttempt.DONE -> {
                Log.i(TAG, "FCM token registered")
                Result.success()
            }
            RegistrationAttempt.NOTHING_PENDING -> Result.success()
            // Pairing registers the token itself, so there is nothing to retry
            // against and holding a work request open would only burn wakeups.
            // The token stays pending, and pairing or the next launch picks it up.
            RegistrationAttempt.NOT_CONFIGURED -> Result.success()
            RegistrationAttempt.RETRY -> {
                Log.w(TAG, "no slot accepted the FCM token (attempt ${runAttemptCount + 1}); will retry")
                Result.retry()
            }
        }
    }
}

/**
 * Asks for the pending token to be registered as soon as there is a network.
 *
 * REPLACE, not KEEP: a token issued while an earlier attempt is still queued
 * supersedes it outright, and leaving the old request in place would leave the
 * retry backing off on behalf of a token that no longer exists.
 */
fun enqueueFcmTokenRegistration(context: Context) {
    val request = OneTimeWorkRequestBuilder<FcmTokenRegistrationWorker>()
        .setConstraints(Constraints.Builder().setRequiredNetworkType(NetworkType.CONNECTED).build())
        .setBackoffCriteria(
            BackoffPolicy.EXPONENTIAL,
            WorkRequest.MIN_BACKOFF_MILLIS,
            TimeUnit.MILLISECONDS,
        )
        .build()
    WorkManager.getInstance(context).enqueueUniqueWork(WORK_NAME, ExistingWorkPolicy.REPLACE, request)
}
