---
name: host-git-submodule-boundary
description: "host Git never descends under child config; deletion checks children explicitly and dynamic constructors require review"
scope: security
sources: [internal/forkspace/gitcommand.go, internal/forkspace/gitcommand_source_test.go, internal/forkspace/gitcommand_alias_test.go, tools/releasenotes/main.go, internal/forkctl/worktree_clean.go, internal/forkctl/rm.go, internal/cli/fork_cmd.go]
check: "go test ./internal/forkspace -run 'TestHostGit'"
updated: 2026-10-17
---

Host Git on box-written workspaces goes through the shared non-recursive policy and a trusted
view. Status ignoring child dirt is not proof that deleting the workspace is safe: inspect every
populated child through its own view. Removal/recreation must repeat the same recursive check
under the lifecycle lock, after confirmation. Preserve the explicit force override.

The source guard must require exact reviewed exceptions for dynamic os/exec executables,
regardless of variable spelling. Function-value references to command constructors and their
forwarding wrappers are forbidden, with no allowlist escape: reviewing an assignment cannot
approve its later calls. A literal non-Git executable may be excluded. Review the origin and
callers of every exception; naming a variable does not prove what program it runs.

Remote queries also execute transports. Release verification must take the remote from trusted
workflow context and run under fresh private metadata with isolated configuration; a checkout's
origin, URL rewrites or sshCommand must not choose a transport. See [[trusted-git-view]] for the metadata boundary and source-check limits.

The check covers constructor policy and its negative fixtures. Behavioral deletion protection
is also covered by TestForkRmKeepsSubmoduleWork and TestForkFreshKeepsSubmoduleWork; neither
requires a real runtime or host socket.

## Changelog
- 2026-10-17 — second review found an unsafe release-query exception and allowlisted constructor
  aliases. Swept raw constructors and forwarding values: release queries now use private Git
  metadata and an explicit workflow remote; session ACP and lifecyclebench use direct calls with
  optional test injection. Constructor references and forwarding closures are forbidden before
  the allowlist. Copied-tree mutations restore the raw release wrapper and inject direct,
  chained, assigned and closure aliases; each must fail the actual guard.
- 2026-10-17 — independent review found parent-only deletion checks and name-based constructor
  detection. Swept rm/fresh (four checks), landed cleanup and storage reclamation: fixed the four
  rm/fresh checks by sharing landed traversal; storage already inspected children. Audited 21
  newly surfaced dynamic/alias sites: host runtime, Coop re-exec, explicit editor/lease commands,
  fixed provider/system tools and benchmark metadata queries; exact exceptions record each reason.
  Added recursive dirty/untracked tests and generic-variable/alias/mixed-dot-import guard fixtures.
