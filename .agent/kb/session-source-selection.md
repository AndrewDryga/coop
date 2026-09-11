---
name: session-source-selection
description: a session picks default/branch/pull_request/commit inside its policy's repository; git fetch by OID short-circuits on a cached object, so commit custody needs a second anchor
subsystem: sessions
sources: [internal/sessionsvc/source.go, internal/sessionsvc/service.go, internal/session/records.go, internal/session/schema.go, internal/workerconnector/executor.go, internal/workerconnector/capabilities.go, docs/session-api.md]
updated: 2026-09-11
---

A create may carry one bounded `source` selector — `{"kind":"default"}`, `{"kind":"branch","name":…}`,
`{"kind":"pull_request","number":…}`, `{"kind":"commit","sha":…}` — and NOTHING else about where the
code comes from. The repository, the remote and the default branch stay in the operator policy;
Coop derives every ref (`refs/heads/<name>`, `refs/pull/<n>/head`) from the policy plus that one
value. An omitted selector means `default`. A policy with no configured `remote` is intentionally
local: it keeps local semantics for its own default and refuses the other three, because it has no
remote that could prove them.

**The trap this subsystem exists for.** `git fetch <remote> <oid>` answers SUCCESS WITHOUT
CONTACTING THE SERVER when the object is already in the local object database (`everything_local()`
in fetch-pack). The policy-owned cache is the operator's own checkout, so it holds their
unpublished commits — meaning "the fetch succeeded" is no evidence at all for a cached object.
`pinSessionSourceCommit` therefore still runs the bounded OID fetch (it is the whole proof when the
object is absent) and, when the object was ALREADY present, anchors custody a second way: the
remote's current advertisement of `refs/heads/*` and `refs/pull/*/head` must contain the commit as a
tip or as an ancestor of one, using only objects already held. A cached local-only commit satisfies
neither and is refused.

Two more things measured against real git 2.39, all of which ruled out simpler designs:

- Git's LOCAL transport (a bare path or a `file://` URL) serves any object the remote has,
  reachable or not, ignoring `uploadpack.allowAnySHA1InWant`. A temporary bare remote therefore
  CANNOT stage a server refusal; that arm is tested at the runner boundary with the exact message
  a real upload-pack sends. Hosted Git needs `uploadpack.allowReachableSHA1InWant` (GitHub sets it)
  for a non-tip commit to resolve at all.
- `--depth=1` writes `shallow` into the repository's real git dir even under `GIT_SHALLOW_FILE`,
  and `--filter=…` rewrites the repository config (`promisor`, `partialclonefilter`,
  `repositoryformatversion = 1`). Both mutate the cache, so neither may be used to make a proof
  cheap. `GIT_OBJECT_DIRECTORY` isolation fails differently: the repo's refs still point at objects
  the temp store lacks, so negotiation sends `have`s it cannot back and the connectivity check
  fails.

Resolution order is fixed and is the reason failures leave no partial fork: the configured default
head and every companion first (concurrently, each in its own object database), then the selected
source, then `merge-base` between them as the review baseline, then the admitted tree — all before
`ensureSessionWorkspaceContext` runs. No common history is an actionable `invalid_request`, never
an invented ancestor. Nothing checks out or resets the cache; the only writes are objects.

The resolved `session.SourceBinding` (version 1) is journaled into the running create intent with
the existing compare-and-swap BEFORE the fork exists, so a lost response, a restart or a replay of
the same idempotency key reuses exact commits instead of landing on a branch that moved. The same
key with a different selector is a different request and conflicts. See
[[session-operation-intents-cross-versions]] for the intent's two durable phases; a create intent
carrying the retired `pull_request` dialect fails its replay closed rather than resolving as
`default`, which would have moved an approved pull-request session onto the default branch.

Schema v23 adds `source_binding` and leaves the three `pull_request_*` columns as the historical
record. `backfillSourceBindings` derives a binding only where the old row PROVES one — its primary
and pull-request freshness receipts must agree with its own columns on remote, refs, head and merge
base — and skips every other row rather than guessing. A migrated binding has no `admitted_tree`
because no pre-selector session ever recorded one.

The worker connector keeps its own bounded copy of the selector shape (like `responderBinding`)
because `workerconnector` may not import `session` ([[worker-connector]], and the import DAG), and
advertises `repository-source-selector:1` only after the live daemon publishes
`repository_source_selector_versions` — independently of `repository-freshness:2`, so a partially
upgraded fleet never receives selector-bound work. The public binding and
`admitted_source_tree` reach clients only through the hand-written DTOs
([[session-api-dto-is-a-second-projection]]).

## Changelog
- 2026-09-11 — created with the generic source selector (task
  2026-09-09-let-responder-select-and-pin-any-authorized-repo), after measuring git's fetch-by-OID
  short-circuit, local-transport permissiveness, and the `--depth`/`--filter` cache mutations
  against git 2.39.5.
