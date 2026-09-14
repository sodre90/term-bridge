---
type: article
description: Agent-native FCM push for direct (Tailscale)-only phones, bypassing the relay — consolidated from the original design/plan docs.
status: canonical
authored: 2026-07-21
author: sodre90
tags:
  - article
  - canonical
  - push-notifications
---
## Summary

> **Since 2026-09-14** the agent also fronts tmux on a Linux host and the app pairs with several hosts; where this write-up says "the Mac" or "the Mac agent", read "the host" -- see [overview](../overview.md) for the current shape.

An agent-native FCM push path for phones paired only via direct (Tailscale) mode. It exists because push notifications ("agent needs your attention") were previously delivered exclusively through the relay, so any phone paired without a relay connection got zero push, permanently, by design. This adds a second, independent push path running directly on the Mac agent, bypassing the relay entirely for direct-mode phones. See [overview](../overview.md) for the shipped push feature and [connectivity-tailscale-dual-pairing](./connectivity-tailscale-dual-pairing.md) for the direct-mode transport this depends on.

## Body

### Motivation

The pre-existing, already-shipped push architecture is entirely relay-owned: the relay's push monitor opens its own subscription to the agent's `/events` over the relay tunnel and fans blocking prompts out to FCM, using tokens registered via a relay-only `POST /devices/register`. Direct (Tailscale) mode had no equivalent — the agent's shared route table explicitly did not mount `/devices/register`, with a comment stating device registration is handled exclusively by the relay. A prior dual-pairing project had, for exactly this reason, hard-pinned Android's push registration to the `RELAY` connection slot. Net effect: a phone paired only to direct mode got zero push, permanently — not a bug, but an acknowledged gap. The user explicitly raised it: "maybe we can move the notification part to the bridge side, can't we?" — the direct trigger for this work.

### Design decisions

**Additive, not a migration** — the central decision. The relay's existing push path is left completely untouched; a second, fully independent push path is added on the agent itself, scoped only to devices in the agent's own local device store (i.e. direct-mode pairs). This was one of three options presented to the user (full move of push to the agent; relay-forwards-tokens-to-agent; or this additive approach); the clarifying question timed out with no response, so — per this project's standing proceed-on-timeout-and-record-the-assumption rule — the lowest-risk, clearly-recommended option was chosen unilaterally, flagged explicitly in the spec for later confirmation.

Two device registries stay fully separate: the relay and the agent each keep their own independent `auth.Store` (SQLite `devices` table). A relay-paired phone's device row lives only in the relay's store; a direct-paired phone's device row lives only in the agent's own local store. These never merge.

The existing single canonical event classifier (`ingestEvents`, already calling `maybeAutoResolve` on every `NeedsAttention` frame for YOLO mode) gets a second call, `maybeSendPush`, at the same hook point.

**Payload sent to FCM (content-privacy relevant).** For each `NeedsAttention` frame, the agent sends one FCM data message per registered token with a fixed title ("Agent needs your attention"), a body containing the frame's `Title` (falling back to `Kind`) — i.e. the actual prompt/permission-request text, in plaintext to Google's FCM service — and a data map (`type=attention`, `feed_id`, `workspace_id`, `surface_id`, `kind`). Neither source document discusses encrypting or redacting this payload before it leaves the Mac; this is a direct Mac-to-Google send, bypassing the relay entirely. The docs frame this as desirable *because* the relay is bypassed, but do not otherwise address that prompt-title content now transits Google's infrastructure directly, distinct from the app↔bridge encrypted channel used elsewhere in this project (see [pairing-e2e-encryption](./pairing-e2e-encryption.md)). This is a documented design characteristic in the source docs, not a gap flagged for remediation there.

**Relay/bridge/app coordination.** The same Firebase project is configured twice: the agent gets its own `fcm_project_id`/`fcm_credentials` TOML config (identical key names to the relay's), same Firebase project, same or re-downloaded service-account key — an operational duplication, not a code-sharing mechanism. Android's push registration moves off the hard `RELAY` pin onto the already-existing fallback-aware `activeBridge()` client, since after this change both slots can genuinely accept a registration. No de-duplication logic exists for a dual-paired phone; a rare duplicate notification is accepted as harmless.

**Security/auth constraints.** Registration writes must use the caller's raw bearer token, never `X-Device-ID` (already a hash) — the store hashes its argument internally, so passing an already-hashed value would double-hash and silently match zero rows. `POST /devices/register` is mounted only on `DirectHandler()`, never on the shared route set used by both trusted and direct handlers, and never on the relay-tunneled `TrustedHandler()` — because that path only does relay-token validation, with no real per-device bearer check at the agent. The pre-existing test asserting `/devices/register` returns 404 in trusted mode must keep passing unmodified. Everything is off by default: empty FCM config fields leave direct mode behaving exactly as before this feature, no error, no behavior change.

### Implementation notes

**Config** (`bridge/internal/config/agent.go`): new `FCMProjectID`/`FCMCredentials` fields, the latter home-expanded like other path fields.

**Server-side plumbing** (`bridge/internal/server/`): a `Pusher` interface (`Send(ctx, fcmToken, title, body, data) error`), satisfied by the existing FCM HTTP v1 client also used by the relay, duck-typed independently since the two packages have no other reason to share a type. `Server` gets `pusher`/`directTenantID` fields plus a post-construction `SetPusher` setter, deliberately not a constructor parameter, to avoid touching the existing test call sites. `maybeSendPush` no-ops if pusher or store is nil, reads tenant-scoped FCM tokens, no-ops if empty, sends with a 10-second timeout, logs (doesn't fail) per-token send errors. `handleRegisterDevice` decodes the token, 503 if no store, 400 if missing token, 401 if the store rejects the bearer, 200 otherwise. Mounted only on `DirectHandler()`.

**Android registration.** `FallbackBridgeClient` gains `registerDevice(fcmToken)`, wrapping the already-existing, transport-agnostic `BridgeClient.registerDevice` — previously deliberately not exposed on the fallback wrapper. Two call sites (`CmuxMessagingService.onNewToken`, `MainActivity.registerFcmToken`) switch from a hard `RELAY` slot pin to `activeBridge()`.

**Agent wiring.** In `runAgent`, the pusher is constructed the same way the relay already does, guarded by `DirectListen != "" && FCMProjectID != "" && FCMCredentials != ""`. Construction failure logs and leaves push disabled for that run, non-fatal.

**Testing approach.** Go config tests (defaults, TOML parsing, home-expansion); `maybeSendPush` tests via a fake in-memory `Pusher` spy covering no-op-without-pusher, no-op-without-store, correct per-token sends, zero-tokens no-op, and a dedicated test asserting push is scoped strictly to `directTenantID` with no cross-tenant leak. HTTP tests for `handleRegisterDevice` via `httptest` covering the 200/400/401 cases and confirming 404 on the trusted handler. `runAgent` wiring itself has no new automated test — verified by build/vet/test-suite-green plus manual inspection. Android `FallbackBridgeClientTest` gets two new MockWebServer cases mirroring the existing primary/fallback pattern. The Android call-site swap has no dedicated test coverage (no Robolectric); verified by compilation plus a grep confirming the old `ConnectionSlot` import is fully removed. A manual end-to-end step is specified as final verification: real FCM config, direct-mode-only phone, trigger a real blocking prompt, confirm the notification arrives with the relay connection killed (proving no relay involvement), confirm a relay-only phone's push still works unchanged.

### Status

Based strictly on markers in the two source documents: the plan lists all six tasks (agent FCM config; `maybeSendPush`; the new registration route; `runAgent` wiring; Android `registerDevice`; the Android call-site swap) plus a final-verification section, and every step in every task is an unchecked checkbox in the document as read. The plan states its working branch, `tailscale-direct-transport`, was **not yet merged to `main`** at the time of writing — direct mode itself, which this entire project depends on, lived there. The spec's central architectural decision (the additive approach) is explicitly marked as made under a timeout/assumption rule rather than user-confirmed, with the document itself instructing the reader to confirm or redirect on review. Both documents describe the pre-existing relay-based push path as already-confirmed-from-code, working, in-daily-use background context; that part is not something these two documents propose or claim to have built. Neither document contains a "shipped" or later-dated status update; both are dated 2026-07-04 and read as a plan/spec pair awaiting execution and confirmation as of that date.

## References

- [docs/superpowers/plans/2026-07-04-agent-native-push-notifications-plan.md](../../docs/superpowers/plans/2026-07-04-agent-native-push-notifications-plan.md)
- [docs/superpowers/specs/2026-07-04-agent-native-push-notifications-design.md](../../docs/superpowers/specs/2026-07-04-agent-native-push-notifications-design.md)
