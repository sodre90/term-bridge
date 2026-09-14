package com.sodre90.cmuxremote.data

import android.content.Context
import com.goterl.lazysodium.LazySodiumAndroid
import com.goterl.lazysodium.SodiumAndroid
import com.sodre90.cmuxremote.data.e2e.Cipher
import com.sodre90.cmuxremote.data.e2e.CryptoSession
import com.sodre90.cmuxremote.data.pairing.PairingClient
import com.sodre90.cmuxremote.model.HostKind
import com.sodre90.cmuxremote.push.activatePush
import com.sodre90.cmuxremote.push.showCredentialRejectedNotification
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import okhttp3.OkHttpClient

/**
 * Manual dependency container held by [com.sodre90.cmuxremote.CmuxApp]. Keeps
 * one [HostConnections] per paired host and implements the narrow
 * [BridgeGateway]/[WorkspaceOrderGateway]/[PairingGateway]/[TerminalDisplayGateway]
 * interfaces ViewModels depend on by delegating to the host the user has
 * selected (see [HostRegistry]). ViewModels depend on the interfaces instead
 * of this concrete class so they stay constructible in a plain JVM test --
 * this class's init does EncryptedSharedPreferences + Keystore I/O and can't
 * be.
 *
 * A ViewModel that outlives a host switch would keep talking to the old host
 * through these delegates, so the UI recreates every screen on a switch
 * (CmuxNavHost) rather than letting one follow the selection mid-flight.
 */
class AppContainer(
    private val appContext: Context,
) : BridgeGateway, WorkspaceOrderGateway, PairingGateway, TerminalDisplayGateway {

    private val securePrefs = encryptedPrefs(appContext, Settings.PREFS_NAME)
    private val e2ePrefs = encryptedPrefs(appContext, CryptoSession.PREFS_NAME)

    // Before Settings and any CryptoSession are built on these files: both
    // read their records in their constructors (see HostKeyedMigration).
    private val migratedHosts = migrateToHostKeyed(securePrefs, e2ePrefs)

    val settings = Settings(securePrefs)
    val cipher = Cipher(LazySodiumAndroid(SodiumAndroid()))
    val workspaceOrderStore = WorkspaceOrderStore(appContext)
    val terminalDisplayStore = TerminalDisplayStore(appContext)
    val networkCost = NetworkCost(appContext)
    val hostRegistry = HostRegistry(settings)

    init {
        migratedHosts.forEach(hostRegistry::register)
        migratedHosts.firstOrNull()?.let { workspaceOrderStore.adoptLegacyOrder(it.id) }
    }

    private val hosts = mutableMapOf<HostId, HostConnections>()

    /** The connections for [id], built on first use. Cheap to hold for a host
     *  that is not selected: nothing in it opens a socket until asked. */
    @Synchronized
    fun host(id: HostId): HostConnections = hosts.getOrPut(id) {
        HostConnections(
            host = id,
            settings = settings,
            e2ePrefs = e2ePrefs,
            cipher = cipher,
            terminalPollMs = { terminalDisplayStore.loadTerminalPollMs(networkCost.isMetered()) },
            onNewRejection = { slot -> showCredentialRejectedNotification(appContext, slot) },
            onHostInfo = { info -> hostRegistry.describe(id, info.name, info.kind) },
        )
    }

    /** Every paired host's connections, selected first -- for the callers that
     *  must reach all of them (push registration, push decryption). */
    fun pairedHosts(): List<HostConnections> {
        val selected = hostRegistry.selected.value
        return hostRegistry.hosts.value
            .sortedByDescending { it.id == selected }
            .map { host(it.id) }
    }

    /** The host every gateway method below reads and writes. A process with no
     *  pairing runs against [HostId.NONE], which has no credentials and so
     *  answers exactly like the old single-host container did before pairing. */
    fun selectedHost(): HostConnections = host(hostRegistry.selected.value ?: HostId.NONE)

    fun selectHost(id: HostId) = hostRegistry.select(id)

    /** "Forget" for ([host], [slot]). A host whose last slot is forgotten leaves
     *  the registry too, and the selection moves on to another host if there
     *  is one. */
    @Synchronized
    fun forgetSlot(host: HostId, slot: ConnectionSlot) {
        host(host).forgetSlot(slot)
        // Drops any keypair a half-finished pairing left pending, so the next
        // attempt cannot commit a key whose fingerprint predates the Forget.
        pairingClients.remove(slot)
        if (!host(host).isConfigured()) {
            hostRegistry.remove(host)
            hosts.remove(host)
        }
    }

    // One PairingClient per slot, not one per call: it holds the keypair
    // minted by prepare() until the matching commit() submits it, and a fresh
    // instance per call would drop that between the two halves of the
    // fingerprint-confirmation flow.
    private val pairingClients = mutableMapOf<ConnectionSlot, PairingClient>()

    /** Unauthenticated -- POST /devices/pair takes no bearer token (see
     *  bridge/internal/relay/relay.go's handleDevicePair). */
    @Synchronized
    override fun pairingClient(slot: ConnectionSlot): PairingClient =
        pairingClients.getOrPut(slot) {
            PairingClient(
                OkHttpClient(),
                slot,
                settings,
                bindHost = ::host,
                onPairingStored = { hostId, baseUrl ->
                    hostRegistry.register(PairedHost(hostId, placeholderHostName(baseUrl), HostKind.CMUX))
                    hostRegistry.select(hostId)
                    // A pairing may have just delivered the Firebase config this
                    // process concluded at startup it did not have.
                    if (activatePush(appContext, settings, ::activeBridge)) pushActivations.value++
                },
            )
        }

    override fun eventsSocket(slot: ConnectionSlot): EventsSocket? = selectedHost().eventsSocket(slot)

    override fun terminalSocket(slot: ConnectionSlot, surfaceId: String): TerminalSocket? =
        selectedHost().terminalSocket(slot, surfaceId)

    override fun loadOrder(): List<String> = workspaceOrderStore.load(selectedHost().host)

    override fun saveOrder(order: List<String>) = workspaceOrderStore.save(selectedHost().host, order)

    override fun loadSortByAttention(): Boolean = workspaceOrderStore.loadSortByAttention()

    override fun saveSortByAttention(sortByAttention: Boolean) =
        workspaceOrderStore.saveSortByAttention(sortByAttention)

    override fun loadFontZoom(): Float = terminalDisplayStore.loadFontZoom()

    override fun saveFontZoom(zoom: Float) = terminalDisplayStore.saveFontZoom(zoom)

    override fun loadWheelScrolling(): Boolean = terminalDisplayStore.loadWheelScrolling()

    override fun saveWheelScrolling(enabled: Boolean) =
        terminalDisplayStore.saveWheelScrolling(enabled)

    override fun loadTerminalPollMs(metered: Boolean): Int =
        terminalDisplayStore.loadTerminalPollMs(metered)

    override fun saveTerminalPollMs(metered: Boolean, ms: Int) =
        terminalDisplayStore.saveTerminalPollMs(metered, ms)

    override fun relayHealth(): RelayHealth = selectedHost().relayHealth

    override fun connectionMonitor(): ConnectionMonitor = selectedHost().connectionMonitor

    override fun slotCredentials(): SlotCredentials = selectedHost().slotCredentials

    override fun slotCredentialHealth(): SlotCredentialHealth = selectedHost().slotCredentialHealth

    // Starts true: a process alive with no activity yet (a push waking a
    // service, tests) behaves like the old always-on app rather than a
    // permanently-backgrounded one whose subscriptions would never start.
    private val appForeground = MutableStateFlow(true)

    /** Read by every streaming subscription (see [BridgeGateway.appForeground]);
     *  written only by MainActivity's onStart/onStop -- single-activity app,
     *  so activity STARTED/STOPPED is exactly "user has the app in front of
     *  them". */
    override fun appForeground(): StateFlow<Boolean> = appForeground.asStateFlow()

    fun setAppForeground(value: Boolean) {
        appForeground.value = value
    }

    // Bumped every time a pairing brings push up in a process that started
    // without it -- the config now arrives at pairing, so startup can
    // legitimately conclude there is nothing to initialise and be wrong about
    // it minutes later.
    private val pushActivations = MutableStateFlow(0)

    /** Collected by MainActivity, which asks for POST_NOTIFICATIONS when this
     *  changes: the permission needs an Activity, and by the time push comes
     *  up on the pairing that delivered its config, the startup prompt has
     *  long since decided there was nothing to ask for. Without it the phone
     *  registers a token, the bridge starts sending, and every notification is
     *  dropped until the user happens to relaunch (cmux-app-snt).
     *
     *  A counter rather than a flag: pairing the second slot must reach a
     *  collector that already consumed the first. */
    fun pushActivations(): StateFlow<Int> = pushActivations.asStateFlow()

    /** The fallback-aware entry point most read/write call sites should use.
     *  Null only when the selected host has no slot paired yet (matches the
     *  old single-slot bridgeClient()'s null contract, so existing "Bridge
     *  not configured" call sites need no shape change). */
    override fun activeBridge(): FallbackBridgeClient? = selectedHost().takeIf { it.isConfigured() }?.bridge

    override fun anyBridgeConfigured(): Boolean = selectedHost().isConfigured()

    /** The paired session for [slot] on the selected host -- see
     *  [HostConnections.session]. */
    fun session(slot: ConnectionSlot): CryptoSession = selectedHost().session(slot)
}
