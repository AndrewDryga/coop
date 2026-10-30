# .agent/kb — the self-improving knowledge base

Descriptive operational knowledge an agent needs but the code doesn't obviously carry: subsystem
maps, cross-cutting traps, hard-won gotchas. A card directly under `kb/` is DESCRIPTIVE ("here's how
X actually works, and the trap").

The NORMATIVE floor lives one level down in **[`rules/`](rules/README.md)** ("do X, not Y") — a
verdict a review can fail against, with its own index, card format, and `check:` field. Read that
index at boot too; a rule may link up to a card here for background. One committed knowledge tree,
two registers.

## Reading protocol
Read this INDEX at boot; open a card ONLY when your task touches its subsystem. Never bulk-load the
kb into a prompt — the index is the routing table, cards are pulled on demand (like skills). That
scoping is also the safety rail: a card only ever reaches the prompts of tasks in its own subsystem,
so a wrong card can't poison work it doesn't touch.

## You maintain this KB — directly
This is a self-improving wiki: no inbox, no human gate. When a task teaches you something non-obvious
about a subsystem — a map, a trap, a gotcha the code doesn't carry — CREATE or UPDATE its card here,
in the same commit as the work. Keep it TIDY as it grows: once a flat list gets long, group cards
into per-subsystem subfolders (`box/`, `acp/`, `loop/`…) and keep this index current. The structure
is yours to evolve — organize it however keeps it usable.

The discipline that replaces the human gate is the metadata: every card states when it was last
`updated`, which `subsystem` it maps, and the `sources` (the code) it describes — so staleness is
obvious at a glance. When you pass through a subsystem, check its cards against their `sources`; if
one has drifted, re-verify and bump it (with a changelog line) or DELETE it — a card that
contradicts the code is worse than no card.

## Card format
One fact per file: frontmatter, a short body (under a screen), and a small changelog so an outdated
card is obvious.

```
---
name: <kebab-case-slug>              # = the filename
description: <one line — judged for relevance straight from this index>
subsystem: <box | acp | loop | …>    # groups the index; decides when the card loads
sources: [internal/box/run.go, …]    # the code this describes — check drift against it
updated: <YYYY-MM-DD>                # last edit
---

<the fact; name the function, type or file behind a load-bearing claim — never a line number,
 which moves as the file changes; link related cards with [[name]]>

## Changelog
- <YYYY-MM-DD> — created / what changed (and what you verified it against)
```

`make rules-check` holds you to the mechanical half of that — every field present and non-empty,
`name` matching the filename, `updated` a real date, every `sources:` path still existing, a
changelog section, no line-number citation (`run.go:1392`, `line 19`), and Index pairing BOTH ways
(every card indexed exactly once, every indexed link resolving). What it can't tell you is whether a
card is still TRUE; only reading it against its `sources` does that.

## Index
- [quality-tool-reviews](quality-tool-reviews.md) — coverage scope, equivalent mutants and semantic ownership behind the Crapper/Dryer/Mutator findings
- [local-development-checks](local-development-checks.md) — explicit Make owner scopes, reverse-consumer guard and broad fallback; no automatic selector or implicit live calls
- [ci-free-checks](ci-free-checks.md) — Make-owned CI partitions, per-owner caches and a status-only required check
- [coop-repo-box-go](coop-repo-box-go.md) — coop's own boxes are filtered, so Go and the gate tools come from .agent/Dockerfile and four approved domains, the module proxy's storage.googleapis.com redirect included; module downloads share the coop-cache volume
- [website](website.md) — site/ is static and partly generated: tools/gendocs owns index.html, orchestrator.html and the docs' terminal regions, which copy real CLI output; night-board, code-block, cache-bust and social-card traps
- [shell-completion-current-word](shell-completion-current-word.md) — current-word replacement and the Bash 3.2 COMP_WORDS/IFS trap in generated completion
- [restricted-networking](restricted-networking.md) — the layers between an `--egress filtered` flag and `docker run`, where authority lives (never a repo, never a box), the precedence ladder, and what a filtered run refuses
- [network-gateway](network-gateway.md) — the controller/guard pair that enforces a capture: nftables capture and grants, SNI and DNS admission, what observation measures versus counts, exact-owned cleanup
- [network-consumers](network-consumers.md) — how the loop, direct runs and remote sessions consume ONE frozen capture per box (a filtered `coop acp` session replaces boxes after an approval), and which `coop net` verb reads which evidence
- [in-box-task-channel](in-box-task-channel.md) — the loop box changes task state through the coop-owned `coop-tasks` MCP server over a helper-container unix socket (never a host-created one, never HTTP), refusing only a task another live process holds
- [release-qualification](release-qualification.md) — tagged releases reuse exact-commit CI, validate finalized notes and preserve tag identity
- [box-time-is-utc](box-time-is-utc.md) — boxes run UTC; the host TZ is forwarded so rate-limit reset prose parses back host-local
- [box-base-image-tags](box-base-image-tags.md) — the shared base is `coop-box:<definition>` so two Coop versions keep their own; no :latest alias, an upgrade is built on launch, project images stay shared
- [box-home-nested-mounts](box-home-nested-mounts.md) — avoid bind targets that make Docker create missing application-owned home parents as root
- [box-entrypoint-descendant-handoff](box-entrypoint-descendant-handoff.md) — supervised loop/review boxes authenticate forwarder exemptions and hand off live detached jobs
- [box-orphans-survive-pdeathsig](box-orphans-survive-pdeathsig.md) — Pdeathsig is not inherited across fork, so a forking worker orphans into the box and holds it open for the whole drain
- [box-supervisor-label-and-orphan-sweep](box-supervisor-label-and-orphan-sweep.md) — every box records the host process supervising it; only a provably dead one authorizes a reap, scoped to the workspace that launched it
- [box-host-port-window-is-contended](box-host-port-window-is-contended.md) — coop's deterministic serve host ports sit inside the OS ephemeral range, so any process can be holding one; publishing is best-effort and a test must never claim one for real
- [host-disk-exhaustion-stops-the-runtime](host-disk-exhaustion-stops-the-runtime.md) — a host out of disk OR memory stops the container runtime mid-run and surfaces as unexplained "unexpected EOF" failures; prune alone never returns the space
- [services-teardown-needs-the-workspace](services-teardown-needs-the-workspace.md) — sibling services persist across boxes on purpose, but teardown reads the workspace's compose file, so stopping them after deleting the workspace does nothing
- [compose-host-authority](compose-host-authority.md) — sibling Compose execution uses a validated private snapshot and explicit values rather than ambient host imports
- [host-execution-surfaces](host-execution-surfaces.md) — which changed files count as "runs on your machine", the two tiers, and where coop surfaces them (fork review/merge, check-secrets)
- [secret-finding-identity](secret-finding-identity.md) — how a secret finding is named across runs (fp-v1 fingerprints), what .coopsecretsignore may excuse, and why exceptions stop at check-secrets
- [doctor-report-accounting](doctor-report-accounting.md) — how `coop doctor` counts: the 37 checks, the outcomes a row can have, and why a failed probe adds one failure plus the checks it was carrying
- [eval-trial-isolation](eval-trial-isolation.md) — what isolates one `coop eval` trial from the next and from the grader, and why per-trial credential copies would break authentication (single-use refresh tokens)
- [trusted-git-view](trusted-git-view.md) — host git runs under a coop-owned GIT_DIR view with an allowlisted config, so repository-defined filter/textconv/merge drivers never execute; ref-store writes stay on the real git dir
- [isolated-fork-publication](isolated-fork-publication.md) — optional independent execution, exact private review, semantic parent preconditions and durable forward-only publication
- [test-fixture-guards-vs-timing-bounds](test-fixture-guards-vs-timing-bounds.md) — a wait that guards a broken fixture is generous (testutil/wait, 60 s); a tight bound is only for timing that IS the behavior, and attributes its phases
- [test-state-isolation](test-state-isolation.md) — every TestMain that can reach Coop's per-user state calls teststate.Isolate; Coop refuses the real roots in a test binary, children inherit the folder, live suites opt back into the real config and network state; the cli unit tests run with failing stubs for the host provider CLIs and runtimes
- [session-cost-accounting](session-cost-accounting.md) — logical-turn spend, native cumulative counters, attempt checkpoints and conservative historical normalization
- [session-migration-fixtures](session-migration-fixtures.md) — frozen historical DDL and rows stay independent of current migrations; fixture clocks and IDs are private store state
- [secret-shadow-walk](secret-shadow-walk.md) — every launch walks the whole checkout from a real-folder root (a linked root and a link to home are refused, though a box that writes the root's parent can still swap it), gitignored task folders included, to hide secrets; its cost grows with local clutter, so the decider remembers directory verdicts, lstats for .coopignore and pre-filters its globs; the .coopignore files above the repository, short of home, apply too
- [repository-path-keys](repository-path-keys.md) — what is keyed by the repository's real path and what by its spelling; a resolved `COOP_REPO` moved a link user's forks, images, serve ports and agent history; place user-given paths with `box.RepoRelative` and match recorded fork paths with `box.SameFolder`; macOS tests must resolve `t.TempDir()`
- [repository-source-copies](repository-source-copies.md) — skills and fallback copies retain rooted content authority and validate links only after relocation; a lead's own agents are copied without following any link
- [sign-in-again-is-actionable](sign-in-again-is-actionable.md) — refreshable OAuth stays usable; "Sign in again" means the stored login cannot recover — run the coop login it names
- [credential-presence-is-adapter-declared](credential-presence-is-adapter-declared.md) — adapters own credential presence, selected env authority, and inspectable stored readiness
- [native-credential-cutover](native-credential-cutover.md) — existing native writer locks keep their inode during private normalization; uncertain legacy grants remain in custody
- [native-broker-account-discovery](native-broker-account-discovery.md) — guard-selected public account aliases must match native discovery without changing workspace routing
- [native-broker-managed-policy](native-broker-managed-policy.md) — Claude policy retrieval needs its exact native settings route and must retain genuine upstream policy and errors
- [native-broker-images](native-broker-images.md) — Codex generation and editing require exact image routes for each provider credential family
- [native-broker-lease-reads](native-broker-lease-reads.md) — the host renews up to 10 minutes ahead, runs on one account take turns retrying, and the held access stays leased through a passing failure; VM lease reads use fresh descriptors and the existing load deadline, never a cached grant
- [native-helper-file-modes](native-helper-file-modes.md) — native helper read mounts keep their intended modes under a private umask while outer custody remains owner-only
- [mcp-authority-projection](mcp-authority-projection.md) — one validated shared snapshot fans out to native configs, direct command args, nested wrappers, and ACP without widening credential scope
- [provider-scripted-e2e](provider-scripted-e2e.md) — drive the external Coop CLI through strict runtime/provider fixtures without ambient state
- [provider-live-e2e](provider-live-e2e.md) — probe installed upstream CLIs with isolated read-only, native-resume, and task-completion workflows
- [provider-client-qualification](provider-client-qualification.md) — locked clients, conformance rows, on-demand live suites and the client bump procedure
- [provider-session-history](provider-session-history.md) — native session layouts, lookup bounds, pre-v11 import completion stamps, and the large-history regression contract
- [usage-recorded-token-value](usage-recorded-token-value.md) — independent native quota and recorded token value, historical account ambiguity, and original-home Gemini refresh coordination
- [usage-history-cache](usage-history-cache.md) — per-transcript token cache for coop usage: keys, invalidation, and per-folder pruning after a complete full check
- [provider-consult-e2e](provider-consult-e2e.md) — verify generated coop-consult behavior through all provider arms, fallback pairs, and a four-edge live ring
- [model-tiers-and-role-vs-lead](model-tiers-and-role-vs-lead.md) — ModelFor is one model per provider (active>target>env); a preset role's model rides its wrapper target, never global state, or it shadows a rotated lead
- [gemini-effort-thinking-settings](gemini-effort-thinking-settings.md) — Gemini has no effort flag: low/high become per-call system settings overriding its two thinking family bases, never a model name the client would remap
- [preset-scaffold-publication](preset-scaffold-publication.md) — preset initialization creates every file exclusively inside the repository, refuses any existing destination and removes its own files on failure
- [init-preflight](init-preflight.md) — init resolves stack prerequisites and renders the Dockerfile before any scaffold or hook mutation; its in-place edits (Compose, project.yaml, .gitignore) refuse links and files hidden from the box, new files and agent links are created, checked and removed through one parent handle, and what it prints from the repository goes through ui.SafeInline
- [loop-live-bar](loop-live-bar.md) — the loop's sticky bottom bar; every paint parks the cursor at column 0 so a kernel ^C echo can't wrap the line and desync the region's erase math — the subsystem once deleted instead of debugged
- [loop-provider-watchdog](loop-provider-watchdog.md) — built-in attempts always stream; the watchdog is ARMED by default (10m/30m/2h) and trusts only decoder events, and the box's own process group makes redirected loops handle stop signals themselves
- [loop-rotation-advance-triggers](loop-rotation-advance-triggers.md) — rate limits cool and come back, auth failures are sticky for the run; known-invalid credentials never become rungs
- [loop-resume-never-rewrites-history](loop-resume-never-rewrites-history.md) — a leaked box descendant un-completes committed work; resuming it must never amend a non-HEAD commit
- [loop-range-rejects-outside-commits](loop-range-rejects-outside-commits.md) — an iteration's commit range is a time window: a foreign commit's Coop-Task trailer rejects that iteration's completion only when it names a task this iteration's authority could touch, else it's tolerated and journaled; the per-worktree ref-authority lock closes the narrower validate-then-consume race
- [host-signing-starts-at-first-unsigned](host-signing-starts-at-first-unsigned.md) — every host signing pass rewrites only from the first unsigned commit and refuses to rewrite a commit the upstream already has; -f stays, and "signed" means a signature header
- [gate-source-protection](gate-source-protection.md) — built-in gate files plus a frozen project-owned exact-path list decide which loop changes require protected review; a change list Git cannot read stops the run
- [fork-review-scratch-two-copies](fork-review-scratch-two-copies.md) — forkctl and sessionsvc keep separate review-scratch clones on purpose: the same 18-line scaffold under opposite anchoring contracts (live HEAD preview vs verified captured intent)
- [fork-lifecycle-state-file](fork-lifecycle-state-file.md) — one generation-bound owner-v2 file names a fork's detached worker by pid+start token, written under the lock its child must take first; unreadable bytes stay held and only pid+start-token — never file age — may decide that worker is gone
- [fork-lifecycle-test-barriers](fork-lifecycle-test-barriers.md) — lifecycle race tests observe command lock entry after preflight, never infer it from a fixed delay
- [fork-candidate-review-rounds](fork-candidate-review-rounds.md) — immutable reviewed fork snapshots, the replayable current manifest, fresh candidate-wide signoff, and zero-ahead merge bookkeeping
- [acp-preset-owns-toolbar](acp-preset-owns-toolbar.md) — active ACP presets own the whole lead target and refuse stale Provider/Account editor replays
- [acp-warm-pool-identity](acp-warm-pool-identity.md) — the editor warm pool lends a parked box only to an identical switch (plain target, default model/effort, same account, image and network policy); prove hits from the trace
- [lifecycle-latency-measurement](lifecycle-latency-measurement.md) — pair the two builds and alternate them for any start/stop claim; never bisect a noisy wall-clock metric single-shot; build the comparison binary outside the checkout or its version stamp lies; an old binary cannot start in an approved project
- [acp-auth-is-provider-account-scoped](acp-auth-is-provider-account-scoped.md) — initialize capability truth and successful authentication belong to one provider account
- [acp-scripted-e2e](acp-scripted-e2e.md) — test the real ACP supervisor/control/proxy path with a scripted runtime and isolated state
- [acp-child-exit-diagnostics](acp-child-exit-diagnostics.md) — stdout EOF can precede stderr collection, and a fast-failing child can exit before its first frame is written; both wait briefly for the exit before rendering safe setup guidance
- [acp-replay-publication](acp-replay-publication.md) — publish replacement native bindings atomically before releasing held editor work
- [acp-thread-bindings](acp-thread-bindings.md) — persist each thread's provider + native session id so a fresh coop acp reopens the transcript the conversation continued on, not the pre-switch stub
- [acp-target-commit](acp-target-commit.md) — commit model/effort truth from the effective provider response, including Grok migrations
- [acp-carry-echo](acp-carry-echo.md) — inject best-effort context once and hide only its exact provider echo from the editor
- [acp-generated-output-boundary](acp-generated-output-boundary.md) — generated images bypass transcript bytes but remain bounded, immutable turn artifacts
- [acp-rewrites-must-keep-line-framing](acp-rewrites-must-keep-line-framing.md) — an ACP line rewrite that drops the trailing newline hangs the session silently; parsing tests still pass
- [codex-acp-agent-mode](codex-acp-agent-mode.md) — Codex ACP must select full-access mode explicitly because session config does not override its per-turn sandbox policy
- [session-api-dto-is-a-second-projection](session-api-dto-is-a-second-projection.md) — a field on the durable session record stays invisible to API clients until the hand-written DTO and its public* copier carry it too
- [session-source-selection](session-source-selection.md) — controller jobs freeze exact source bindings; private staging is immutable while review refreshes the current default separately
- [session-awaiting-validation-runtime-cleanup](session-awaiting-validation-runtime-cleanup.md) — every started turn retains its exact runtime receipt through teardown; awaiting-validation owns durable candidate authority but no live provider runtime
- [session-operation-intents-cross-versions](session-operation-intents-cross-versions.md) — durable intents survive upgrades; replay requires frozen job authority and never reconstructs it from retired local policies
- [session-companions-own-git-metadata](session-companions-own-git-metadata.md) — companion boxes mount only the pinned workspace, so each snapshot must own usable Git metadata rather than point back to its source checkout
- [session-controller-task-git-custody](session-controller-task-git-custody.md) — confirmed engineering task bookkeeping is separate checkpoint custody, not product changes or a blanket hidden task folder
- [review-host-owned-verdicts](review-host-owned-verdicts.md) — review boxes report bounded evidence; Coop alone applies validated task lifecycle changes
- [signoff-scope-is-run-anchored](signoff-scope-is-run-anchored.md) — signoff subjects are a run-anchored folder diff; re-anchor only on receipt-consistent rounds, from the post-review done set
- [task-state-is-the-folder](task-state-is-the-folder.md) — a task's state IS its directory; a bare `mv` to a missing state dir silently corrupts the queue
- [canonical-fork-task-authority](canonical-fork-task-authority.md) — the project owns one Markdown task; a fork owns only an exact-generation execution projection and reviewed candidate
- [identity-fences-compare-the-inode](identity-fences-compare-the-inode.md) — inode and timestamp reuse is real; durable fork/network roots use private hardlink anchors, while tasks and destructive plans combine logical identity, semantic checks and live pins
- [task-authority-registry-is-durable-state](task-authority-registry-is-durable-state.md) — host-global task ownership and completion trust live in ~/.local/state/coop/task-leases, never a cache dir; every authority flock rechecks its inode
- [task-authority-model](task-authority-model.md) — four separate authorities decide who may act on a task/checkout — durable owner, iteration lease, checkout lock, and ref window — never merge them
- [task-tmp-lifetime](task-tmp-lifetime.md) — task-local tmp survives resumable states but is containment-cleaned on done before review; artifacts persist
- [session-evidence-export](session-evidence-export.md) — one bounded versioned read exports a session's network posture, observation, receipt and bound task; every section states its own availability and unknown never collapses into zero
- [worker-connector](worker-connector.md) — the outbound worker journals before executing, resends results until acknowledged, moves workspaces only as verified bundles, and never falls back to local execution
- [worker-storage-accounting](worker-storage-accounting.md) — allocated blocks not apparent size, a hardlinked baseline charged once, a discard that renames before it deletes, used-byte watermarks over a free-byte reserve, and unknown that is never zero
