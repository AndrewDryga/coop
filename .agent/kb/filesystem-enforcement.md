---
name: filesystem-enforcement
description: APFS Docker admissions enforce secret objects and Unicode path rules with packaged BPF hooks, guarded execution and durable cleanup custody
subsystem: box
sources: [internal/cli/net_approve.go, internal/box/caseapproval.go, internal/box/filesystem.go, internal/box/filesystem_controller.go, internal/fsfilter/controller.go, internal/fsfilter/assets/shared.h, cmd/coop-fs/controller_linux.go, internal/box/filesystem_controller_live_test.go, internal/box/filesystem_snapshot.go, internal/box/filesystem_walk.go, internal/box/filesystem_execution.go, internal/box/filesystem_services.go, internal/box/filesystem_recover.go, internal/box/filesystem_probe.go, internal/box/hidden_files.go, internal/fsfilter/policy.go, internal/fsfilter/unicode.go, internal/fsfilter/protocol.go, internal/fsfilter/assets/filter.c, internal/fsfilter/assets/loader.c, internal/fsimage/image.go, internal/fsimage/Dockerfile, cmd/coop-fs/main_linux.go, cmd/coop-fs/admit_linux.go, cmd/coop-fs/probe_linux.go, cmd/coop-fs/probe_linux_test.go, internal/box/filesystem_live_test.go, internal/box/filesystem_partial_live_test.go, internal/box/filesystem_preload_live_test.go, internal/box/filesystem_normalizer_live_test.go, internal/box/testdata/filesystem_normalizer_fixture.c, internal/runtime/docker_image.go, internal/runtime/docker_lifecycle.go, internal/runtime/docker_execution.go]
updated: 2026-11-03
---

## Selection and policy

Agent and final service launch share the filesystem admission. Automatic selection initially
requires host APFS, Docker cgroup v2 with cgroupfs, the expected BTF hook signatures and an
actual-share probe. Unsupported runtimes keep spelling mounts and existing case-exposure
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

One host walk applies the shared shadow-policy decider and remembered identities. Policy discovery
prunes hidden descendants and .git as the mount backend does, while identity inventory visits both
to close existing hardlink aliases. It persists
semantic origins under the shared hidden-file record lock before policy publication. A rejected
admission therefore cannot forget old identities after a host atomic save. Legacy mount
persistence preserves prior omitted identities at surviving aliases instead of erasing them.
Removing a rule declassifies its original object only when no remembered alias survives.

Host device/inode values group aliases only. The loader opens the actual guest objects with
O_PATH and retains those descriptors; guest device/inode keys are never translated from host
metadata. Unicode matching uses full case folding and canonical decomposition with combining
class ordering, without inserting normalization stream-safety characters. Hashed path matches
can only add denial, never grant access. Explicit service-file grants are identity-only read
access applied after protected entries; policy files and directories cannot be granted.

The helper image generates its immutable Unicode wire table at build time. Admissions compile
unique exact path/ancestor rows (never normalized hashes as allow authority), followed by all
grants. Fresh-owner creation and controller acquisition overlap the host inventory. The trusted share
probe is created stopped in that same branch; it starts only after complete policy and READY.
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
revalidation, including Linux leaf mount device checks. Every admission proves the actual share
by replacing a warmed public pathname with a protected hardlink and checking that pathname
first, before another alias could refresh guest metadata. A completed host rename can briefly
appear missing in the guest, including without BPF. The probe retries only that same open on
ENOENT, with one two-second deadline across all denied paths. Absence never proves enforcement:
actual EPERM/EACCES is mandatory, a successful open or any other error fails immediately, and
public-content checks remain required. The probe records how many missing observations occurred;
it never refreshes another alias or restarts the qualification to make a failure disappear.

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
only to the frozen cgroup parent and its descendants. Secret reads, metadata and endpoint
mutations are denied; .coopignore is readable but cannot be changed, including through aliases.
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

Probe cleanup does not compare persisted device numbers or recursively delete a path. It holds
the directory, validates at most six known inert leaves, unlinks only those leaves, rechecks the
current name, and removes only an empty directory. Inode is a replacement warning; semantic
contents supply deletion authority. Partial cleanup is retryable; foreign contents stay intact.

Parallel launches in one project (an editor's warm boxes start together) each put a probe at the
root, so a walk can meet another admission's probe and then lose it at that admission's
qualification. Before 2026-11-03 the walk classified its `.env` and the final check failed with
`lstat <repo>/.coop-fs-probe-…: no such file or directory`. The inventory (`inventoryFilesystem`)
now defers what it cannot read under a probe-named root entry and, after the walk,
`leaveOutProbes` drops a probe that a not-provably-dead admission recorded with the same
identity, or that vanished under a recorded name. Any other probe-named entry, a dead admission's
included, stays project content and its read errors still fail the walk. Identity is reliable here
because admission runs only on APFS, which does not reuse inode numbers.

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

## Changelog
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
  cached-link-count diagnostic results remain historical evidence in tools/fsprobe/RESULTS.md.
