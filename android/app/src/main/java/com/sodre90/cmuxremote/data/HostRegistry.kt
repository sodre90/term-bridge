package com.sodre90.cmuxremote.data

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/** The durable half of [HostRegistry]. [Settings] is the real one; tests use
 *  an in-memory implementation. */
interface HostRegistryStore {
    fun loadHosts(): List<PairedHost>
    fun saveHosts(hosts: List<PairedHost>)
    fun loadSelectedHost(): HostId?
    fun saveSelectedHost(id: HostId?)
}

/**
 * The paired hosts and which one the app is currently showing. Every mutation
 * is written through to [store] before the flows update, so a process death
 * between the two can only lose an in-memory view, never a paired host.
 *
 * Exactly one host is selected whenever any is paired: registering the first
 * selects it, removing the selected one falls back to the first remaining.
 */
class HostRegistry(private val store: HostRegistryStore) {

    private val _hosts = MutableStateFlow(store.loadHosts())
    val hosts: StateFlow<List<PairedHost>> = _hosts.asStateFlow()

    private val _selected = MutableStateFlow(store.loadSelectedHost()?.takeIf { isRegistered(it) })
    val selected: StateFlow<HostId?> = _selected.asStateFlow()

    fun host(id: HostId): PairedHost? = _hosts.value.firstOrNull { it.id == id }

    private fun isRegistered(id: HostId) = host(id) != null

    fun selectedHost(): PairedHost? = _selected.value?.let(::host)

    /** Adds [host], or on a re-pair of a known host leaves its learned name and
     *  kind alone -- the placeholder a fresh pairing carries is no better than
     *  what an earlier GET /sessions already reported. */
    @Synchronized
    fun register(host: PairedHost) {
        if (host(host.id) == null) saveHosts(_hosts.value + host)
        if (_selected.value == null) select(host.id)
    }

    /** Records what [id] reported about itself. A blank [name] (an agent older
     *  than the host block) keeps whatever the registry already has. */
    @Synchronized
    fun describe(id: HostId, name: String, kind: String) {
        val current = host(id) ?: return
        val updated = current.copy(name = name.ifBlank { current.name }, kind = kind)
        if (updated != current) saveHosts(_hosts.value.map { if (it.id == id) updated else it })
    }

    @Synchronized
    fun select(id: HostId) {
        if (host(id) == null) return
        store.saveSelectedHost(id)
        _selected.value = id
    }

    @Synchronized
    fun remove(id: HostId) {
        saveHosts(_hosts.value.filterNot { it.id == id })
        if (_selected.value == id) {
            val next = _hosts.value.firstOrNull()?.id
            store.saveSelectedHost(next)
            _selected.value = next
        }
    }

    private fun saveHosts(hosts: List<PairedHost>) {
        store.saveHosts(hosts)
        _hosts.value = hosts
    }
}
