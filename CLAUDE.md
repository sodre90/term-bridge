# term-bridge — agent operating card

Android (Kotlin/Compose) phone client + two Go binaries (`term-bridge-relay` home-server
daemon, `term-bridge agent` on each host -- a Mac running cmux or a Linux box
running tmux, behind one `Host` interface). The app pairs with several hosts
and switches between them. Full context: `docs/improvement-guide.md` (read it
before any non-trivial change). Architecture/security model: `README.md`;
the Linux/tmux/multi-host design:
`docs/superpowers/specs/2026-09-14-linux-tmux-host-design.md`.

## Build & verify (run before every commit)

```bash
cd bridge && go build ./... && go vet ./... && go test ./...
cd android && ./gradlew :app:assembleDebug :app:testDebugUnitTest :app:detekt
```

Note: `testDebugUnitTest` needs a JDK 21+ runtime (a test-only dependency,
`lazysodium-java`, ships Java 21 class files) even though app code targets JVM 17.

`:app:detekt` is part of the gate, not an optional extra — it enforces import
ordering and declaration spacing that nothing else catches, and three commits
landed broken while it was missing from this list.

## Non-negotiable invariants

1. **cmux is a black box.** Only talk to it via `cmux rpc` / `cmux events`.
   Never copy cmux source. Every mutation names its target by UUID; cmux's
   create methods fall back to whatever is focused on the Mac when given
   none, so never call `workspace.create`/`surface.create` to probe them
   (see `docs/superpowers/specs/2026-09-12-workspace-layout-control-design.md`).
   **tmux likewise**: only via the `tmux` CLI and its control mode (`tmux
   -C`), always targeting a window/pane by `$n`/`%n` id, never "the current
   one"; never read its source or socket format. Live probing of either is
   confined to the scratch tmux session `cmux-app-scratch` on the home
   server; never create/kill against a live session.
2. **`internal/relay/multitenant_test.go` must always pass** — it enforces
   tenant isolation, not just a test.
3. **Wire-format lockstep.** The app<->bridge protocol is hand-mirrored in
   Kotlin (`model/Dtos.kt`, `data/e2e/*`) and Go (`internal/server/*`,
   `internal/e2e/*`) — pairing DTOs also exist a third time in
   `internal/relay/relay.go`. Any field change lands on ALL copies in the same
   commit, with tests on every side touched.
4. **Never weaken e2e crypto**: X25519+HKDF, AEAD on every body/terminal
   frame, replay-protected counters (validate+commit is atomic — keep it so).
   The relay must stay blind to content.
5. **Never log secrets**: typed input, tokens, key material must never reach
   logcat or Go logs.
6. **Commits**: one logical item per commit, authored solely by the human
   developer, no AI co-author trailers.

## Working conventions

- Locate code by symbol (grep), not by line numbers in docs — they drift.
- One guide item = one branch/commit; don't bundle refactor with behavior change.
- For anything **L-sized** (multi-day / needs a design pass): write a spec +
  plan pair under `docs/superpowers/` first (see existing pairs there for the
  format), get it reviewed, then implement.
- Don't act on `docs/improvement-guide.md` §9 (explicit non-goals) — those were
  investigated and refuted.


<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:6cd5cc61 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->
