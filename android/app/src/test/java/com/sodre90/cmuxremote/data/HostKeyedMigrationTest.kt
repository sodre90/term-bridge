package com.sodre90.cmuxremote.data

import android.app.Application
import android.content.Context
import android.content.SharedPreferences
import com.sodre90.cmuxremote.data.e2e.CryptoSession
import com.sodre90.cmuxremote.model.HostKind
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.annotation.Config
import java.util.Base64

/**
 * The two pre-multi-host layouts an upgrading phone can hold, and where each
 * record has to land. Robolectric supplies a working SharedPreferences; the
 * encryption at rest is AndroidX's and not what the migration is about.
 */
@RunWith(RobolectricTestRunner::class)
@Config(application = Application::class)
class HostKeyedMigrationTest {

    private lateinit var secure: SharedPreferences
    private lateinit var e2e: SharedPreferences

    private val macKey = ByteArray(32) { 0x41 }
    private val macKeyB64 = Base64.getEncoder().encodeToString(macKey)
    private val mac = hostIdOf(macKey)

    @Before
    fun freshPrefs() {
        val app = RuntimeEnvironment.getApplication()
        secure = app.getSharedPreferences("secure_test", Context.MODE_PRIVATE).also { it.edit().clear().commit() }
        e2e = app.getSharedPreferences("e2e_test", Context.MODE_PRIVATE).also { it.edit().clear().commit() }
    }

    private fun storeDualPairing(slot: ConnectionSlot, url: String, sendCounter: Long = 7L) {
        val p = slot.name.lowercase() + "_"
        secure.edit()
            .putString(p + "base_url", url)
            .putString(p + "device_token", "tok-$p")
            .putBoolean(p + "credential_rejection_reported", true)
            .commit()
        e2e.edit()
            .putString(p + "device_public_key_b64", macKeyB64)
            .putString(p + "shared_secret_b64", "c2VjcmV0")
            .putLong(p + "send_counter", sendCounter)
            .putLong(p + "recv_highest", 41L)
            .putLong(p + "recv_window_bits", 3L)
            .commit()
    }

    @Test
    fun aFreshInstallMigratesNothing() {
        assertTrue(migrateToHostKeyed(secure, e2e).isEmpty())
        assertTrue(secure.all.isEmpty())
        assertTrue(e2e.all.isEmpty())
    }

    @Test
    fun bothSlotsOfOneMachineLandUnderOneHost() {
        storeDualPairing(ConnectionSlot.RELAY, "https://relay.example.org")
        storeDualPairing(ConnectionSlot.DIRECT, "https://mac.tailnet.ts.net:8443", sendCounter = 9L)
        secure.edit().putString("fcm_source_slot", "RELAY").commit()

        val hosts = migrateToHostKeyed(secure, e2e)

        assertEquals(listOf(PairedHost(mac, "relay.example.org", HostKind.CMUX)), hosts)
        val settings = Settings(secure)
        assertConfig("https://relay.example.org", "tok-relay_", settings.bridgeConfig(mac, ConnectionSlot.RELAY))
        assertConfig(
            "https://mac.tailnet.ts.net:8443",
            "tok-direct_",
            settings.bridgeConfig(mac, ConnectionSlot.DIRECT),
        )
        assertTrue(settings.rejectionReportLog(mac).wasRejectionReported(ConnectionSlot.DIRECT))
        assertEquals(fcmConfigOwner(mac, ConnectionSlot.RELAY), secure.getString("fcm_source_slot", null))

        val relay = CryptoSession(e2e, mac, ConnectionSlot.RELAY)
        assertTrue(relay.isPaired())
        assertEquals(7L, relay.nextSendCounter())
        assertEquals(9L, CryptoSession(e2e, mac, ConnectionSlot.DIRECT).nextSendCounter())
        assertEquals(41L, e2e.getLong(hostSlotKey(mac, ConnectionSlot.RELAY, "recv_highest"), -1L))

        assertNoLegacyKeysRemain()
    }

    @Test
    fun theOriginalSinglePairingIsRoutedBySlotInferenceAndThenByHost() {
        secure.edit()
            .putString("base_url", "https://mac.tailnet.ts.net:8443")
            .putString("device_token", "tok-legacy")
            .commit()
        e2e.edit()
            .putString("device_public_key_b64", macKeyB64)
            .putString("shared_secret_b64", "c2VjcmV0")
            .putLong("send_counter", 3L)
            .putLong("recv_highest", 12L)
            .putLong("recv_window_bits", 1L)
            .commit()

        val hosts = migrateToHostKeyed(secure, e2e)

        assertEquals(listOf(PairedHost(mac, "mac.tailnet.ts.net", HostKind.CMUX)), hosts)
        val settings = Settings(secure)
        assertNull(settings.bridgeConfig(mac, ConnectionSlot.RELAY))
        assertConfig("https://mac.tailnet.ts.net:8443", "tok-legacy", settings.bridgeConfig(mac, ConnectionSlot.DIRECT))
        assertEquals(3L, CryptoSession(e2e, mac, ConnectionSlot.DIRECT).nextSendCounter())
        assertNoLegacyKeysRemain()
    }

    /** No e2e record means no agent key to attribute the credential to -- and
     *  every request on it was about to fail as unpaired anyway. */
    @Test
    fun aSlotWithoutAnE2eRecordIsDroppedRatherThanGuessed() {
        storeDualPairing(ConnectionSlot.RELAY, "https://relay.example.org")
        secure.edit()
            .putString("direct_base_url", "https://mac.tailnet.ts.net:8443")
            .putString("direct_device_token", "tok-orphan")
            .putString("fcm_source_slot", "DIRECT")
            .commit()

        val hosts = migrateToHostKeyed(secure, e2e)

        assertEquals(1, hosts.size)
        assertNull(Settings(secure).bridgeConfig(mac, ConnectionSlot.DIRECT))
        assertNull(secure.getString("fcm_source_slot", null))
        assertNoLegacyKeysRemain()
    }

    @Test
    fun aSecondRunFindsNothingToDoAndLeavesTheMigratedRecordsAlone() {
        storeDualPairing(ConnectionSlot.RELAY, "https://relay.example.org")
        migrateToHostKeyed(secure, e2e)
        val secureAfterFirst = secure.all.toMap()
        val e2eAfterFirst = e2e.all.toMap()

        assertTrue(migrateToHostKeyed(secure, e2e).isEmpty())

        assertEquals(secureAfterFirst, secure.all)
        assertEquals(e2eAfterFirst, e2e.all)
    }

    private fun assertConfig(url: String, token: String, actual: BridgeConfig?) {
        assertEquals(url, actual?.baseUrl)
        assertEquals(token, actual?.deviceToken)
    }

    private fun assertNoLegacyKeysRemain() {
        val legacyPrefixes = listOf("relay_", "direct_")
        val legacyBases = listOf(
            "base_url",
            "device_token",
            "credential_rejection_reported",
            "device_public_key_b64",
            "shared_secret_b64",
            "send_counter",
            "recv_highest",
            "recv_window_bits",
        )
        for (key in secure.all.keys + e2e.all.keys) {
            assertFalse("legacy key survived: $key", key in legacyBases)
            assertFalse("slot-prefixed key survived: $key", legacyPrefixes.any { key.startsWith(it) })
        }
    }
}
