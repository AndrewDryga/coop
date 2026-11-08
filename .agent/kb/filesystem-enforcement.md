---
name: filesystem-enforcement
description: APFS (Mac) and ext4/xfs (Linux) Docker admissions enforce secret objects and Unicode path rules with packaged BPF hooks, guarded execution and durable cleanup custody; systemd slices under Docker's systemd cgroup driver
subsystem: box
sources: [internal/cli/tempreap.go, internal/cli/net_approve.go, internal/box/caseapproval.go, internal/box/run.go, internal/box/filesystem.go, internal/box/filesystem_controller.go, internal/fsfilter/controller.go, internal/fsfilter/assets/shared.h, cmd/coop-fs/controller_linux.go, internal/box/filesystem_controller_live_test.go, internal/box/filesystem_diagnostics.go, internal/box/filesystem_snapshot.go, internal/box/filesystem_walk.go, internal/box/filesystem_execution.go, internal/box/filesystem_services.go, internal/box/filesystem_recover.go, internal/box/filesystem_probe.go, internal/box/hidden_files.go, internal/fsfilter/policy.go, internal/fsfilter/unicode.go, internal/fsfilter/protocol.go, internal/fsfilter/assets/filter.c, internal/fsfilter/assets/loader.c, internal/fsimage/image.go, internal/fsimage/Dockerfile, cmd/coop-fs/main_linux.go, cmd/coop-fs/admit_linux.go, internal/cli/doctor.go, internal/box/filesystem_live_test.go, internal/box/filesystem_partial_live_test.go, internal/box/filesystem_preload_live_test.go, internal/box/filesystem_normalizer_live_test.go, internal/box/testdata/filesystem_normalizer_fixture.c, internal/runtime/docker_image.go, internal/runtime/docker_lifecycle.go, internal/runtime/docker_execution.go, internal/box/filesystem_cgroups.go, internal/box/filesystem_profile_linux.go, internal/systemdbus/systemdbus.go, cmd/coop-fs/slices_linux.go]
updated: 2026-11-07
---

## Selection and policy

Agent and final service launch share the filesystem admission. Automatic selection requires host
APFS (Mac) or ext4/xfs (native Linux; `filesystemProfileCandidate`), Docker cgroup v2 with the
cgroupfs or systemd driver and no rootless/userns (`filesystemCgroups`), the BPF LSM in the host's
`/sys/kernel/security/lsm` on Linux (`filesystemKernelReady`), and the expected BTF hook signatures.
A Linux volume that supports ext4 casefolding (probed by setting FS_CASEFOLD_FL on a scratch
folder) is profiled as folding and normalizing, since an agent could casefold a folder it makes.

**systemd slices (2026-11-07, owner decision: create the slices through systemd).** Docker's
systemd driver accepts only a slice as `--cgroup-parent`, and systemd owns every slice. The
controller record then carries `CgroupDriver: "systemd"`; the controller is the top-level slice
`coop_fs_controller_<hex>.slice` and each admission `coop_fs_controller_<hex>-coop_fs_<hex>.slice`
inside it (dashes nest, so names use underscores; `fsfilter.ControllerSlice`/`AdmissionSlice`). The
helper (`COOP_FS_CGROUPS=systemd`, the system bus socket mounted, `apparmor=unconfined` because
Docker's default AppArmor profile refuses every system-bus message) starts each transient slice
with `StartTransientUnit` before the loader attaches, and waits for its cgroup. Retirement runs the
loader as a child: its `rmdir` proves the cgroup empty (EBUSY otherwise), and only then
`StopUnit` removes the unit, because stopping a slice stops every unit inside it. Descendant
checks treat `A-B-x.slice` as inside `A-B.slice`. systemd hosts keep the hierarchy root 0555, and
projects can hold private folders, so the helper also has DAC_OVERRIDE. Kernel 7.0's verifier
refuses a context pointer used as a map key or moved before a store; `release_owner` copies its
key to the stack and `hierarchy_root` is fully unrolled. On native Linux the image layers share
the project's disk, so a dead owner denies the box's own programs too (fail closed). ext4 and xfs
reuse inode numbers (APFS does not), which the (ino, dev) object keys could confuse; the owner keeps
every labelled object open (`pins` in loader.c), so no labelled number is freed while the policy
lives, and a dead owner denies everything. Since
2026-11-03 (owner decision) launches no longer prove the actual share: a per-launch probe folder
in the project showed up in editors and cost a container start. `coop doctor` runs the
kernel-only checks instead (below). Unsupported runtimes keep spelling mounts and existing case-exposure
approval. A failed cleanup is never permission to fall back: unresolved custody stops the launch.
Only `coop approve --case-exposure` reviews and remembers mount-fallback exposure. Ordinary
`coop approve` handles network access without calculating or granting file exposure; it cannot
predict the images and options of future launches. Qualified BPF admissions ignore this waiver.
If mount preparation also fails, `FilesystemFallbackError` retains the bounded backend-selection
reason beside that refusal. The CLI preserves the mount remedy; joined cleanup failures use the
complete generic error instead. Successful fallback stays quiet, and `FilesystemReport` still
reports the selected backend to diagnostics.
The helper image builds solely from sources embedded in the Coop binary; its compiler/libraries
stay inside Docker. `make filesystem-sources-check` detects stale embedded sources.
A failed helper build reports the failed step's own lines from its private log before cleanup
deletes that log (`filesystemBuildDetail`). The log's end cannot be used: BuildKit's footer repeats
the command, excerpts the Dockerfile, and keeps only the step's last ten lines, which lost the
first compiler error in the November 2 failure.

One host walk applies the shared shadow-policy decider and remembered identities. Policy discovery
prunes hidden descendants and .git as the mount backend does, while identity inventory visits both
to close existing hardlink aliases. It persists
semantic origins under the shared hidden-file record lock before policy publication. A rejected
admission therefore cannot forget old identities after a host atomic save. Legacy mount
persistence preserves prior omitted identities at surviving aliases instead of erasing them.

Don't judge a path by the guest kernel's link count. On OrbStack `i_nlink` can stay stale after the
host adds a hard link to a warmed path (host count 2, kernel count 1, read allowed), and only in some
runs, so one passing run proves nothing. Admission uses object identities for this reason.
Removing a rule declassifies its original object only when no remembered alias survives.

Host device/inode values group aliases only. The loader opens the actual guest objects with
O_PATH and retains those descriptors; guest device/inode keys are never translated from host
metadata. Unicode matching uses full case folding and canonical decomposition with combining
class ordering, without inserting normalization stream-safety characters. Hashed path matches
can only add denial, never grant access. Explicit service-file grants are identity-only read
access applied after protected entries; policy files and directories cannot be granted.

The helper image generates its immutable Unicode wire table at build time. Admissions compile
unique exact path/ancestor rows (never normalized hashes as allow authority), followed by all
grants. Fresh-owner creation and controller acquisition overlap the host inventory.
Only this branch writes custody, and both branches settle before publication or failure/panic
cleanup. Sparse path/object maps
retain the accepted upper bounds without preallocating every element. Host capture and remembering
finish before atomic policy publication. Go supervises the C loader and pipes the encoded policy;
C alone pins descriptors and retains its exact task reference. Exact EOF, map freeze and durable
run birth precede publication into the controller tables and READY. Child failure is observed during publication; its timeout
never limits the admitted owner lifetime. Each real guest identity stays pinned until enforcement
is revoked. The helper descriptor limit covers the accepted 100,000 pinned objects plus
map/program/root descriptors; Docker defaults must not silently lower that policy limit.
Final pre-execution revalidation remains fresh.

Inventory uses at most eight directory workers, bounded read batches and one million entries.
Each opened directory must match its discovered identity. No symlink is followed, no hidden or
`.git` subtree is omitted, and cross-device directories fail. The coordinator owns the resulting
map and restores WalkDir ordering before policy classification. A complete fresh inventory lets
the decider skip searches for absent `.coopignore` files; policies that exist still use the safe
reader. Built-in basename verdicts are memoized only within that decider.

On qualified APFS, final validation lists every name/type and stats every directory plus every
protected object, grant and ancestor. It reclassifies current names and refuses new protection
or changed protected identities. Public leaf identities need not be fetched again: the loader
still holds every initially protected object, so a new public alias inherits its denial. A change
to remembered identity authority falls back to full inventory. Other filesystems keep full
revalidation, including Linux leaf mount device checks.

Nothing is written into a root. The case/Unicode profile (`fsfilter.Profile`) is measured in the
admission's private state folder when it shares the root's device: APFS case sensitivity and
normalization belong to the whole volume. A root on another device gets both equivalences, so the
policy may deny spellings that volume would not resolve but never misses one it would.

FSEvents is not authoritative evidence that a retained inventory is current. Its flush can
return before completed filesystem changes are reported, even after draining the callback
queue. [Watchman documents the same unsupported synchronization guarantee](https://github.com/facebook/watchman/blob/main/website/docs/cookies.md#limitation-macos-fsevents).
The November 2026 prototype reproduced this on 100 fsynced creates. A live index would still
need fresh authority; a settling delay or watcher restart fallback does not close that gap.

## Trusted-host snapshot contract

The host and external editor are trusted. Public edits/hardlinks/moves work normally. Existing
protected destinations remain hidden across in-place writes, atomic replacement and replaced
ancestors. Restart/re-admit after changing policy, classifying an already-public object as secret,
or adding an unprotected host alias to secret bytes. Existing readable mappings are not revoked.
Prepare secret atomic-save temporary files outside the share or inside a hidden directory.
Service read grants admit the object, not frozen bytes: in-place edits remain readable; replacement
requires new admission. See docs/box.md for the user contract.

## Enforcement and lifecycle

The packaged loader validates 27 LSM hook signatures before loading. Global attachments apply
only to the frozen cgroup parent and its descendants. Secret reads, xattr/ACL/`fileattr` metadata
and endpoint mutations are denied; .coopignore is readable but cannot be changed, including through
aliases. `inode_getattr` alone lets a hidden non-directory leaf through (operation `STAT`), because
tools stat every listed name and Terraform refuses a module folder otherwise. An LSM hook can only
allow or deny, so it cannot report the mount backend's empty-file size: stat shows the real size,
times, mode, owner and link count. Hidden ancestors, hidden directories, a dead owner and a missing
normalizer still deny stat. `inode_file_getattr` (lsattr) keeps the read rule; stat never calls it.
Terraform v1.17 stats `terraform.tfvars` and then opens it, and LSM can only allow or deny. Since
2026-11-04 (owner decision) an eBPF launch also binds an empty read-only file over each hidden regular
file from its snapshot, one mount per real name, capped at 1,000 and at `maxCoverBytes`
(`withHiddenFileCovers`). The cover is a separate object, on a source the policy does not guard, so it
opens as empty. In a repository only Git-ignored files get one, because `git add -A` would commit a
tracked or new file as empty (seen for real). eBPF stays the boundary: other spellings, aliases, later files and anything past the cap
are denied. A hidden directory beside the `.tf` still fails. Measured: 1,000 covers add about 0.2 s.
GNU `ls -l` also asks for xattrs/ACLs of each name, so on a hidden non-directory `inode_getxattr` and
`inode_get_acl` answer ENODATA and `inode_listxattr` answers EOPNOTSUPP (a list call has no "no such
attribute"; gnulib treats ENODATA there as a real error) via `xattr_read` and the QUIET_* verdicts. Hidden
directories, anything below them and a dead owner keep EPERM, and set/remove stay EPERM. `inode_readlink`
keeps the read rule, so a hidden symlink shows no target (`ls -l` on one still errors). `find` stays clean.
Private writable mappings of read-only policy are allowed because they cannot change host bytes.

The same C source builds the guards and one fresh filename-normalization EXT program per
controller generation. The loader reuses that generation's exact `work` and `runes` maps and
attaches the replacement to every guard before attaching LSM hooks. Its scalar call looks up
existing current-thread scratch; missing scratch or replacement denies. The base stub preserves
all four arguments and a compiler memory clobber because the replacement writes scratch through
its map lookup. The final depth bound stays adjacent to the indexed store, after `bpf_loop`.
Only C owns the unpinned replacement links; ordinary cleanup destroys them after the durable LSM
links, before either object closes. C death drops replacements while durable guards retain the
existing owner-death denial. No replacement custody is transferred to the host or another run.

An exact task reference to each inode owner and the controller detects their deaths. Pinned links survive it and deny
all access on admitted share devices until retirement. The unprivileged workload gets no BPF
capability or cgroup control. Its fixed read-only guard checks the VM boot identity before
executing the image entrypoint/command; service healthchecks use the same guard. Ordinary image
healthchecks currently use the fallback backend because Docker's CLI health command invokes an
image-owned shell before the guard. Compose command/healthcheck extraction undoes `$$` once;
image defaults remain literal.

One release-owned controller stays warm per immutable helper image and Docker daemon. Its frozen
scope map names the exact root; a separately frozen depth comes from the real cgroup-mount ancestry
(the VM mount need not be the global hierarchy root). Four outer maps select fresh frozen inner
maps by admission cgroup ID, including deeper descendants. Missing or partial policy denies.
No workload receives controller state or BPF descriptors.

The owner HASH has an ordinary publication state and a fresh token beside its task kptr. Recycled
kernel hash slots may retain kptr fields even after deletion; userspace map updates skip those
fields. A non-null kptr is therefore not readiness. Enrollment atomically claims unpublished
state, exchanges/releases the recycled reference, then publishes ready. Readers acquire that
state before task liveness checks. Retirement explicitly clears publication and releases the kptr
before deleting the entry; it never relies on deferred allocator destruction. Duplicate or
wrong-token publication and live-owner revocation are refused.

Registry then generation is the only controller lock order. Enrollment holds a shared generation
lease; retirement needs its exclusive lease plus no durable admission references and no stopped
or running descendant containers. A dead controller is never restarted: fresh launches and
retained-service replacements use a new root, while old admissions stay denied. Only unused dead
or obsolete generations retire; the current live generation stays warm. Controller birth precedes
pins; run birth precedes shared-table insertion. Recovery checks exact object kinds, IDs, schemas
and known pin names before mutation. Missing birth after READY retains custody.

The private record stores frozen endpoint/daemon identity, immutable images and consumers,
creation intent, supervisor identity, READY/birth receipts, probe custody and a held lease.
Birth is synced before the first LSM attachment. Recovery accepts only the recorded pin directory,
program IDs and link types; unexpected pins retain custody. Removal order is consumers, proof
that the parent has no remaining Docker references (stopped containers count), owner, kernel
release, exact inert probes and finally private state. Unknown outcomes retain records.
Consumer inspection uses bounded batches. If a batch fails and a fresh same-daemon inventory
proves one of its IDs disappeared, retirement restarts the entire inventory under one deadline.
Stable failures, malformed observations and newly discovered stopped consumers retain custody.
An inventory that meets that deadline is `runtime.ErrConsumerInventoryUnfinished`, reported once.
At the end of a run (`keptFileProtection`, in the filtered cleanup and the unfiltered finish) it
keeps custody like any failure, prints one warning line and leaves the run's status to its command:
the next run's `RecoverFilesystemAdmissions` in that workspace retires the record.

Filtered launches prepare the trusted filesystem owner and network helpers concurrently. Both
branches finish before cleanup or workload creation; errors never abandon an in-flight branch.
After creating the stopped workload, final filesystem validation overlaps final network checks.
Both must pass before authorization and attach. Native credentials are revoked before filesystem
retirement. Filtered cleanup confirms the shared agent is gone, then overlaps filesystem
retirement with the remaining network cleanup. Both branches settle before the network receipt
is sealed or the run returns; composition artifacts survive the join. Early preparation failures
and panics retain the same local-admission cleanup fallback. Container inspection uses the frozen Docker socket with daemon checks on both sides;
an HTTP failure or malformed response is unknown, never absence. Exact noninteractive container starts and force removal also use this bound socket, preserving
never-started checks and fresh postconditions; ambiguous removal succeeds only after proven absence.
Image identity, layer chains
and execution defaults also use fresh bounded socket observations, with no mutable-tag cache.
Full image metadata has a separate wire bound; each selected projection retains its 64KiB bound.

Service admissions outlive the starting process. Unchanged readonly/no-repo-bind services reuse
the same helper/consumer; changed service config or protected identities obtain a new admission.
The existing writable-service launch barrier is preserved. A failed start must not stop a reused
sibling. Resolve private state-root aliases before comparing control mounts (`/var` versus
`/private/var` on macOS). Recovery racing reuse can legitimately remove a candidate between the
scan and lease acquisition; absence means continue, other errors remain errors.

Recovery is per workspace (`RecoverFilesystemAdmissions`, from a service start or stop), so custody
on a workspace that is gone was out of reach: a host setup check's scratch folder, a deleted
worktree. The daily sweep (`cli.reapTemp`) now asks with no workspace, which selects only records
whose `Workspace` reports "no such file" (`filesystemWorkspaceGone`); a path it cannot check stays.
The usual rules still apply: this host's supervisor, a free lease, a supervisor provably gone for a
run or every consumer absent for services. A helper whose workspace exists is never that pass's,
even with a dead supervisor. A `Pending` create proves nothing while Docker may still finish it, so
removal keeps reporting it until `unfinishedCreateSettle` after the record's last write
(`pendingCreateSettled`), then treats the absence as a create that never landed. A create Docker
refused (`runtime.DockerCreateLeftNothing`) clears `Pending` at once, so the failed launch removes
its helper and state folder itself. A record's scope
check allows a gone workspace, since a path spelled through a link no longer resolves to its scope;
an unreadable record would also block every controller recovery (`controllerReferenced`).

A workspace's pass tries every controller's recovery but reports a failure only for a controller
that an admission bound when the pass began (`referencedControllers`). Recovery binds only the
recorded endpoint and daemon, so a controller whose endpoint is gone (a test's proxy socket, or
Docker Desktop no longer running after a move to OrbStack) keeps its custody. Only the host-wide pass
(no workspace) returns it, and the daily sweep that runs that pass keeps quiet. Before 2026-11-07, every
session's service stop warned about it, and a service start with filesystem admissions
(`startServicesFileContext`, `preparedFilteredServices.start`) failed over it.

Records written before 2026-11-03 list `.coop-fs-probe-*` folders the old per-launch probe made
at each root, so the record keeps an `omitempty` `Probes` field and retirement still removes one a
crash left. That cleanup does not compare persisted device numbers or recursively delete a path.
It holds the directory, validates at most six known inert leaves, unlinks only those leaves,
rechecks the current name, and removes only an empty directory. Inode is a replacement warning;
semantic contents supply deletion authority. Partial cleanup is retryable; foreign contents stay
intact. Walks treat any probe-named folder as ordinary project content. A launch from an older
binary that is still running (an editor's agent server keeps its binary until restart) can still
make one, and a new walk that loses it mid-read fails that launch once; a retry succeeds.

## Verification

The opt-in `filesystemruntimee2e` box suite exercises actual Docker admission, normal commands,
Compose/healthchecks, service stop/restart/reuse, mixed replacement and failed startup, host
SIGKILL, owner death, partial attachment, unexpected-pin refusal, stale VM boot guards, Unicode,
metadata/mutation, COW mappings and public hardlinks. Enable `COOP_FILESYSTEM_RUNTIME_TEST=1`
and name a local `COOP_FILESYSTEM_CLIENT_IMAGE`; these are test inputs, not product settings.
A test-only compiled loader pause reproduces partial attachment without a production fault knob.
The normalizer fixture records exact program, map and link IDs, verifies map reuse and independent
multi-root admissions, detaches one replacement, fails partial setup and kills C while Go remains
alive. Kernel ID lookups prove retirement after cleanup; each observation FD closes immediately.
The ACP live conformance test accepts the same qualification switch to inspect the actual guard
and prove .env/.ENV denial before prompts and after replay. Live provider calls remain opt-in.

`coop doctor` is the user-run proof on a real machine. When its sandbox run reports kernel
protection, it starts a second box on its own throwaway project (a public name hardlinked to
`.env`, and `clé.pem` made with a precomposed é) and requires that `notes-hardlink`, `.ENV` and
`cle\u0301.pem` cannot be read. A spelling the disk does not resolve finds nothing, which also
passes, so the box must first read the project's public file and list `notes-hardlink`: an empty
or wrong mount fails the probe instead of passing it. The separate project keeps those files away
from the ordinary checks on mount hosts, where an unhidden hardlink would reveal the test secret.

An open or offline run on this backend leaves `box.Run` through `finish` and `retireFilesystem`, not
the plain-runner branch, and narrates its own end there: `Stopping X` when the main process
returns, the stop line once `close` confirms the box gone. A test with a shim runtime never reaches
that branch (`rt.Name != "docker"` skips admission), so `TestFilesystemBoxRunNamesItsStopRuntime`
is its proof; a missing stop line on an offline or open run points here first.

## Changelog

- 2026-11-07 — native Linux: ext4/xfs candidates, casefold probe, BPF LSM check, systemd-slice cgroups through D-Bus, kernel 7.0 verifier fixes, slice-aware descendant checks; live suite adapted to non-folding volumes and owner-death whole-disk denial; all 20 live tests and the late-secret acceptance pass on Ubuntu 24.04 (kernel 7.0, Docker 29, systemd driver).
- 2026-11-07 — every `coop acp` close warned "could not remove this editor session's services ...
  filesystem controller custody retained at ...: Docker daemon observation failed". The cause was a
  controller record bound to a QA proxy socket that no longer existed. `RecoverFilesystemAdmissions`
  with a workspace now reports only controllers an admission bound when it began (others are still
  tried; only the host-wide pass returns them). The same error also failed service starts. There is
  no endpoint fallback and custody is kept. Pinned by `TestWorkspaceRecoveryReportsOnlyControllersInUse`
  and `TestReferencedControllersNamesWhatAdmissionsBind`, whose controller-bound admission fixture copies
  fsfilter's hook list. A helper exported from internal/fsfilter would change `fsimage.Fingerprint()` and
  replace live controllers on upgrade (task 2026-11-07-keep-another-controller-s-stale-custody-record-o).
- 2026-11-06 — an offline or open run on this backend printed no stop line, because its branch of
  `box.Run` never narrated one (`coop run --egress offline -- true` ended at `Starting true` on the
  owner's Mac); `retireFilesystem` now says it once `close` confirms the box gone. Checked with the
  real binary before and after, and with the new live test (task
  2026-11-06-hide-claude-s-own-resume-hint-in-coop-and-say-th).
- 2026-11-06 — a teardown inventory that ran out of time on a host with about 58 containers turned a
  successful run into exit 1, with the deadline printed twice (task
  2026-11-06-keep-busy-host-docker-inventories-from-failing-r). Verified with
  `TestATeardownInventoryThatRunsOutOfTimeKeepsTheRunsStatus` and the churn case of
  `TestDockerCgroupInventoryReconcilesOnlyProvenDisappearance`.
- 2026-11-06 — the live name check (reason 6) exempts no folder name any more: `walk_step` cleared
  `named` under any folder called `.git`, and a box can rename any folder into one (`mv sub
  .git/sub`), so only root-level late secrets were safe; Git writes no name on the list, so nothing
  needed the exemption. `inode_symlink` passes BLIND like the other create hooks, so `ln -s x .env`
  works while follow_link/readlink still deny. `TestGuardsPassBlindOnlyWhereNothingIsRevealed` and
  `TestGuardWalkExemptsNoFolderName` pin both at the source; proved on a real box on the owner's Mac.
- 2026-11-06 — a refused create no longer stays `Pending`: QA's refused agent creates left
  `coop-fs-…-owner` helpers running until a later launch reaped them.
- 2026-11-04 — a custody record with a helper image and no controller (written 2026-11-01 and
  2026-11-02, before the shared controller) is refused by name (`errFilesystemRecordBeforeController`,
  from `record.validate`) and left as it is; its protection keeps holding, and until a v12.1.0 build
  retires it, controller recovery fails closed on it too. `matchesController` matches nothing
  without a controller. The `Probes` field and its cleanup stay: four live records on the owner's Mac
  still carry it (task 2026-11-04-drop-the-old-host-state-readers).
- 2026-11-04 — retired tools/fsprobe and its card; its one lasting trap, the stale link count, is above.
- 2026-11-04 — xattr/ACL reads on a hidden non-directory answer ENODATA/EOPNOTSUPP so `ls -l` exits 0; live suite asserts it.
- 2026-11-04 — ran `ls -l`, `readlink`, `stat`, `find` and `cat` beside a hidden file and symlink in a real box
  on virtiofs and corrected `docs/box.md`: `ls -l` exits 1 there.
- 2026-11-04 — stat of a hidden file succeeds under eBPF (blitz-infra Terraform refused module folders
  holding `terraform.tfvars`); reverified the hook list and `access()` against `filter.c`.
- 2026-11-03 — a failed helper build shows the failed step's output instead of the BuildKit footer
  (fixture: the real November 2 log). Checked the card's build paragraph against `filesystem.go`
  and `fsimage`; other sections were not reverified.
- 2026-11-03 — the daily sweep retires custody whose workspace is gone (a timed-out setup check left
  coop-fs-65dafdb1…-owner on the owner's Mac), and an unfinished create settles after ten minutes.
- 2026-11-03 — review: the doctor probe got a positive control, retiring an old probe whose
  project folder is gone no longer retains custody, and folding is measured on ASCII letters too.
- 2026-11-03 — removed the per-launch share probe (folder, stopped probe container, `coop-fs
  probe`, walk exclusion and other-admission probe handling) on the owner's decision; the volume
  profile is now measured privately; `coop doctor` gained the hardlink, letter-case and Unicode
  checks under kernel protection. Swept code, tests, docs and both filesystem cards for the probe.
- 2026-11-03 — checked approval dispatch and agent/service mount fallback. Made exposure review
  explicit; existing saved permissions and runtime backend selection remain unchanged.
- 2026-11-03 — walks leave out other live admissions' recorded probes and defer read errors under
  probe-named root entries; reproduced the `lstat` failure with two recorded probes and pinned it
  in `TestFilesystemSnapshotLeavesOutAnotherAdmissionsProbe` and three mutations.
- 2026-11-02 — recorded the failed FSEvents freshness experiment and Watchman limitation;
  rechecked inventory and final-validation ownership in the snapshot/walk sources. No scan
  was replaced with persistent observer state. Other enforcement sections were not reverified.
- 2026-11-02 — split reusable controller infrastructure from fresh per-run policies; documented
  exact generation custody, nested mount depth, recycled-kptr publication and explicit revocation.
- 2026-11-02 — removed CLI processes from exact start/removal and overlapped filtered retirement
  after consumer absence, preserving credential/artifact ordering and joining before receipt seal.
- 2026-11-02 — shared fresh normalization across all guards through exclusively C-owned unpinned
  replacements; documented the scalar ABI, reused maps and lifecycle tests against packaged sources.
- 2026-11-02 — synchronized denied probe opens with bounded host-rename visibility, requiring
  actual permission denial and retaining missing-observation counts. Verified the probe and
  warmed-alias test against plain-Docker controls; other enforcement sections were not reverified.
- 2026-11-02 — retained the bounded selection reason when mount fallback also refuses, without
  hiding joined cleanup failures or adding warnings to a successful fallback.
- 2026-11-02 — overlapped owner/probe creation with capture, including joined panic cleanup;
  image observations now use the frozen socket with fresh identity fences and separate bounds.
- 2026-11-02 — fresh-helper program loading overlaps the host census; complete policy and EOF
  still precede LSM attachment, with one C descriptor owner supervised by Go.
- 2026-11-01 — bounded parallel inventory, fresh selective APFS final validation, warmed alias
  qualification, inventory-aware policy lookup and overlapped trusted startup checks. Removed
  repeated pre-workload inventories while preserving the final stopped-container barrier.
- 2026-11-01 — consolidated snapshot traversal, build-time Unicode data, unique ancestor pins
  and policy-sized maps. Selection transfers its owned Docker binding; service bindings retain
  independent lifetimes. Readiness shares a fresh ownership observation with logs, and retirement
  batches complete exact-ID cgroup inventories while retaining final daemon verification.
- 2026-11-01 — documented integrated APFS Docker backend, its explicit snapshot contract,
  identity persistence, service reuse and reviewed crash/partial-attachment cleanup. Earlier
  cached-link-count diagnostic results are in Git history, at tools/fsprobe/RESULTS.md.
