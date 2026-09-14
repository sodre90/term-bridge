package com.sodre90.cmuxremote.data

import android.content.Context
import android.content.SharedPreferences
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey

/**
 * The keystore-backed store [Settings] and [com.sodre90.cmuxremote.data.e2e.CryptoSession]
 * keep their secrets in. Built here, once per file, rather than inside those
 * classes so that [AppContainer] can hand the same instances to the host-keyed
 * migration first -- and so that a test can hand them a plain
 * SharedPreferences instead: there is no AndroidKeyStore off-device to build a
 * MasterKey against (cmux-app-fdl).
 */
fun encryptedPrefs(context: Context, name: String): SharedPreferences {
    val masterKey = MasterKey.Builder(context)
        .setKeyScheme(MasterKey.KeyScheme.AES256_GCM)
        .build()
    return EncryptedSharedPreferences.create(
        context,
        name,
        masterKey,
        EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
        EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
    )
}
