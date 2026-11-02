# Shared-hook and bulk-scan experiments

Measured on 2026-11-02 in the Coop checkout on macOS/APFS and OrbStack's Linux
arm64 Docker VM, starting from `42c8306119451e01814582d3b10fcc7acb99b3d6`.
These are feasibility results. Production still uses independent filesystem
helpers and the existing directory reader. No new host dependency was installed.

## Shared BPF hooks

An isolated fixture loaded the existing 27 guard hooks and normalization program
once. Two sibling cgroup slots selected fresh object/path/device/config inner maps.
Each admission froze those maps and retained its own exact owner-task reference
and inode pins. The shared controller had a separate task reference. Consumer
children closed inherited controller descriptors before entering their cgroup.

The final normal and abrupt-kill tests used the same immutable image:
`sha256:6f7e702229f8290faf11fa6a35fe46547a1d679967c7d2bfe846a665a29418e3`.
They passed:

- Four sequential admissions alternating opposite policies in the same slot.
- Two overlapping admissions with opposite permissions on the same underlying files.
- Actual content reads, uppercase paths and hardlink aliases; read-only grant
  write denial on a writable Docker bind, requiring EPERM/EACCES.
- Missing inner-map denial without disrupting the other admission.
- One owner dying while its sibling remained usable.
- Controller-task death denying both admissions while both inode owners remained alive.
- Empty, childless cgroup checks before withdrawing scope and policy.
- Per-run map disappearance and unchanged shared link/program identities across reuse.
- External Docker SIGKILL after both admissions were ready, followed by cleanup.
- Recorded guard, normalization and owner/controller program IDs, link IDs and map
  IDs absent after normal retirement or recovery. All observation FDs were closed.

Final normal fixture timings:

| Phase | Time |
| --- | ---: |
| Install shared programs/hooks once | 706 ms |
| Admit one tiny policy, six observations | 1.5–4.6 ms |
| Retire one tiny policy, six observations | 0.49–0.85 ms |
| Remove shared infrastructure once | 353 ms |

These exclude Coop's Docker orchestration, real inventory, policy compilation,
qualification and consumer cleanup. They do not establish a 50% CLI improvement.
Older full-run measurements included multi-second hook retirement, whereas this
fixture's final shared removal was much faster. That historical cost must be
remeasured on a matched production workload before estimating savings.

A failed early test exposed an important lifetime detail: a userspace zero update
of an array value did not clear its referenced task. Explicit BPF `kptr_xchg` plus
`bpf_task_release` fixed slot reuse. Failed verifier and lifecycle runs were kept
as evidence, and their test resources were retired.

### Production requirements still open

The fixed two-slot dispatcher proves the map and owner-lifetime shape only. It
publishes scope last with no consumers running, and updates outer references
sequentially. Production needs:

1. A bounded dispatcher and admission protocol that cannot mix policy generations,
   including failure during publication and concurrent launch/recovery.
2. Durable controller ownership bound to VM boot, Docker daemon, cgroup identity,
   image/version and exact kernel objects; supported coexistence during upgrades.
3. Separate per-run owners, frozen fresh policy and inode custody; controller loss
   must leave live consumers denied until safely retired or freshly admitted.
4. Retirement barriers covering every restartable Docker consumer and descendants,
   with exact recorded identity validation **before** recovery removes anything.
5. The existing filesystem/service/crash qualification matrix, operation-overhead
   benchmarks, and paired whole-CLI measurements on the integrated candidate.

The fixture recovery checks its fresh private nonce directory, expected names and
link types, then proves recorded IDs absent. It does not establish production
recovery ownership before mutation. The controller-task liveness test and whole
container crash test establish different properties; neither substitutes for the
other. No persistent shared daemon was added by this experiment.

## Darwin bulk metadata reader

A no-cgo prototype used bounded `getattrlistbulk` records, validated attribute/name
bounds and mount status, and preserved opened-directory identity checks. Twelve
paired scans of approximately 217,000 entries returned identical names, types and
required identities. Tests covered wide directories, Unicode, hardlinks, symlinks,
FIFO/socket entries and malformed records. Focused race tests and no-cgo Darwin
amd64/Linux arm64 builds passed.

| Inventory | Existing median | Bulk median |
| --- | ---: | ---: |
| Full metadata | 459 ms | 384 ms |
| Final selective metadata | 300 ms | 285 ms |

Eight alternating real Claude launches all reached the prompt and exited normally.
Startup medians were 4.00 s existing / 3.57 s bulk, and teardown medians 1.59 s /
1.26 s. Concurrent builds and variable host load confound these observations;
teardown is unaffected by the reader, and no whole-launch causal speedup is claimed.

The bulk implementation was **rejected for production**. The broad gate first
caught formatting in experiment files, then SA1019: the pinned `x/sys` deprecates
Darwin raw syscalls in favor of libSystem, without supplying this wrapper.
[Go documents the migration to libSystem](https://github.com/golang/go/issues/17490).
A warning suppression or numeric trap would conceal the support issue. A private
runtime/assembly bridge or cgo build migration is disproportionate to about 90 ms
of combined scan savings. Keep the existing reader until a supported binding is
available or stronger whole-launch evidence justifies that maintenance cost.

## Evidence and disposition

The local archived task
`2026-11-02-prototype-shared-bpf-hooks-and-bulk-apfs-metadat` retains the generated
C sources, build-context generator, Docker runner with finally cleanup, immutable
image IDs, logs, paired scanner/CLI samples, rejected scanner patch and reviews.
It is research evidence, not part of the production executable.

All eight test image IDs were removed after checking their experiment ownership
label. Test containers, cgroups and pins were cleaned; shared and per-run kernel
ID absence was verified by the harness. Docker build cache was not globally pruned.
Two independent reviewers supported rejecting the raw syscall path; the shared
fixture was reviewed as bounded feasibility evidence. The rejected executable
candidate did not pass the broad gate; the retained change is this report only.
