// Package metrics holds the small set of process-wide expvar counters and
// gauges the relay and agent binaries export at their natural choke points
// (see docs/improvement-guide.md §6.2). It is deliberately not a general
// metrics framework: add a new var here only when a real choke point needs
// one, and increment it right at that choke point rather than threading
// metrics-specific parameters through unrelated call chains.
//
// Every var here is registered exactly once, at package init, via the
// standard library's expvar.NewInt/expvar.NewMap. expvar's registry is
// process-global, so both binaries carry every var; only the ones a given
// binary actually increments ever leave zero.
//
// The relay serves them live at /debug/vars (relay.Handler, behind the edge
// token). The agent has no such route and deliberately gains no new listening
// surface for one: it reports [Snapshot] in its periodic status file instead,
// which `term-bridge status` prints (cmux-app-9aa).
package metrics

import "expvar"

var (
	// TunnelsActive is a gauge: how many tenant tunnel sessions are
	// currently registered in relay.Registry. Set (not Add'd) on every
	// registry mutation so it always reflects the live count, never drifts
	// from missed decrements.
	TunnelsActive = expvar.NewInt("tunnels_active")

	// ProxyRequestsTotal counts every proxied app request that reaches
	// relay's reverse proxy, keyed by tenant ID, regardless of outcome.
	ProxyRequestsTotal = expvar.NewMap("proxy_requests_total")

	// ProxyAgentOfflineTotal counts, per tenant ID, proxied requests that
	// failed because that tenant had no active agent tunnel.
	ProxyAgentOfflineTotal = expvar.NewMap("proxy_agent_offline_total")

	// PairingCodesIssuedTotal counts pairing codes successfully minted via
	// POST /agent/pairing-code.
	PairingCodesIssuedTotal = expvar.NewInt("pairing_codes_issued_total")

	// PairingCodesRedeemedTotal counts pairing codes successfully redeemed
	// via POST /devices/pair.
	PairingCodesRedeemedTotal = expvar.NewInt("pairing_codes_redeemed_total")

	// PairingCodesExpiredTotal counts redemption/info-lookup attempts
	// against a pairing code whose TTL had already elapsed, distinct from
	// not-found or already-redeemed (see auth.Store.RedeemPairingCode and
	// PairingCodeInfo, the two places that actually detect expiry).
	PairingCodesExpiredTotal = expvar.NewInt("pairing_codes_expired_total")

	// PushSentTotal and PushFailedTotal are running totals of individual FCM
	// send attempts across all tenants (relay/pushmon.go's fanout already
	// computes sent/failed per call; these accumulate them).
	PushSentTotal   = expvar.NewInt("push_sent_total")
	PushFailedTotal = expvar.NewInt("push_failed_total")

	// PushTokensDroppedTotal counts FCM registration tokens dropped because
	// FCM reported them UNREGISTERED. Separate from PushFailedTotal, which a
	// relay outage inflates too: this one only moves when a device has
	// genuinely stopped being reachable and needs to re-register, which is
	// the state that used to be invisible (cmux-app-6u7).
	PushTokensDroppedTotal = expvar.NewInt("push_tokens_dropped_total")

	// E2EDecryptFailuresTotal counts failed e2e decrypt attempts on the
	// agent, keyed by call site ("terminal_frame", "body").
	E2EDecryptFailuresTotal = expvar.NewMap("e2e_decrypt_failures_total")

	// TerminalReplayFailuresTotal counts mobile.terminal.replay calls that
	// failed while a terminal socket was open. Since such a failure no longer
	// closes the socket (cmux-app-8a0), and the log deliberately reports one
	// line per outage rather than one per attempt, this counter is what keeps
	// the volume measurable.
	TerminalReplayFailuresTotal = expvar.NewInt("terminal_replay_failures_total")

	// TerminalReplayGaveUpTotal counts terminal sockets closed because cmux
	// answered no replay for the whole grace window. The ratio against
	// TerminalReplayFailuresTotal is the one that matters: many failures and
	// few give-ups is the fix working, and the two rising together means the
	// grace is too short for the outages actually being seen.
	TerminalReplayGaveUpTotal = expvar.NewInt("terminal_replay_gave_up_total")
)

// Snapshot reports the current value of every counter and gauge in the
// process's expvar registry, flattened: a map var contributes one entry per
// key, as "name/key".
//
// It reads the registry rather than a list of the vars above on purpose. A
// second list is a second thing to forget, and forgetting is precisely how
// every one of these ended up unreadable on the agent for as long as it did
// (cmux-app-9aa).
//
// Only *expvar.Int and *expvar.Map are collected, which is exactly this
// package's own vars: the runtime's own "cmdline" and "memstats" are Funcs,
// and neither belongs in an operator's health snapshot.
func Snapshot() map[string]int64 {
	out := make(map[string]int64)
	expvar.Do(func(kv expvar.KeyValue) {
		switch v := kv.Value.(type) {
		case *expvar.Int:
			out[kv.Key] = v.Value()
		case *expvar.Map:
			v.Do(func(entry expvar.KeyValue) {
				if n, ok := entry.Value.(*expvar.Int); ok {
					out[kv.Key+"/"+entry.Key] = n.Value()
				}
			})
		}
	})
	return out
}
