package com.sodre90.cmuxremote.data

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * Slots and hosts are configured independently and direct push is documented
 * as optional, so "the bridge sent no config" must not be read as "delete the
 * config another pairing's bridge sent".
 */
class FcmConfigOwnershipTest {

    private val mac = HostId("aa".repeat(16))
    private val linux = HostId("bb".repeat(16))

    @Test
    fun `an unclaimed config may be cleared by any pairing`() {
        assertTrue(mayClearFcmConfig(null, fcmConfigOwner(mac, ConnectionSlot.RELAY)))
        assertTrue(mayClearFcmConfig(null, fcmConfigOwner(linux, ConnectionSlot.DIRECT)))
    }

    @Test
    fun `the pairing that supplied the config may clear it`() {
        val owner = fcmConfigOwner(mac, ConnectionSlot.RELAY)
        assertTrue(mayClearFcmConfig(owner, fcmConfigOwner(mac, ConnectionSlot.RELAY)))
    }

    /** The regression: pairing a push-less direct agent after a push-enabled
     *  relay used to wipe the relay's config and kill push on both slots. */
    @Test
    fun `the other slot of the same host may not clear a config it did not supply`() {
        val owner = fcmConfigOwner(mac, ConnectionSlot.RELAY)
        assertFalse(mayClearFcmConfig(owner, fcmConfigOwner(mac, ConnectionSlot.DIRECT)))
    }

    /** The multi-host case of the same rule: the Linux agent's relay pairing
     *  hands over the same relay-supplied config, or none at all if its relay
     *  has push off -- and neither may take the Mac's config away. */
    @Test
    fun `another host may not clear a config it did not supply`() {
        val owner = fcmConfigOwner(mac, ConnectionSlot.RELAY)
        assertFalse(mayClearFcmConfig(owner, fcmConfigOwner(linux, ConnectionSlot.RELAY)))
    }
}
