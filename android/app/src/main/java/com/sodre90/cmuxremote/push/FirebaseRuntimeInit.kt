package com.sodre90.cmuxremote.push

import android.content.Context
import android.util.Log
import com.google.firebase.FirebaseApp
import com.google.firebase.FirebaseOptions
import com.google.firebase.messaging.FirebaseMessaging
import com.sodre90.cmuxremote.data.FallbackBridgeClient
import com.sodre90.cmuxremote.data.FcmClientConfig
import com.sodre90.cmuxremote.data.Settings

/**
 * Initialises Firebase from a config the bridge handed over at pairing.
 *
 * Firebase normally reads its options from resources the google-services
 * Gradle plugin generates at build time, which made push a build-time
 * decision: an APK built without a google-services.json could never send its
 * FCM token, no matter how the bridge was configured. Supplying
 * [FirebaseOptions] directly is the supported way to decide it at runtime
 * instead.
 *
 * Returns whether a default FirebaseApp exists afterwards.
 */
fun ensureFirebaseInitialized(context: Context, config: () -> FcmClientConfig?): Boolean {
    // A build that DOES ship google-services.json has already been
    // initialised by FirebaseInitProvider before any of our code runs.
    // Re-initialising would throw, and the baked-in config is the more
    // specific choice anyway, so leave it alone.
    //
    // Checked against the DEFAULT app by name rather than "any app exists":
    // that is the one initializeApp(Context, FirebaseOptions) creates and the
    // only one whose presence makes a second call throw.
    //
    // config is a lambda so this returns before it is evaluated. Reading it
    // means Keystore-backed EncryptedSharedPreferences I/O, which a build with
    // a baked-in config should not pay for on every launch.
    if (FirebaseApp.getApps(context).any { it.name == FirebaseApp.DEFAULT_APP_NAME }) return true
    val cfg = config() ?: return false

    return try {
        val options = FirebaseOptions.Builder()
            .setProjectId(cfg.projectId)
            .setApplicationId(cfg.appId)
            .setApiKey(cfg.apiKey)
            .setGcmSenderId(cfg.senderId)
            .build()
        FirebaseApp.initializeApp(context, options)
        true
    } catch (e: Throwable) {
        // A malformed config is the operator's to fix; the app carries on
        // without push rather than failing to start. Builder.build() is inside
        // the try for that reason -- it throws on a blank application id, and
        // that must not take onCreate down with it.
        //
        // Only the exception class is logged, never the values: they are not
        // secret, but nothing here is ours to print.
        Log.w(TAG, "firebase init failed: ${e.javaClass.simpleName}")
        false
    }
}

private const val TAG = "CmuxPush"

/**
 * Brings push up now: initialise Firebase if it is not up yet, then get the
 * current token registered.
 *
 * Called on every launch and again the moment a pairing delivers a config.
 * That second call is what makes push work on the launch that pairs. Firebase
 * is initialised once per process from stored config, and pairing stores it
 * long after [android.app.Activity.onCreate] has already run and concluded
 * there was nothing to initialise -- so without this, a freshly paired phone
 * has no FirebaseApp, no token and no notification permission until the user
 * kills and reopens the app, which nothing tells them to do.
 *
 * Safe to call repeatedly: initialisation is guarded, and recording a token
 * already pending is idempotent (see [FcmTokenRegistrar.onTokenIssued]).
 *
 * Returns whether push is now up, which is what tells a caller holding an
 * Activity that this is the moment to ask for the notification permission --
 * a token registered against a phone that never granted it is dropped
 * silently on API 33+.
 */
fun activatePush(
    context: Context,
    settings: Settings,
    pairedBridges: () -> List<FallbackBridgeClient>,
): Boolean {
    if (!ensureFirebaseInitialized(context) { settings.fcmClientConfig() }) return false
    return try {
        FirebaseMessaging.getInstance().token.addOnSuccessListener { token ->
            FcmTokenRegistrar(settings, pairedBridges).onTokenIssued(token)
            enqueueFcmTokenRegistration(context)
        }
        true
    } catch (e: Throwable) {
        // Firebase present but unusable (no Play services, bad config). The
        // worker retry has nothing to retry against, so this is where it ends.
        Log.w(TAG, "fcm token request failed: ${e.javaClass.simpleName}")
        false
    }
}
