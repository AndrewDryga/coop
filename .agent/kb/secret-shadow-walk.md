---
name: secret-shadow-walk
description: every launch walks the whole checkout from a real-folder root (a linked root and a link to home are refused, though a box that writes the root's parent can still swap it), gitignored task folders included, to hide secrets; its cost grows with local clutter, so the decider remembers directory verdicts, lstats for .coopignore and pre-filters its globs; the .coopignore files above the repository, short of home, apply too
subsystem: box
sources: [internal/box/mounts.go, internal/shadowpath/shadowpath.go, internal/shadowpath/homepath_darwin.go, internal/shadowpath/homepath_linux.go, internal/shadowpath/shadowpath_test.go, internal/box/run.go, internal/box/repo.go, internal/box/sourcecopies.go, tools/lifecyclebench]
updated: 2026-10-17
---

`box.ComputeMounts` runs on every launch (`run.go`). It walks the whole repository, skipping only
`.git` and directories it hides, and asks `shadowpath.NewDecider` about each path. Everything in
the checkout is visible in the box, gitignored working state included, so `.agent/tasks`
(artifacts, `tmp/` worktrees and clones) is walked too and can't be skipped. A secret in a task's
scratch clone is still a secret.

**The root must be a real folder.** `filepath.WalkDir` lstats its root and never follows a link,
so a repository path that is itself a link was walked as one entry and bound whole: no decoy, no
`.coopignore`, every secret readable in the box. A `COOP_REPO` naming a link did exactly that
until 2026-10-16. `box.ResolveRepo` now follows a linked override to its folder (an unresolvable one
is kept as given), and `ComputeMounts` refuses a root that is still a link or a file. For the walk,
a link in a parent folder (macOS `/var` → `/private/var`) is harmless: only the last component
decides. `TestRunHidesTheSecretsOfALinkedRepository` launches through a linked override and checks
both.

**A link may not lead to the home folder.** `ResolveRepo` refuses an override that a link, in its
last part or an earlier one, takes to the home folder or a folder above it (`HomeOrAbove`).
Otherwise a box that can write the folder above a nested checkout could swap it for a link to
`$HOME`, and the next launch would mount every file you own with only the decoys hiding anything.
An override spelled as `HOME` spells the home folder, or as a folder above it, is still taken when
a link in that spelling changes its path (macOS `/var`, `/home` on some Linux systems); one that
reaches the home folder through any other link is refused.
`TestResolveRepoRefusesALinkToTheHomeFolder` covers both.

"Above" counts every path to the home folder, not only its real one: `HomeOrAbove` also walks up
the paths a second mount gives. On macOS that is the mount point of each volume holding home's
real path or a folder above it, followed by that path, through real folders, so the data volume
counts (`/System/Volumes/Data`, `/System/Volumes` and `/System`) for a home under `/Users` and for
one on a volume mounted under `/Volumes` or inside home; no other volume is touched, so a stalled
disk image elsewhere cannot hang a launch. On Linux it is every other mount of home's filesystem
in /proc/self/mountinfo, a bind mount's source included (with `/data/home` bound at `/home`,
`/data` counts). Those paths come from the mount table, never from the folder asked about, so
nothing a box plants below that folder can add one. Known limits: a copy of home served by another
filesystem (bindfs or sshfs, an NFS export with its own cache, an overlay layer) never counts, and
NFS reports `/` as every mount's root, so a second mount of the same NFS export that holds home is
not seen either; a box can make none of these. The walk for the `.coopignore` files above a project
(`enclosingPolicies`) stops on home's real path only (`homeOnRealPath`): there, counting more
folders would read fewer `.coopignore` files. `TestHomeOrAboveCountsTheMacOSDataVolume`,
`TestMountedPathsFollowsHomesFilesystemToItsOtherMounts` and `TestParseMountinfoReadsDevicesRootsAndEscapedPoints`
pin it.

**The limit: the root guard does not protect a root whose parent a box can write.** The guards
check the root by path, and the runtime binds that path later: Coop hands `docker run -v` a path,
never a folder it holds open, so nothing binds what was checked. A box that can write the folder
above the root, as one working in a project can for a repository nested in it, a
`.claude/worktrees/*` checkout or a task's `tmp/` clone, can swap the root for a link just after
the walk, and the runtime binds whatever the path names then. Before a launch it can also swap the
override for a link to any other folder, `~/.config` say, which then mounts read-write with only
the decoys hiding anything. That is an escape, not a leak: the next box can write
`coop/coop.conf` there with `COOP_RUN_ARGS=-v /:/host`, and the launch after it gets the host's
root. Backlog item 2026-10-17-decide-how-coop-treats-a-coop-repo-whose-parent holds the decision.
The working-directory fallback has the same gap: a shell that enters the swapped folder through
its link has the home folder as its working directory, and when no `.git` sits at or above it,
`ResolveRepo` takes that folder as the project (task
2026-10-16-let-coop-login-and-coop-doctor-run-without-a-pro).

**Cost follows local clutter, not the repo.** On 2026-10-04 this checkout held 44k files in 12k
directories, 41k of them under `.agent/tasks`. The walk took 4.1 s, while a clean clone of the same
commit took 0.05 s, and a filtered warm start took 8.1 s against the clone's 3.1 s. Two costs
multiplied:

- `shadowed` re-decided every ancestor of every path: 64 secret globs plus the `.coopignore`
  chain at each level.
- `loadDir` opened the repository root and every component, looking for a `.coopignore` that
  almost no directory has.

`NewDecider` now remembers each directory's verdict (a path costs its own check plus one lookup)
and lstats before the safe read (only "not there" skips it). `shadowed` stays the plain definition
that the `Snapshot` uses, and `TestDeciderMatchesShadowed` pins the two together, including links
the safe read refuses.

**The rest was CPU, not disk** (measured 2026-10-06, 40k paths in 6k directories, warm). The walk's
own directory reads took 0.12 s; deciding took 0.47 s, of which `filepath.Match` against the 64
secret globs was 0.31 s (7.8 µs a name) and re-walking every ancestor's `.coopignore` level was
most of the rest. The decider now matches through a `globSet` (a pattern's literal start and end
rule out almost every name before `filepath.Match`, which still decides the rest;
`TestGlobSetMatchesFilepathMatch` pins it) and consults only the ancestors whose `.coopignore` has
rules. A pass over the 40k paths went from 518 ms to 67 ms, alternated under the same load, with
the same answer for every path. Don't cache directory listings between launches to win the last
0.1 s: the cache would be a stored file the box must never reach, guarding the code that hides
secrets. Profile this walk with direct timing; the macOS CPU profiler pins most of it on syscalls.

**Rules from above the repository.** A box can make any folder it writes a repository by planting
a `.git` there (a folder or a gitfile), and a command run from inside takes that folder for the
project (`box.FindRepo` takes the nearest `.git`, as git does). So `NewDecider` and `OpenTree` also
carry the `.coopignore` of every folder above the repository, stopping at the home folder or a
folder above it (`HomeOrAbove`, shared with FindRepo). Base patterns carry unchanged. Path patterns
are matched against the repository's path from their folder plus the path (`enclosingPolicies`).
The walk never looks for the enclosing repository's `.git`, which a box can move away; it keys on
the `.coopignore` files, which every box mounts read-only. The folder's owner decides whether its
`.coopignore` counts, never the file's: one in a folder another user owns is passed over, since
anyone can create `/tmp/.coopignore` or hard-link one of your files there so it looks like yours,
while one saved with sudo in your own folder applies. Root's `/tmp`, `/private` and `/Volumes`
therefore drop out. Only root can give a file away, so `TestRulesAboveFollowTheFolderOwner` stands in owners
through the `ownerOf` variable. Nothing else carries: neither the built-in secret names of the
folders above (a project under `~/work/secrets/` must not vanish) nor a rule that hides the project
folder itself (the root is never hidden). A box cannot plant a `.git` in a folder it sees only as a
read-only decoy. A `.coopignore` that is there but is not a regular file of at most 1 MiB coop
can read (a link, a folder, a FIFO, no read permission, too large), above the repository or inside
it, is `shadowpath.UnreadableCoopignore`: the decider (`NewDecider`, now `(bool, error)`) answers
every path it covers "hidden" with that error, as a snapshot does. Launches, service binds and the
build context refuse it; every command draws it the same way (`asUsage`, `reportUnreadableCoopignore`,
and through it `buildFailure`, `updateBoxFailure` and `coop up`'s `composeRefusal`): "Could not read
<file>", escaped, with "Remove it if you did not create it" first in the fix. The loop stops at the
first launch that meets one (`stopsTheRun`) rather than retry it into "Stopped after 5 failed
attempts", which would hide the file's name. `coop fork merge` warns (needs --force) and still
names a file whose name, or a folder name above it, alone marks it as a secret (`BuiltinSecret`,
the decider's own per-name check, `secretName`); the isolated publication refuses; session review
scans report it once; a preset file under it fails naming the .coopignore, not the preset, escaped
in `coop presets` too. `TestNestedRepositoryKeepsTheRulesAboveIt`
(box) covers a planted `.git` folder, a gitfile and a real submodule through the mount plan and a
source copy.

The prefix keeps the spelling of the repository path it was given. FindRepo uses the disk's own
spelling, but a `COOP_REPO` typed in another case on a case-insensitive disk (`~/work/App` for
`~/work/app`) builds the prefix in that case, so a wildcard path rule from a folder above
(`app/config/*.yml`) misses. Base patterns, and the final name of a literal path rule, still match.

Tests reach this walk from every repository they create under the machine's temp folder, so a
stray `.coopignore` above it changes their answers: one holding `*` in TMPDIR failed 50 box tests
and hung another. The box tests that depend on what is hidden pin HOME to a temp folder
(`pinHome`), which keeps the walk inside the test's own temp folders. To find a new one, run the
package with TMPDIR pointing at a scratch folder whose `.coopignore` holds `*`.

**Measuring start time:** `make lifecycle-bench` on a clean clone gives the product's number. A
cluttered checkout measures its clutter. After the globSet change (2026-10-06, before and after
builds alternated, load 8 to 10), a filtered start in this checkout took 2.62 s against 3.18 s,
and a clean clone 2.30 s; on 2026-10-04 the same checkout took 8.1 s.

## Changelog
- 2026-10-17 — `HomeOrAbove` also walks up the other paths the mount table gives to home: every
  local macOS volume's mount point plus home's real path (the data volume) and Linux mounts of
  home's filesystem from /proc/self/mountinfo; the `.coopignore` walk keeps the real-path check
  (task 2026-10-17-count-a-folder-that-holds-home-through-another-m).
- 2026-10-17 — an unreadable `.coopignore` no longer drops its rules: the decider returns
  `UnreadableCoopignore` for the paths it covers and every caller refuses, warns or reports it
  (task 2026-10-17-refuse-a-coopignore-the-secret-decider-cannot-re). Checked against
  shadowpath.go, box/mounts.go, box/serviceshadow.go, cli/checksecrets.go. Second review round: the
  loop stops at the first refusal, `coop up` and `coop presets` draw it escaped, and `BuiltinSecret`
  checks folder names too.
- 2026-10-04 — created with the decider memo and lstat precheck (task 2026-10-04-find-why-a-warm-start-takes-8-s-in-a-checkout-wi).
- 2026-10-06 — the remaining cost was glob matching, not directory reads: globSet and policy chains (same task).
- 2026-10-16 — `make lifecycle-bench` runs the Go port, tools/lifecyclebench (task
  2026-10-15-port-the-lifecycle-and-services-benchmarks-to-go); nothing else was rechecked.
- 2026-10-17 — the `.coopignore` files above the repository, short of home, now apply too, in the
  decider and in snapshots (task 2026-10-16-keep-a-box-planted-nested-git-from-dropping-the).
  Checked against shadowpath.go, box/repo.go and box/sourcecopies.go. After review: the folder's
  owner decides, not the file's; the COOP_REPO case limit is noted; box tests that depend on what
  is hidden pin HOME, found by running the package under a temp folder holding a stray `*` rule.
- 2026-10-16 — a linked repository root: ResolveRepo follows a linked `COOP_REPO`, ComputeMounts
  refuses a root still a link or a file (task 2026-10-16-resolve-links-in-coop-repo-so-secret-decoys-stil).
- 2026-10-17 — the read-only and bare launches are gone; run.go is the only caller of ComputeMounts.
- 2026-10-17 — ResolveRepo refuses an override a link takes to the home folder or above; the
  parent-writable limit, bind-by-path included, is stated (task
  2026-10-17-refuse-a-coop-repo-reached-through-a-link-that-r). Checked against box/repo.go,
  box/mounts.go and shadowpath.go.
