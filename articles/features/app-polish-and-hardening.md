---
type: article
description: App icon, Settings README link, API versioning policy, and the e2e store SQLite persistence proposal — four independent polish/hardening items consolidated from the original design docs.
status: canonical
authored: 2026-07-21
author: sodre90
tags:
  - article
  - canonical
  - polish
  - hardening
---
## Summary

Four unrelated small-scope items grouped only by being minor app-polish or hardening work: a new app icon, a Settings-screen help link, a wire-compatibility policy note, and a security-relevant persistence fix for the e2e replay-counter store. Each stands on its own and is treated separately below. See [overview](../overview.md) and [pairing-e2e-encryption](./pairing-e2e-encryption.md) for how the e2e store fits the shipped security model.

## Body

### App icon

**What it is:** A "layered terminal windows" icon (two overlapping terminal-window frames, cyan foreground + magenta background, white prompt glyphs on a dark navy field) to replace the Android app's default system icon, with an optional macOS ICNS for the bridge agent if it ever gets a GUI.

**Why:** The app had no custom icon; this defines one signaling "terminal / command-line control" plus the two-device (phone + Mac) remote-connection concept.

**Key design decisions:** exact palette (Cyan `#00D9FF`, Magenta `#FF1493`, White `#FFFFFF`, Dark `#0A0E27`); flat style, no gradients/drop shadows, legible down to 48px; SVG master at 1024×1024, exported to Android's five density buckets plus an optional 512px macOS ICNS; `AndroidManifest.xml`'s `android:icon` changes to `@mipmap/ic_launcher`. Deferred to future considerations: adaptive icon safe-zone, light/dark variants, launch animation. The implementation plan lists seven tasks (create the SVG, mipmap directories, export PNGs, optionally build the ICNS, update the manifest, build/install/verify on device, commit).

**Status:** every checkbox in both the plan's tasks and the spec's success criteria is unchecked in the documents as written; neither doc states the icon was actually produced, committed, or verified on a device.

### Settings README link

**What it is:** A "Setup guide" text button added above the Bridge base URL field on the Settings screen, opening the repo's README on GitHub in the device's default browser via an implicit intent.

**Why:** Someone installing the APK directly with no prior context sees a bare pairing form with no explanation; linking out avoids duplicating the README's architecture/setup content inside the app.

**Key design decisions:** link out to GitHub rather than render Markdown in-app; a secondary-weight `TextButton`, not a filled button; no changes to `SettingsViewModel` or persisted settings; no deep link to a specific section; requires no new Android permissions.

**Status:** design spec only (no accompanying implementation plan among these docs); its own success-criteria checklist is entirely unchecked. The spec's target URL predated the later repo rename to `sodre90/cmux-mobile-app`; it was corrected in the spec on 2026-08-08. Confirmed unimplemented — no GitHub URL appears anywhere in `android/app/src/main/`.

### API versioning policy

**What it is:** A design-only policy note responding to the improvement guide's flag that the app↔bridge wire schema has no real version negotiation beyond a hardcoded envelope `v:1`.

**Why it matters:** wire fields do churn (multiple commits touching the Kotlin DTOs, plus one confirmed breaking rename — `sessions` → `workspaces`, see [android-terminal-foundation](./android-terminal-foundation.md)) — the improvement guide wanted a deliberate compatibility story before it became a real problem.

**Key findings/decisions:** an audit of the current code found both sides are already additive-only-safe by construction, as an accidental default — Kotlin's codec uses `ignoreUnknownKeys = true` with every DTO field defaulted, and Go's `encoding/json.Unmarshal` ignores unknown keys and zero-fills missing ones by default. No `X-Cmux-API-Version` header or version-negotiation signal exists anywhere in the codebase. The recommendation is a deliberate scale-down from the guide's own suggested starting point: codify the additive-only rule; defer the version header entirely, since a header nobody reads/branches on is inert and this app's deployment model (one operator deploying phone + relay + Mac bridge together, no staged rollout) has no scenario today where negotiation machinery would do anything. Concrete near-term action proposed (docs-only): extend `internal/wire`'s package doc comment to state the additive-only contract explicitly, and cross-reference it from this repo's `CLAUDE.md` wire-format-lockstep invariant. Named trigger conditions for revisiting the deferral: moving off self-built debug APKs to staged rollout, a second independently-operated deployment, or a genuinely non-additive wire change landing without guaranteed simultaneous deployment.

**Status:** explicitly a design note with "no production code changes." The two documentation edits it recommends are proposed, not reported as done — whether/when to land them is left as implementation-time judgment.

### E2E store persistence

**What it is:** A plan + design pair migrating `internal/e2e/store.go` and `internal/yolo/store.go` off whole-file-JSON persistence onto SQLite-backed stores, mirroring the pattern already used by `internal/auth/store.go`.

**Why it matters (security-relevant, not just polish):** the JSON design reloads and rewrites the entire file on every mutating call — every outbound/inbound terminal frame triggers two full loads plus one full rewrite. Worse, `term-bridge pair-device` (short-lived) and the long-running agent process each hold independent store handles with only in-process mutexes; because the persistence unit is the whole devices map, concurrent writes to unrelated devices can clobber each other via last-writer-wins rename. A lost recv-counter commit is a replay-window regression — an AEAD nonce-reuse risk (see [pairing-e2e-encryption](./pairing-e2e-encryption.md) for the sliding-window design this would undermine), not a cosmetic bug. A corrupt store file also fails silently today, disabling e2e/push for all devices with no warning.

**Key design decisions:** three directions were evaluated — SQLite migration; keep JSON + in-memory cache + debounced writes + flock; or route `pair-device` writes through the running agent via local IPC. Only the SQLite direction was assessed as satisfying all three acceptance criteria (no clobber, no full-file rewrite per frame, loud corruption); the flock option was rejected as still doing full-file rewrites and worsening crash-safety via its debounce window, and the IPC option only fixes the rare pairing race while leaving the frequent per-frame rewrite cost untouched. Sized **M**, not **L** as the improvement guide's own tag suggested, since production code outside `e2e`/`yolo` needs zero changes and the same JSON→SQLite move was already executed once for `auth.Store` — a literal precedent to mirror. Schema: a `devices` table keyed by device ID (pubkey, shared secret, send counter, recv-window state, last-active) and a `yolo_modes` table keyed by workspace ID. `Open(path) (*Store, error)` replaces the old non-fallible constructor. Loud corruption handling: on a typed corruption error, rename the bad file aside to a timestamped `.corrupt` copy, log loudly, retry fresh — rather than today's silent-empty-store behavior. A one-time, self-terminating legacy import runs on first `Open` with empty tables and an existing legacy JSON file present, renaming it to `.migrated` afterward as a forensic/rollback copy.

**Implementation plan:** six tasks — rewrite `e2e/store.go` + tests + call-site updates; same for `yolo/store.go`; update `agent.go`/`pair.go` for the now-fallible `Open`; update default config paths; add three new acceptance tests (concurrent pair+counter-commit race, no-scaling-with-device-count, loud corruption recovery); full-suite verification including an explicit re-run of `internal/relay/multitenant_test.go` to confirm it was untouched.

**Status:** the design doc states explicitly "No production code changes accompany this document," and every checkbox in the implementation plan is unchecked — both documents are the design + plan artifacts only, not a record of the migration being implemented, tested, or merged. **Note:** the [enhancement-audit](../enhancement-audit.md) material (from a later, separate audit pass) confirms the underlying replay-race concern described here is real in the shipped code at the time of that audit (a genuine concurrent validate/commit TOCTOU in `internal/e2e/store.go`), which is consistent with this SQLite migration still being unimplemented as of these documents.

## References

- [docs/superpowers/plans/2026-07-01-app-icon-implementation.md](../../docs/superpowers/plans/2026-07-01-app-icon-implementation.md)
- [docs/superpowers/specs/2026-07-01-app-icon-design.md](../../docs/superpowers/specs/2026-07-01-app-icon-design.md)
- [docs/superpowers/specs/2026-07-01-settings-readme-link-design.md](../../docs/superpowers/specs/2026-07-01-settings-readme-link-design.md)
- [docs/superpowers/specs/2026-07-10-api-versioning-policy-design.md](../../docs/superpowers/specs/2026-07-10-api-versioning-policy-design.md)
- [docs/superpowers/plans/2026-07-10-e2e-store-persistence-plan.md](../../docs/superpowers/plans/2026-07-10-e2e-store-persistence-plan.md)
- [docs/superpowers/specs/2026-07-10-e2e-store-persistence-design.md](../../docs/superpowers/specs/2026-07-10-e2e-store-persistence-design.md)
