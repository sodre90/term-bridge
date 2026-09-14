---
type: article
description: Phased improvement guide (CI/tests, Android and Go structural cleanup, operability, UX, polish) produced by an architect-level review of the whole repo, ordered so each phase de-risks the next.
status: canonical
authored: 2026-07-21
author: sodre90
tags:
  - article
  - canonical
  - roadmap
  - improvement-guide
---
## Summary

A phased improvement guide for `cmux-app`, produced 2026-07-09 (at HEAD `98c5d66`) by an architect-level review of the whole repo (Android + Go), cross-checked against the [enhancement-audit](./enhancement-audit.md) material. All CONFIRMED bugs from that audit were already fixed by the time this guide was written; this is the *next* tier of work — structure, tests, operability, UX — not a bug list. Phases are ordered so each unlocks or de-risks the next; within a phase, items are ordered by impact × ease. Effort tags: **S** (small, single sitting), **M** (half-day-ish), **L** (multi-day / needs a design pass).

## Body

### Orientation

~4.9k lines of main Kotlin (45 files, 23 test files), ~13k lines of Go (46 prod files, 46 test files) as of the guide's writing — small and healthy, so the guide's standing instruction is to prefer surgical changes over sweeping rewrites. Each shipped feature has a spec + plan pair under `docs/superpowers/` (see [bridge-relay-architecture](./features/bridge-relay-architecture.md), [pairing-e2e-encryption](./features/pairing-e2e-encryption.md), [connectivity-tailscale-dual-pairing](./features/connectivity-tailscale-dual-pairing.md), [push-notifications](./features/push-notifications.md), [app-polish-and-hardening](./features/app-polish-and-hardening.md), and [android-terminal-foundation](./features/android-terminal-foundation.md) for the consolidated syntheses of those); the guide asks that any **L**-sized item here follow the same discipline — short design note first, reviewed, then implemented.

### Non-negotiable invariants

Violating any of these is a rejected change regardless of how nice the diff is:

1. **cmux is a black box.** Talk to it only via the documented CLI (`cmux rpc` / `cmux events`). Never copy cmux source. Never add create/close/restore-workspace capabilities to the bridge — it is deliberately limited to reads, terminal I/O, feed replies, and rename.
2. **`internal/relay/multitenant_test.go` must always pass** — it enforces the tenant-isolation security model, not just a test.
3. **Wire-format lockstep.** The app↔bridge protocol is hand-mirrored between Kotlin (`model/Dtos.kt`, `data/e2e/*`) and Go (`internal/server/*`, `internal/e2e/*`). Any field change must land on both sides in the same commit, with tests on both. The pairing DTOs exist in a third place too (`internal/relay/relay.go`) until the dedup in Phase 2 lands — until then, a pairing wire change means three synchronized edits.
4. **Never weaken the e2e crypto:** X25519+HKDF derivation, AEAD on every body and terminal frame, replay-protected counters (validate+commit is atomic — keep it that way; see [enhancement-audit](./enhancement-audit.md) for why this specific invariant was a real, found bug before it was fixed). The relay must stay blind to content.
5. **Never log secrets.** Typed terminal input, tokens, and key material must never reach logcat or the Go logs — this was a Critical audit finding, already fixed; don't regress it when touching logging.
6. **Commits:** one item per commit, authored solely by the human developer (no AI co-author trailers), message style matching the existing log (`android: …`, `bridge: …`, `docs: …`).

### How to work in this repo

Line numbers in the guide drift — locate code by symbol name (grep), not by cited line. One guide item = one branch/commit; never bundle a refactor with a behavior change. Where the guide says "consider" or "options," propose a choice in the PR description with a sentence of rationale rather than silently picking. Don't act on anything in the non-goals section below — those were investigated and refuted. The existing comment discipline (wire-contract doc comments, "why" notes on tricky code) is deliberately high and should be preserved, but don't add narrating comments — express intent through naming.

### Phase 0 — Safety net (do first; protects everything after)

- **CI pipeline (S, highest single-value item in the repo).** No CI exists (no `.github/`); the security model leans on tests that today only run locally. Add a workflow with a `bridge` job (`go build ./... && go vet ./... && go test ./...`, Go 1.26, no network/real cmux needed) and an `android` job (`./gradlew :app:assembleDebug :app:testDebugUnitTest`, JDK 17), triggered on push to `main` and on PRs.
- **Linters (S, separate commit).** Go: `golangci-lint` with a minimal config (defaults + `errcheck`, `staticcheck`). Kotlin: one of ktlint or detekt (detekt with default/formatting-only rules recommended — the codebase is already clean, avoid opinionated rules that churn every file). Wire both into CI.
- **Dependabot (S).** `.github/dependabot.yml` covering `gomod` (`/bridge`), `gradle` (`/android`), and `github-actions`; weekly, grouped minor/patch updates.
- **Repo-root `CLAUDE.md` (S).** A short file (<60 lines): build/verify commands, the invariants above, the wire-lockstep rule, and pointers to this guide and to `docs/superpowers/` conventions — makes every future agent session safer.

### Phase 1 — Android structural (a dependency chain; do in order)

The transport/crypto layer (`BridgeClient`, `FallbackBridgeClient`, `TerminalSocket`/`EventsSocket`, `E2eInterceptor`, `data/e2e/*`) is solid and well-tested — leave it alone except where named. All the accumulated debt is in the ViewModel layer, and it chains: breaking the god-dependency unlocks extracting the reconnect state machine and delivery tracker, which unlock the ViewModel test suite.

- **Break the `AppContainer` god-dependency (M, keystone).** Every ViewModel takes the whole concrete `AppContainer`, whose init does EncryptedSharedPreferences + Keystore I/O — so no ViewModel can be instantiated in a JVM unit test today, which is why the app's highest-risk logic (reconnect FSMs, input delivery tracking) has zero tests. Define narrow interfaces for what VMs actually consume (roughly a `BridgeGateway` and a `PairingGateway`); `AppContainer` implements them, VMs take the interface. Pure refactor, no behavior change.
- **Extract the triplicated reconnect state machine (M).** The same relay-penalty/slot-fallback/exponential-backoff loop is copy-pasted across three ViewModels and has already drifted (only one has the `consecutiveRelayDrops` → prefer-DIRECT refinement). Extract one reusable reconnector; also unify the relay-health knowledge that `FallbackBridgeClient` separately re-implements, into one process-wide object so "relay is down" is learned once.
- **Extract the terminal delivery/ack tracker (M).** `TerminalViewModel` inlines a ~200-line delivery subsystem (`nextSeq`, `neverSentQueue`, `pendingAcks`, `inFlightInputSeq`, a poll loop, and the never-double-send guarantee for non-idempotent input) — the most safety-critical logic in the app, welded into an untestable class. Extract a plain class taking a `send` lambda and an injectable clock.
- **ViewModel test suite (M, after the above).** Cover `SessionsViewModel`'s fetch-dedup/debounce logic, and the one-shot legacy-settings migration flow (factor it to take injectable read/write callbacks and test with in-memory maps — a bug here would silently strand an upgrading user with no e2e session).
- **Consistency batch (S each, independent, parallelizable).** Unify the four different UiState modeling patterns onto one sealed-per-screen `StateFlow` shape; move the 409 `not_paired` retry (handled ad hoc in one ViewModel) into `FallbackBridgeClient` so every read path inherits it; log dropped/undecryptable frames in `EventsSocket` instead of silently swallowing them (mirroring `TerminalSocket`); rename the misleadingly-named `silentRefresh()` (it shows the spinner) to `pullRefresh()`/`userRefresh()`.

### Phase 2 — Go structural

- **Unify HTTP error shape + one JSON writer (S).** Most handlers emit `{"error":"..."}`, but the WS routes use plain-text `http.Error` for the same logical condition, forcing the app to parse two formats for one error. Consolidate onto one shared writer; verify Android tolerates the now-JSON WS-route errors before merging (wire-lockstep rule).
- **Extract `internal/wire` and dedupe the pairing handlers (M).** The pairing wire contract (6 DTO types + 4 handlers) exists verbatim twice in Go (relay and direct-mode server) plus once more in Kotlin — three copies of a security-sensitive contract. Create a shared `wire` package for app-facing types, and a pairing-handler factory parameterized by a tenant-resolver function so relay and direct mode share one implementation. Also stops the relay binary from dragging in the whole agent-side `server` package just for one type.
- **Rework `e2e.Store` persistence (M/L, highest correctness payoff).** `internal/e2e/store.go` reloads and rewrites the entire JSON file inside one mutex on every counter bump or device add — i.e. per terminal frame — and the CLI (`pair-device`) and the running agent open the same file from separate processes with no flock, so last-writer-wins can clobber a counter commit (a real replay-window/nonce-reuse risk, independently confirmed in [enhancement-audit](./enhancement-audit.md)) or drop a freshly-paired device. Preferred direction: migrate to SQLite reusing `auth.Store`'s already-shipped pattern (the design work for this is covered in [app-polish-and-hardening](./features/app-polish-and-hardening.md)); alternatives are debounced-JSON+flock or routing pairing writes through the running agent. Also: stop swallowing load errors silently — log loudly and quarantine a corrupt file instead of silently disabling e2e/push for all devices.
- **Small correctness/hygiene batch (S each).** Context-aware backoff sleeps in the three loops still using bare `time.Sleep` (SIGTERM can hang up to the max backoff); drop the dead, unread `cfg` field on `server.Server`; fix the relay's systemd `StateDirectory` hardening vs. its actual config-file default path mismatch; add `CMUX_RELAY_*` env overrides for secrets/listen-addr only (not a full config-from-env system); stop `auth.Store` from collapsing "not found" and "DB error" into one bool where the distinction actually drives an HTTP response.
- **Fast-path socket pool (M, after the hygiene batch; measure first).** All cmux RPC funnels through one `socketConn` whose mutex is held for the whole round-trip, so a slow replay blocks an unrelated input ack. The existing idempotency (`committed` flag) logic already makes multiple connections safe — add a small pool (2–4 conns).
- **Missing integration tests (M).** Nothing exercises an agent redialing while a device request is in flight; the fast-path-to-subprocess fallback has unit tests only; the relay's `devices`/`tenants` CLI commands (security-relevant) have zero tests.

### Phase 3 — Operability (prerequisite for any multi-tenant growth)

- **Structured logging with `slog` (M).** Zero `log/slog` today — 11 files of plain `log.Printf` with inconsistent fields. Adopt one shared handler with consistent keys (`tenant_id`, hashed `device`, `route`, `status`, `dur_ms`); never log body content (invariant above).
- **Metrics (M).** No metrics at all — there's no way to answer "which tenant is hot or erroring." Cheapest fit is `expvar` on the existing loopback listener: active-tunnels gauge, per-tenant proxied requests + `agent_offline` count, pairing issued/redeemed/expired, push sent/failed, e2e decrypt failures.
- **Agent status surface (S/M).** The host agent has no health surface today — add a `term-bridge status` subcommand and optionally deepen the relay's `/healthz` to ping its store.
- **Rate-limit `/devices/pair` (S).** The tenant-registration bootstrap is already per-IP throttled; the pairing-redeem endpoint still relies solely on single-use codes + a 10-minute TTL. Cheap defense-in-depth on an internet-reachable endpoint — reuse the existing rate limiter.

### Phase 4 — UX (product-level, Android-only unless noted; ordered by pain × ease)

Overflow menu on workspace cards for the long-press-only rename/YOLO actions (currently undiscoverable); distinct, confirm-gated styling for the YOLO `Bypass` mode since it mirrors `--dangerously-skip-permissions`; an aggregate unread badge on the Inbox button; persisting the "Waiting first" sort toggle; Inbox rows navigating to their originating terminal instead of only replying; a latching Ctrl modifier chip (today's key bar hardcodes only ^C/^D/^Z); a "send test notification" button for push setup debugging; a compact autopilot-mode summary banner; an accessibility pass (D-pad/key bar/badges currently have no `contentDescription`/TalkBack semantics); and, lowest priority, search/filter over workspaces once session counts grow.

### Phase 5 — Polish backlog (batchable, no ordering)

Compose `@Preview`s for the pure leaf composables (none exist); move ~42 hardcoded UI strings into `strings.xml`; promote a couple of hex-literal accent colors to theme-level semantic colors; gate the keystroke-diff debug log behind `BuildConfig.DEBUG`; resolve the `Session`-class/cmux-"sessions"/`/sessions`-endpoint/`Workspace`-model naming overload; measure before optimizing `RenderGridView`'s scrollback rendering (a stable, line-content-based `remember` key is the cheapest real win; only attempt `LazyColumn` with a working text-selection story); write (design-only, don't implement yet) the `security-crypto` migration note covering in-place data migration for existing installs; a routine dependency refresh (AGP/Kotlin/Compose BOM/OkHttp are a late-2024 cohort) once CI exists; start a `CHANGELOG.md` now that the repo is public; and a design-only API-versioning note (additive-only rule + optional version header, anchored to the Phase 2 `wire` package) — don't implement negotiation machinery until it's actually needed.

### Explicit non-goals — do NOT do these

Investigated and rejected, per [enhancement-audit](./enhancement-audit.md)'s validation pass: any REFUTED audit finding (the RenderGrid wide-char handling, the `Registry.Set` "race", the "rootful podman" claim — all already correct as written); the OkHttp bearer-on-redirect "leak" (OkHttp 4.x already strips `Authorization` on cross-host redirects); TLS `MinVersion`/cipher hardening in Go's `serveDirect` (Go's defaults already exclude sub-TLS1.2); rewriting `TerminalInputDiff` to a cursor-aware edit script (the prefix-only diff is documented, correct, and low value to replace); terminal viewport "reflow" work in this repo (the phone already sends cols/rows and the bridge forwards them to cmux — any remaining sizing weirdness lives in cmux's own backend, outside this repo); and adding cmux workspace create/close/restore, ever.

### Known parked issues (context, not tasks)

IPv6 flakiness on the relay path, parked deliberately — don't drive-by-fix it while touching transport code. A possible dual-path duplicate push (relay pushmon + the later agent-native path from [push-notifications](./features/push-notifications.md)) is unconfirmed — if touching push code, add a dedup assertion or verify live rather than assuming. Multi-tenant sub-projects 2–4 (self-service onboarding/abuse resistance, rate limiting/quotas, audit/retention/ops — see [bridge-relay-architecture](./features/bridge-relay-architecture.md)) are open by design; Phase 3 and the pairing rate-limit item above are their prerequisites, not their replacement.

## References

- [docs/improvement-guide.md](../docs/improvement-guide.md)
