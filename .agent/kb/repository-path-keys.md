---
name: repository-path-keys
description: what Coop keys by the repository's path, which keys resolve links first and which take the spelling as given, what a spelling change moves (forks, images, serve ports, agent history), and why paths recorded in fork state match by folder
subsystem: box
sources: [internal/box/repo.go, internal/forkspace/forkspace.go, internal/box/image.go, internal/box/derived_image.go, internal/box/run.go, internal/box/history.go, internal/project/project.go, internal/tasks/lease.go, internal/tasks/identity.go, internal/tasks/queue.go, internal/tasks/proposal.go, internal/tasks/projection.go, internal/tasks/assignment.go, internal/tasks/assignment_registry.go, internal/tasks/dir.go, internal/tasks/snapshot.go, internal/loop/lock.go, internal/networkstate/authority.go, internal/cli/commands.go, internal/cli/context.go, internal/cli/doctor.go]
updated: 2026-10-17
---

`box.ResolveRepo` names the project for nearly every command: the folder `FindRepo` walks to from
the kernel's working directory, which never names a link, or `COOP_REPO` followed through links
(`EvalSymlinks`; an override that cannot be resolved is kept as given, and one a link takes to the
home folder or above it is refused, see [[secret-shadow-walk]]). Outside any repository it is the
kernel's working directory, never the home folder or a folder above it (`homeProjectRefusal` ends
both refusals). `coop login` and `coop doctor` need no project: login's box works in an empty folder
of its sign-in stage and mounts none, and doctor (`doctorProject`) falls back to the shared image
and skips the box survey, saying why. That string feeds two kinds of key.

**Resolved before use**, so a different spelling of the same folder keeps them: Compose project and
network names and sidecar port scopes (`ComposeProject`, `canonicalWorkspace` in repo.go), native
agent homes and their history key (`HistoryKey`), task leases and queue identity
(`LeaseAuthorityKey`, `canonicalTaskRoot`), the loop checkout lock (loop/lock.go), network
approvals (networkstate/authority.go, matched by inode) and git views.

**Taken as spelled**, so a different spelling moves them: the fork home `<repo>-forks` and all fork
state in it (`forkspace.Home`); project image names, built from the folder's base name
(`ServicesProject`, used by `ImageForRepo` and `filteredProjectTag`); `serve.ports` host ports
(`project.HostPort`); the box workdir, which is the agent's cwd and so the key of each agent's own
conversation history (`Workdir` in run.go), and the folder-trust entries written for it; and the
interactive session lock (`lockSessionProducer`).

**Recorded in fork state**, absolute paths keep the spelling they were written in: a task
assignment's projection and canonical queue root (the owner record and `ForkAssignmentIndex`).
Coop matches them to the paths it derives now by folder (`box.SameFolder`), never as strings.
`forkProjectionRel` and `ProjectionQueueRel` take the box-writable parts below the fork workspace
as text and match only the host-owned workspace; `sameForkAssignmentIndex` and the snapshot behind
`coop tasks watch` and `coop fork ls` (`ReadProjectSnapshot`, which keys its queues by folder)
match the queue root. The projection is opened under the workspace Coop derives. So a fork that
took its task while `COOP_REPO` passed through a link above the project still resumes, merges,
lands, shows once in the snapshot and can be removed. Reads of the recorded queue root, and
`LatestForkTaskLog`'s read of the projection, still go through the recorded spelling, so that link
has to stay.

Paths a person hands Coop keep their own spelling: the shell's working directory (`os.Getwd` is
`$PWD`) and an absolute `COOP_TASKS`. Place them with `box.RepoRelative`, which matches folders
rather than spellings, never `filepath.Rel` against the repository (`coop context` and
`TaskQueues` use it).

**The trap:** two spellings of one folder split the second group. Until 2026-10-16 a `COOP_REPO`
naming a link kept the link's spelling while a walk from inside the folder gave the real path.
Resolving the override moved a link user's agent history and serve ports to the folder's path, and
when the override itself was the link, its forks (left in `<link>-forks`) and images too. A new
check that compares a recorded fork path with a derived one as strings strands every fork that took
a task through a parent-folder link, so match folders. Tests that hand Coop a `t.TempDir()`
repository through `COOP_REPO` must resolve it first on macOS, where `/var` links to
`/private/var`, or each expectation built from it spells the project differently from Coop.

## Changelog
- 2026-10-17 — the working-directory fallback refuses the home folder and above; login and doctor
  run without a project (task 2026-10-16-let-coop-login-and-coop-doctor-run-without-a-pro).
- 2026-10-16 — created from a survey of every `ResolveRepo` consumer (task
  2026-10-16-resolve-links-in-coop-repo-so-secret-decoys-stil).
- 2026-10-17 — added serve ports and `box.RepoRelative`, found by the macOS test run (same task).
- 2026-10-17 — recorded fork paths now match by folder, so a fork assigned through a link above
  the project keeps working; dropped the `coop fork rm` trap the string comparison caused.
- 2026-10-17 — `box.SameFolder` for recorded paths: projections are split as text with only the
  workspace matched, and `ReadProjectSnapshot` keys queues by folder (re-review of the same task).
- 2026-10-17 — an override a link takes to the home folder or above is refused (task
  2026-10-17-refuse-a-coop-repo-reached-through-a-link-that-r).
