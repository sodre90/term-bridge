package com.sodre90.cmuxremote.data

import com.sodre90.cmuxremote.model.HostKind
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNull
import org.junit.Test

class HostRegistryTest {

    private class InMemoryStore : HostRegistryStore {
        var hosts: List<PairedHost> = emptyList()
        var selected: HostId? = null
        override fun loadHosts() = hosts
        override fun saveHosts(hosts: List<PairedHost>) {
            this.hosts = hosts
        }
        override fun loadSelectedHost() = selected
        override fun saveSelectedHost(id: HostId?) {
            selected = id
        }
    }

    private val mac = PairedHost(hostIdOf(ByteArray(32) { 1 }), "relay.example.org", HostKind.CMUX)
    private val linux = PairedHost(hostIdOf(ByteArray(32) { 2 }), "relay.example.org", HostKind.CMUX)

    @Test
    fun hostIdsAreStableHexDigestsOfTheAgentKeyAlone() {
        assertEquals(mac.id, hostIdOf(ByteArray(32) { 1 }))
        assertEquals(32, mac.id.value.length)
        assertNotEquals(mac.id, linux.id)
    }

    @Test
    fun theFirstRegisteredHostBecomesTheSelectionAndLaterOnesDoNot() {
        val store = InMemoryStore()
        val registry = HostRegistry(store)

        registry.register(mac)
        registry.register(linux)

        assertEquals(listOf(mac, linux), registry.hosts.value)
        assertEquals(mac.id, registry.selected.value)
        assertEquals(mac.id, store.selected)
    }

    @Test
    fun aRePairKeepsTheNameAndKindTheHostAlreadyReported() {
        val registry = HostRegistry(InMemoryStore())
        registry.register(linux)
        registry.describe(linux.id, "home-server", HostKind.TMUX)

        registry.register(linux)

        assertEquals(PairedHost(linux.id, "home-server", HostKind.TMUX), registry.host(linux.id))
    }

    @Test
    fun aBlankReportedNameKeepsThePlaceholderButStillUpdatesTheKind() {
        val registry = HostRegistry(InMemoryStore())
        registry.register(linux)

        registry.describe(linux.id, "", HostKind.TMUX)

        assertEquals(PairedHost(linux.id, "relay.example.org", HostKind.TMUX), registry.host(linux.id))
    }

    @Test
    fun removingTheSelectedHostFallsBackToAnotherAndRemovingTheLastClearsTheSelection() {
        val store = InMemoryStore()
        val registry = HostRegistry(store)
        registry.register(mac)
        registry.register(linux)
        registry.select(linux.id)

        registry.remove(linux.id)
        assertEquals(mac.id, registry.selected.value)

        registry.remove(mac.id)
        assertNull(registry.selected.value)
        assertNull(store.selected)
        assertEquals(emptyList<PairedHost>(), registry.hosts.value)
    }

    @Test
    fun aPersistedSelectionOfAHostNoLongerStoredIsIgnored() {
        val store = InMemoryStore().apply {
            hosts = listOf(mac)
            selected = linux.id
        }

        assertNull(HostRegistry(store).selected.value)
    }

    @Test
    fun selectingAnUnknownHostIsANoOp() {
        val registry = HostRegistry(InMemoryStore())
        registry.register(mac)

        registry.select(linux.id)

        assertEquals(mac.id, registry.selected.value)
    }
}
