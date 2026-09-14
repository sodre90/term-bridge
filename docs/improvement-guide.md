# term-bridge improvement guide

**Audience:** an implementing AI model (Sonnet 5) working in this repo with the
human developer reviewing.
**Produced:** 2026-07-09, at HEAD `98c5d66`, by an architect-level review of the
whole repo (Android + Go), cross-checked against `docs/enhancement-audit.md`,
`docs/enhancement-audit-validation.md`, and `docs/improvement-ideas.md`. All
CONFIRMED bugs from that audit are already fixed (`a16868c`, `fd8e6c4`) — do not
re-fix them. This guide is the *next* tier: structure, tests, operability, UX.

Work top to bottom: phases are ordered so each unlocks or de-risks the next.
Within a phase, items are ordered by impact × ease. Effort tags: **S** (small,
single sitting), **M** (half-day-ish), **L** (multi-day / needs a design pass).

---

## 0. Orientation

- **What this is:** an Android (Kotlin/Compose) phone client + two Go binaries
  (`term-bridge-relay` home-server daemon, `term-bridge agent` on each host)
  that together let a phone drive terminal agent sessions remotely. A host is
  a Mac running cmux or a Linux box running tmux; the agent fronts either
  behind `internal/host.Host` (`cmuxhost`, `tmuxhost`; on tmux the agent
  feed comes from Claude Code's hooks via `internal/host/agentfeed` and the
  `term-bridge hook` subcommand), and the app pairs
  with several hosts and switches between them (`HostRegistry`,
  `HostConnections`, everything stored per `HostId`). Read the root
  `README.md` first — the architecture diagram and security model there are
  accurate and current; the host seam and multi-host design are in
  `docs/superpowers/specs/2026-09-14-linux-tmux-host-design.md`.
- **Scale:** ~4.9k lines main Kotlin (45 files, 23 test files), ~13k lines Go
  (46 prod files, 46 test files). Small and healthy — prefer surgical changes,
  never sweeping rewrites.
- **Build & verify (run before every commit):**
  ```bash
  cd bridge && go build ./... && go vet ./... && go test ./...
  cd android && ./gradlew :app:assembleDebug :app:testDebugUnitTest
  ```
- **Prior art:** each shipped feature has a spec + plan pair under
  `docs/superpowers/`. Match that discipline for anything L-sized: write the
  short design note first, get it reviewed, then implement.

## 1. Non-negotiable invariants

Violating any of these is a rejected change, regardless of how nice the diff is.

1. **cmux is a black box.** Talk to it only via the documented CLI
   (`cmux rpc` / `cmux events`). Never copy cmux source. Workspace and pane
   creation, selection and closing arrived 2026-09-12 (see
   `docs/superpowers/specs/2026-09-12-workspace-layout-control-design.md`);
   every such call names its target by UUID and never takes focus on the
   Mac unless the phone asked for it. **tmux is a black box the same way:**
   the `tmux` CLI and control mode only (`internal/tmux`), every command
   targeting a `$n`/`%n` id, never the current window/pane; probe only in
   the scratch session `cmux-app-scratch` on the home server.
2. **`internal/relay/multitenant_test.go` must always pass.** It is the
   enforcement of the tenant-isolation security model, not just a test.
3. **Wire-format lockstep.** The app↔bridge protocol is hand-mirrored between
   Kotlin (`model/Dtos.kt`, `data/e2e/*`) and Go (`internal/server/*`,
   `internal/e2e/*`). Any field change must land on both sides in the same
   commit, with tests on both sides. The pairing DTOs currently exist in
   *three* places (see item 5.2) — until that dedup lands, a pairing wire
   change means three synchronized edits.
4. **Never weaken the e2e crypto:** X25519+HKDF derivation, AEAD on every body
   and terminal frame, replay-protected counters (the validate+commit is
   atomic now — keep it that way). The relay must stay blind to content.
5. **Never log secrets.** Typed terminal input, tokens, and key material must
   not reach logcat or the Go logs. (This was audit Critical #1/#2 — already
   fixed; do not regress it when touching logging.)
6. **Commits:** one item per commit, authored solely by the human developer
   (no `Co-Authored-By` AI trailers), message style matching the existing log
   (`android: …`, `bridge: …`, `docs: …`).

## 2. How to work in this repo

- **Line numbers in this doc will drift.** Locate code by symbol name
  (grep), not by the cited line; citations are anchors, not gospel.
- **One guide item = one branch/commit.** Do not bundle a refactor with a
  behavior change.
- **When a direction below says "consider" or "options,"** propose your choice
  in the PR description with a sentence of rationale rather than silently
  picking.
- **Do not act on anything in §9 (explicit non-goals)** — several
  plausible-looking "fixes" were investigated and refuted.
- The existing comment discipline (wire-contract doc comments, "why" notes on
  tricky code) is deliberately high — preserve it. But don't add narrating
  comments; express intent through naming.

---

## 3. Phase 0 — Safety net (do these first, they protect everything after)

### 3.1 CI pipeline — **S**, highest single-value item in the repo
There is no CI at all (no `.github/`). The security model leans on tests that
only run locally today.
- Add `.github/workflows/ci.yml` with two jobs:
  - **bridge:** `go build ./... && go vet ./... && go test ./...` (Go 1.26,
    working-directory `bridge/`). Tests need no network and no real cmux.
  - **android:** `./gradlew :app:assembleDebug :app:testDebugUnitTest`
    (JDK 17, `gradle/actions/setup-gradle` for caching).
- Trigger on push to `main` and on PRs.
- **Accept:** a PR that breaks either test suite shows red on GitHub.

### 3.2 Linters — **S** (separate commit from 3.1)
- Go: `golangci-lint` with a minimal `.golangci.yml` (defaults + `errcheck`,
  `staticcheck`); fix or `//nolint` with justification anything it flags.
- Kotlin: pick **one** of ktlint or detekt (recommend detekt with default
  rules, formatting only — this codebase is clean, don't turn on opinionated
  style rules that churn every file).
- Wire both into the CI workflow.
- **Accept:** lint runs in CI; zero findings at HEAD.

### 3.3 Dependabot — **S**
`.github/dependabot.yml` covering `gomod` (`/bridge`), `gradle` (`/android`),
and `github-actions`. Weekly, grouped minor/patch updates.

### 3.4 Repo-root `CLAUDE.md` — **S**
There is none. Write a short one (< 60 lines): build/verify commands, the §1
invariants, the wire-lockstep rule, pointer to this guide and to
`docs/superpowers/` conventions. This makes every future agent session safer.

---

## 4. Phase 1 — Android structural (a dependency chain; do in order)

The transport/crypto layer (`BridgeClient`, `FallbackBridgeClient`,
`TerminalSocket`/`EventsSocket`, `E2eInterceptor`, `data/e2e/*`) is solid and
well-tested — **leave it alone** except where named. All accumulated debt is in
the ViewModel layer, and it chains: 4.1 unlocks 4.2/4.3, which unlock 4.4.

### 4.1 Break the `AppContainer` god-dependency — **M** (keystone)
Every ViewModel takes the whole concrete `AppContainer`
(`TerminalViewModel.kt`, `SessionsViewModel.kt`, `InboxViewModel.kt`,
`PairingViewModel.kt`; constructed in `CmuxNavHost.kt`). `AppContainer`'s init
does EncryptedSharedPreferences + Keystore I/O, so **no ViewModel can be
instantiated in a JVM unit test** — which is why the app's highest-risk logic
(reconnect FSMs, input delivery tracking) has zero tests.
- Define narrow interfaces for what VMs actually consume — roughly a
  `BridgeGateway` (`activeBridge()`, `eventsSocket(slot)`,
  `terminalSocket(slot, id)`, `anyBridgeConfigured()`) and a
  `PairingGateway`. `AppContainer` implements them; VMs take the interface.
- Pure refactor: no behavior change, no test deleted.
- **Accept:** each ViewModel is constructible in a plain JVM test with a fake
  gateway; all existing tests still pass.

### 4.2 Extract the triplicated reconnect state machine — **M**
The same relay-penalty/slot-fallback/exponential-backoff loop is copy-pasted in
`TerminalViewModel` (~lines 164–238), `SessionsViewModel` (~166–200), and
`InboxViewModel` (~54–88), with `INITIAL_BACKOFF_MS`/`MAX_BACKOFF_MS`/
`RELAY_PENALTY_MS` defined three times. They have **already drifted**: only
TerminalViewModel has the `consecutiveRelayDrops` → prefer-DIRECT refinement.
- Extract one reusable `SocketReconnector` (or a
  `reconnectingStream(openSocket: (ConnectionSlot) -> Flow<T>?): Flow<T>`
  helper) owning slot selection, penalty window, backoff, and cancellation.
  Give all three VMs the TerminalViewModel variant's behavior (it is the most
  evolved).
- Also unify the *shared* relay-health knowledge: `FallbackBridgeClient`
  independently implements the same relay→direct failover + 30s penalty for
  REST. Host the penalty state in one process-wide `RelayHealth` object both
  consult, so "relay is down" is learned once, not per-VM.
- **Accept:** constants exist once; one implementation; JVM tests cover
  penalty window, slot flip, backoff cap, and the relay-drops threshold.

### 4.3 Extract the terminal delivery/ack tracker — **M**
`TerminalViewModel` (~lines 100–312) inlines a ~200-line delivery subsystem:
`nextSeq`, `neverSentQueue`, `pendingAcks`, `inFlightInputSeq`,
`pendingOutbound`, a 500 ms poll, and the deliberate never-double-send
guarantee for non-idempotent input. This is the most safety-critical logic in
the app and it is welded into an untestable class.
- Extract a plain class (e.g. `OutboundInputQueue` / `DeliveryTracker`) taking
  a `send: (TerminalUp) -> Boolean` lambda and an injectable clock.
  TerminalViewModel shrinks to wiring.
- **Accept:** JVM tests prove: input is never sent twice; disconnect with
  pending acks sets the lost-input notice; `recomputeDeliveryStatus`
  transitions match current behavior. Behavior identical from the UI.

### 4.4 ViewModel test suite — **M** (after 4.1–4.3)
Beyond the tests named in 4.2/4.3, add:
- `SessionsViewModel`: `fetchInFlight` dedup across
  `silentRefresh`/`autoRefresh`/`refresh`, and the debounced `refreshRequests`
  coalescing.
- The one-shot migration flow (`Settings.migrateLegacyIfNeeded` +
  `Session.absorbLegacyIfTarget`, coordinated in `AppContainer.init`) — factor
  it to take injectable read/write callbacks (copy the pattern
  `PairingClient.pairInternal` already uses) and test with in-memory maps. A
  bug here silently strands an upgrading user with no e2e session.
- **Accept:** the previously-uncovered stateful logic has meaningful JVM
  tests; coverage of `ui/` no longer consists solely of pure-function tests.

### 4.5 Consistency batch — **S each** (independent, safe to parallelize)
- **Unify UiState modeling:** four patterns exist today (shared `UiState`
  sealed interface; TerminalViewModel's implicit `grid==null && error==null`
  loading; InboxViewModel's separate `_items`+`_error` flows with no loading
  state; PairingViewModel's Compose `mutableStateOf`). Standardize on sealed
  per-screen state exposed as `StateFlow` (PairingViewModel's sealed interface
  is the best model to copy — but move it to `StateFlow`).
- **Unify 409 `not_paired` retry:** `SessionsViewModel.fetchSessionsWithPairingRetry`
  handles the post-pairing race; `InboxViewModel.refresh()` and
  `TerminalViewModel.loadYoloMode()` hit the same race with no retry. Move the
  retry into `FallbackBridgeClient` (or a thin wrapper) so every read path
  inherits it.
- **Log dropped frames in `EventsSocket`** (`.getOrNull()` currently swallows
  decode/decrypt failures silently — a counter desync would look like "the
  list stopped updating"). Mirror `TerminalSocket`'s warn-log. Leave the push
  decrypt fallback in `CmuxMessagingService` silent — that one is intentional.
- **Rename `silentRefresh()`** — it *shows* the spinner; the silent one is
  `autoRefresh()`. Rename to `pullRefresh()` or `userRefresh()`.

---

## 5. Phase 2 — Go structural

### 5.1 Unify HTTP error shape + one JSON writer — **S**
Most handlers emit `{"error":"..."}`, but `terminal.go` (~44/56/60) and
`events.go` (~255) use `http.Error` plain text — the *same* logical error
(`not_paired`) is JSON from `encryptionMiddleware` and plain text from the WS
routes, so the app must parse two formats for one condition. There are also
four near-identical JSON writers (`relay/proxy.go writeJSONErr`,
`server/encryption.go writeEncryptionErr`,
`server/direct_pairing.go writeDirectPairingErr`, `server/sessions.go
writeJSON`).
- One small shared helper package (`internal/httpjson` or fold into 5.2's
  `wire`); route every error response through it.
- Check the Android side tolerates the (now-JSON) WS-route errors before
  merging — wire-lockstep rule.
- **Accept:** grep shows no `http.Error(` in prod handlers; one writer.

### 5.2 Extract `internal/wire` and dedupe the pairing handlers — **M**
Two coupled problems:
- `internal/relay/pushmon.go` imports the entire agent-side `server` package
  just for `EventFrame` — the relay binary drags in `cmux`, `e2e`, `yolo`.
- The pairing wire contract (6 DTO types + 4 handlers) exists **verbatim
  twice**: `internal/relay/relay.go` (~271–508) and
  `internal/server/direct_pairing.go` (~15–135) — the file's own comment
  admits it's a near-verbatim port. Plus the Kotlin mirror = three copies of a
  security-sensitive contract.
- Create `internal/wire` holding app-facing types (`EventFrame`,
  `TerminalUp`/`TerminalDown`, `Workspace`, pairing DTOs, `pairingCodeTTL`).
  Then extract a pairing-handler factory parameterized by
  `tenantResolver func(*http.Request) (string, bool)` — relay passes its
  mTLS-CN resolver, direct mode passes a constant-tenant resolver.
- **Accept:** `relay` no longer imports `server`; pairing DTOs/handlers exist
  once in Go; all pairing + multitenant + direct tests pass unchanged.

### 5.3 Rework `e2e.Store` persistence — **M/L**, highest correctness payoff
`internal/e2e/store.go` reloads and rewrites the **entire** `sessions.json`
inside one mutex on every send-counter bump, every recv-counter commit, and
every `AddDevice` — i.e. per terminal frame. Worse, `pair.go` and the running
`agent` open the same file from **separate processes** with no flock:
last-writer-wins on rename can clobber a counter commit (replay-window
regression → nonce-reuse risk) or drop a freshly paired device.
- Direction (in order of preference): (a) migrate `e2e` (and `yolo`) state to
  SQLite reusing `auth.Store`'s patterns — the multi-tenant design doc already
  recommends this and it inherits `auth`'s migration story; or (b) keep JSON
  but load once into memory, persist debounced, and add `flock`; or (c) have
  `pair-device` write through the running agent instead of the shared file.
  Propose your pick before implementing (this is L-sized if SQLite).
- While in there: `DeviceIDs`/`SharedSecret`/`yolo.Store.Mode` currently
  swallow load errors — a corrupt file silently disables e2e/push for all
  devices. Log loudly and rename the bad file to `.corrupt` so the operator
  sees it.
- **Accept:** a test proving concurrent pair + counter-commit loses neither;
  no full-file rewrite per frame; corruption is loud.

### 5.4 Small correctness/hygiene batch — **S each**
- **Context-aware backoff sleeps:** `agent.go` (~315) and `events.go`
  (~187/193) use bare `time.Sleep(retry.Next())` — SIGTERM can hang up to the
  30 s max backoff. `pushmon.go` already does the correct
  `select ctx.Done()/time.After` — add `backoff.Sleep(ctx, d) bool` to the
  existing `internal/backoff` package and use it in all three loops.
- **Drop dead `config.Config`:** `server.Server` stores `cfg` that nothing
  reads; remove field + import, simplify `New()`.
- **Deploy path mismatch:** `deploy/term-bridge-relay.service` uses
  `ProtectSystem=strict` + `StateDirectory=term-bridge-relay`, but `config.defaults()`
  points the token store/CA at `~/.config/term-bridge-relay/…`, which is unwritable
  under that hardening. Ship `relay.example.toml` pointing at
  `/var/lib/term-bridge-relay/…` and note it in the README.
- **Env overrides for secrets:** config is TOML-file-only; the containerized
  relay wants `relay_token`/`edge_token`/`listen` from env. Add
  `CMUX_RELAY_*` env overrides for secrets + listen addr only (not a full
  config-from-env system).
- **`auth.Store` bool-collapsing:** `Verify`/`Revoke`/`SetFCMToken` collapse
  "not found" and "DB error" into one bool, and `relay.go` ignores
  `SetFCMToken`'s result. Distinguish where it drives an HTTP response; log
  the rest.

### 5.5 Fast-path socket pool — **M** (after 5.4; measure first)
The cmux host (`internal/host/cmuxhost`) funnels **all** cmux RPC via
`internal/cmux/client.go` (every device, every workspace: input, replay
polls, renames, yolo replies) through one
`socketConn` whose mutex is held for the whole round-trip — a slow replay
blocks an unrelated input ack. The `committed`-flag idempotency logic already
makes multiple connections safe.
- Add a small pool (2–4 authenticated conns, checkout or round-robin). Keep
  the per-call watchdog as is.
- **Accept:** an integration-style test showing a slow RPC on one conn does
  not delay a concurrent fast RPC; fallback-to-subprocess path still covered.

### 5.6 Missing integration tests — **M**
- **Reconnect/session-replacement:** nothing exercises an agent redialing
  while a device request is in flight (`Registry.Set` swap). Add: dial, kill
  session, redial, assert registry swap + continued service.
- **FastPath fallback end-to-end:** the `committed==false` connect-failure →
  subprocess fallback has unit tests only.
- **CLI commands:** `cmd/term-bridge-relay/commands.go` (devices/tenants
  list/revoke) is security-relevant and has zero tests.
- Consolidate the per-package `waitFor`/dial helpers into `internal/testutil`
  while you're there.

---

## 6. Phase 3 — Operability (prerequisite for any multi-tenant growth)

### 6.1 Structured logging with slog — **M**
Zero `log/slog` today; 11 files of plain `log.Printf` with ad-hoc fields
(tenant on some relay lines, absent elsewhere; no request correlation).
- Adopt `slog` with one shared handler and consistent keys: `tenant_id`,
  `device` (use the existing `HashSuffix`), `route`, `status`, `dur_ms`.
  Convert `internal/relay/logging.go`'s access log first, then the rest
  mechanically. Never log body content (invariant §1.5).
- **Accept:** `grep -rn '"log"' bridge/internal bridge/cmd` shows no plain
  `log` imports in prod code; every relay request line carries `tenant_id`.

### 6.2 Metrics — **M**
No metrics at all; you cannot answer "which tenant is hot / erroring."
Cheapest fit: `expvar` on the existing loopback listener (Prometheus can wait).
Instrument the natural choke points: active tunnels gauge (`Registry`),
per-tenant proxied requests + `agent_offline` count (`proxy.go` is a single
choke point), pairing issued/redeemed/expired, push sent/failed (`pushmon`
`fanout` already computes these — just export), e2e decrypt failures.
- **Accept:** `curl localhost:<port>/debug/vars` shows the counters moving
  under the existing integration tests.

### 6.3 Agent status surface — **S/M**
The host agent has no health surface: an operator can't ask "tunnel up? cmux
reachable? last event when?". Add a `term-bridge status` subcommand (read a
small status file the agent maintains, or a local unix-socket query).
Optionally deepen relay `/healthz` to ping its store.

### 6.4 Rate-limit `/devices/pair` — **S**
`/tenants/register` is per-IP throttled now, but the pairing-redeem endpoint
(`handleDevicePair`) still relies solely on single-use codes + 10-min TTL.
Code entropy makes brute force unrealistic; this is cheap defense-in-depth on
an internet-reachable endpoint. Reuse the existing `ipRateLimiter`.

---

## 7. Phase 4 — UX (the product itself; all verified still open at HEAD)

Ordered by (user pain × ease). All are Android-only unless noted.

1. **Overflow "⋮" menu on workspace cards — S.** Rename and YOLO are
   long-press-only with zero visual affordance; undiscoverable. Add a small
   overflow icon opening the *same* `DropdownMenu`; keep long-press as the
   shortcut. (`SessionsScreen.kt`, card composable ~L328/372.)
2. **Distinct treatment for YOLO `Bypass` — S.** It mirrors
   `--dangerously-skip-permissions` yet renders as a plain radio row identical
   to `Off`. Give it error-color styling and a one-tap confirm before it takes
   effect. (`SessionsScreen.kt` ~L258–290.)
3. **Aggregate unread badge on the Inbox button — S.** `hasUnread` exists
   per-workspace (red dot) but the top-bar Inbox `TextButton` has no badge, so
   checking costs a tap even when empty. (`SessionsScreen.kt` ~L81.)
4. **Persist the "Waiting first" sort toggle — S.** Plain
   `remember { mutableStateOf(false) }` today; the drag order right next to it
   persists via `WorkspaceOrderStore`. Persist in `Settings`.
5. **Inbox row → open originating terminal — S/M.** Inbox rows only reply; an
   ambiguous prompt forces backing out to Sessions and hunting. Add navigation
   from `InboxRow` to the workspace's terminal (`InboxScreen.kt`,
   `CmuxNavHost.kt`).
6. **Composable Ctrl modifier chip — M.** Key bar is hardcoded ^C/^D/^Z
   (`TerminalKeys.kt` ~L52–54); Ctrl+L/A/E/R are unreachable. Add a latching
   "Ctrl" chip that modifies the next key (including typed letters), instead
   of more one-off buttons.
7. **"Send test notification" button — S/M** (bridge + app). Push setup was
   painful to debug; a settings-screen button triggering a round-trip test
   push turns future breakage into a 30-second check. Needs a small bridge
   endpoint + relay FCM send.
8. **Autopilot summary — S.** No single place shows which workspaces are on
   `Always`/`Bypass` before stepping away. A compact banner/row on Sessions
   when any workspace is in an auto-reply mode.
9. **Accessibility pass — S.** D-pad and key bar have no `contentDescription`
   (raw glyph labels); unread dot, YOLO badge, attention stripe are invisible
   to TalkBack. Add semantics; verify with TalkBack once.
10. **Search/filter over workspaces — M.** Lowest priority; matters once
    session count grows.

---

## 8. Phase 5 — Polish backlog (batchable, no ordering)

- **Compose previews (0 exist):** add `@Preview` for the pure leaf
  composables: `WorkspaceCard`, `PaneRow`, `KindBadge`, `InboxRow`,
  `ConnectionRow`, `DeliveryStatusLabel`.
- **Strings to resources:** `strings.xml` has only `app_name`; 42 hardcoded
  `Text` literals + repeated VM error strings ("Bridge not configured" ×5).
- **Semantic colors:** `PermissionAccent`/`WaitingAccent` hex literals in
  `SessionsScreen.kt` → theme-level semantic colors. (The terminal palette is
  legitimately outside Material theming — leave it.)
- **Gate keystroke debug logs behind `BuildConfig.DEBUG`** (redaction exists,
  but per-keystroke logging of a remote terminal is still a smell).
- **Naming:** the e2e `Session` class vs cmux "sessions" vs `/sessions`
  endpoint vs `Workspace` model is a three-way overload — rename the crypto
  one (e.g. `CryptoSession`); introduce enums/consts for the stringly-typed
  frame discriminators (`"ack"`, `"input"`, `"resize"`, …) and
  `Workspace.attention` values.
- **RenderGridView perf (flagged tradeoff, measure before acting):** up to
  2000 scrollback rows render in a plain `Column`, and the per-row
  `remember` key includes freshly-allocated objects in wrap mode, so it
  rarely hits. `SelectionContainer`-over-`LazyColumn` is awkward — that's why
  `Column` was chosen. Cheapest real win: make the remember key stable
  (line-content based) so unchanged rows skip `AnnotatedString` rebuilds.
  Only attempt `LazyColumn` with a working text-selection story.
- **Plan (don't yet execute) the `security-crypto` migration:**
  EncryptedSharedPreferences is deprecated and load-bearing for tokens + key
  material. Write the migration design note (Tink directly, or DataStore +
  app-managed Keystore key), including in-place data migration for existing
  installs.
- **Dependency refresh:** AGP 8.7 / Kotlin 2.0.20 / Compose BOM 2024.10 /
  OkHttp 4.12 are a late-2024 cohort; bump routinely once CI (Phase 0)
  exists. Migrate deprecated `kotlinOptions` → `compilerOptions` at the same
  time.
- **CHANGELOG.md:** start one now that the repo is public.
- **API versioning design note (L, design-only):** the app↔bridge schema has
  no version negotiation beyond the envelope's `v:1`, while fields are still
  churning. Write the compat-policy note (e.g. `X-Cmux-API-Version` header +
  additive-only rule) anchored to the `wire` package from 5.2 — decide the
  story before many app versions are installed. Don't implement negotiation
  machinery until it's actually needed.

---

## 9. Explicit non-goals — do NOT do these

Investigated and rejected (see `docs/enhancement-audit-validation.md`):
- Any REFUTED audit finding: the RenderGrid wide-char "bug" (already
  correctly handled), the `Registry.Set` "race" (correct as written), the
  "rootful podman" claim (it's deliberately rootless).
- OkHttp bearer-on-redirect "leak" — OkHttp 4.x strips `Authorization` on
  cross-host redirects; no fix needed.
- TLS `MinVersion`/cipher hardening in Go `serveDirect` — Go's defaults
  already exclude sub-TLS1.2 and use the curated suite list.
- Rewriting `TerminalInputDiff` to a cursor-aware edit script — the
  prefix-only diff is documented, correct, and low value to replace.
- Terminal viewport "reflow" work in this repo — the phone already sends its
  own cols/rows and the bridge forwards them to cmux
  (`mobile.terminal.viewport`); any remaining sizing weirdness lives in the
  cmux backend's arbitration, outside this repo. Verify live before touching.
- Restoring workspaces from the phone: cmux has only
  `session.restore_previous` (the whole app session), nothing per
  workspace. Create/select/close are in as of 2026-09-12; restore stays out.

## 10. Known parked issues (context, not tasks)

- **IPv6 flakiness** on the relay path — parked deliberately; don't "drive-by
  fix" while touching transport code.
- **Possible dual-path duplicate push** (relay pushmon + agent-native path) —
  unconfirmed; if touching push code, add a dedup assertion or verify live
  rather than assuming.
- **Multi-tenant sub-projects 2–4** (onboarding, abuse controls, ops) from
  `docs/superpowers/specs/2026-07-01-multi-tenant-relay-design.md` are open
  by design; Phase 3 + 6.4 here are their prerequisites, not their
  replacement.
