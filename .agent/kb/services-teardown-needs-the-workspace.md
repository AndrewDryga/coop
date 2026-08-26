---
name: services-teardown-needs-the-workspace
description: sibling-service teardown is driven by the workspace's own compose file, so stopping services after deleting the workspace is a silent no-op
subsystem: box/services
sources: [internal/box/services.go, internal/box/repo.go, internal/forkctl/rm.go, internal/forkspace/create.go]
updated: 2026-08-25
---
Sibling services are brought up per box (`box.Run` → `EnsureServicesFile`) and are deliberately
**not** brought down when a box exits: the stack is idempotent and reused across iterations, so a
loop doesn't pay Keycloak's startup on every task. Persistence across boxes is correct.

What is NOT correct is persistence past the WORKSPACE. Teardown is driven by the workspace's own
compose file:

```go
DownServices(rt, workspace, policyRepo, …) → ComposeFile(workspace, policyRepo) → ComposeFileAt(…)
// ComposeFileAt returns "" when the file is missing/empty  →  DownServices returns nil, doing NOTHING
```

So teardown after `os.RemoveAll(workspace)` doesn't fail — it **silently succeeds having done
nothing**. Observed 2026-08-03: a fork's `keycloak` + `postgres` were still running five days after
`coop fork rm`, holding disk and a published port the whole time.

**The rule: stop services BEFORE the workspace is deleted.** `forkctl.DestroyFork` (`internal/forkctl/rm.go`)
does this at the single choke point every destroy path funnels through (`fork rm`, `fork merge`,
`merge --all`, and the `fork create` rollback), with `volumes: true` — a fork is
disposable by definition, and leaked volumes were a real share of the disk growth. It is best-effort
and logs rather than blocking: a service that won't stop must not veto the removal the operator
asked for.

The ORDER is why the two halves are split: the removal itself is `forkspace.Destroy`
(`internal/forkspace/create.go`, a leaf with no container runtime), and `forkctl.DestroyFork` is the
box-aware wrapper that runs teardown first and then calls it. Anything that reaches for
`forkspace.Destroy` directly skips the teardown and re-opens this bug.

Two related seams behave correctly already and are worth copying rather than duplicating:
- `internal/sessionsvc/service.go` downs services itself before planning a discard, so `discardSessionWorkspace`
  passes a zero `runtime.Runtime{}` into `forkctl.DestroyFork` to avoid a second teardown.
- `StopSessionServices` removes by immutable compose LABELS instead of the file, so an agent's
  interrupted edit to `compose.yml` cannot block cleanup. Prefer that shape when the file's
  trustworthiness — not its existence — is the risk.

`runtime.Runtime` is a struct, not an interface: guard with `rt.Name != ""`, never `rt != nil`.

## Changelog
- 2026-08-25 — removed the retired Fleet prune path from the current destroy-path inventory;
  direct fork removal, merge, merge-all, and create rollback still share the same teardown-first
  choke point.
- 2026-08-10 — `destroyFork` moved out of `internal/cli` with the rest of the fork lifecycle and is
  now exported as `forkctl.DestroyFork` (`internal/forkctl/rm.go`); the ordering, the `volumes: true`,
  and the best-effort logging are byte-identical. Every destroy path still funnels through it —
  `fork rm`/`fork merge`/`merge --all`/fleet prune were all inside `internal/forkctl`, and
  `internal/cli`'s `--fresh` rollback calls the exported one.
- 2026-08-09 — re-verified after the sessions extraction: the discard path is now `internal/sessionsvc/workspace.go` calling `forkspace.Destroy` DIRECTLY (its `destroyFork` call always passed a zero runtime, so the wrapper was doing nothing for it), which makes the ordering rule above explicit — the service downs the services itself, first.
- 2026-08-09 — re-verified after the `internal/forkspace` extraction: the workspace removal moved to
  the leaf (`forkspace.Destroy`), the service teardown deliberately did NOT, so cli's `destroyFork`
  is now the wrapper that keeps the order. `TestDestroyForkStopsServicesBeforeRemovingTheWorktree`
  stays in `internal/cli` because it is the ORDER it proves.
- 2026-08-03 — created: removed forks were leaving their compose stacks running because teardown ran after the worktree was deleted, where the missing compose file made it a silent no-op.
