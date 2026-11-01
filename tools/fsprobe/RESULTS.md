# OrbStack qualification, 2026-11-01

**The strengthened eBPF candidate still does not qualify for production.**
Repeated tests caught a stale-metadata bypass: after the host added a protected
hardlink, `file_open` sometimes still saw one link and allowed the ordinary-named
alias. Earlier runs saw two links and denied it. A passing run cannot establish
that kernel attributes are authoritative for this host share.
This is a reclassification failure. A separate public-to-secret-write variant
denied fresh opens in all six observations; it did not demonstrate disclosure of
newly written bytes through a fresh open.

The probes ran on the actual project share, using Docker 29.8.2, arm64 Linux
`7.2.12-orbstack-00394-gbecdd6130068`, and cached clients image
`sha256:3331ac0b8fe9b4a1551248c4141caadb2a04e0a0b9d51d5b8f97e21a1ce2e2eb`.
No packages were installed and no filesystem or VM settings were changed.

## Strengthened candidate

The original identity checks remain, with basename `.env`, suffix `*.pem`, and
read-only `.coopignore` rules at any depth, plus an experimental blanket denial of
multiply-linked regular files. Secret rules now apply to the existing mutation
hooks as well. `bpf_d_path` successfully loads in the cgroup `file_open` program
and records access paths; basename/suffix matching fixes the replaced-parent case.

| Check | Measured result |
|---|---|
| Original identities and aliases | Denied |
| Atomic save, fresh ASCII spelling | Denied |
| Unicode prefix under `*.pem` | Denied by suffix rule |
| Exact custom `Café` rule after replacement | Fresh case and decomposed aliases exposed |
| Host replaces `nested/`, then opens `nested/.env` | Denied |
| Guest links/renames fresh `.env` before reading it | Denied |
| Guest replaces `.env` with a public file | Denied |
| Fresh host hardlink opened first | Denied in the retained strengthened runs |
| Previously opened alias gains a host `.env` link | Intermittently exposed; kernel link count stale |
| Legitimate public hardlink | Blocked: incompatible with ordinary hardlink use |
| Existing descriptor and mapping, then host adds secret alias and edits inode | New bytes readable; fresh open denied |
| Public single-link creation, rename and host edits | Preserved |
| Policy mutations through new hardlinks | Host contents preserved, but linked policy reads also blocked |

The stale-link observation occurred in both layouts across repeated runs. In one
agent run, host links were 2 while the kernel reported 1 before and after the
mutation; the open returned `INERT_REPLACEMENT`. A subsequent service run reproduced
the same transition. Other runs correctly reported `1 → 2` and `2 → 1`. These are
measurements of the same inode gaining/losing a host alias after a guest open, not
guest `inode_link` operations. The warmed-link fixture already contains inert
bytes before the alias is added; it tests reclassification. The separate held-FD
fixture starts with public bytes, then the host adds `.env` and writes new inert
secret bytes, demonstrating the existing-handle limitation directly.

A supplementary task-local variant started the warmed file with `PUBLIC`, added
the `.env` link on the host, then wrote `INERT_REPLACEMENT` through `.env` before
reopening the ordinary name. Across three runs and both layouts, the kernel saw
two links and denied all six fresh opens. Host writes may refresh the attributes
for this sequence; that is an inference, not a guarantee. This does not erase the
reclassification failures or turn existing-handle access into a fresh-open leak.
The exact variant patch and raw reports are retained with the task's evidence;
the reusable candidate and its gate evidence were not changed by this variant.

The initial retained strengthened reports recorded these reads in raw observations
but omitted them from their `Limitations` arrays. The final diagnostic also reports
warmed-alias exposure, public-hardlink denial, and host/kernel link-count mismatch
explicitly. Earlier failures are retained even when a later run passes.

All completed strengthened runs removed the loader before access tests, left the
unrelated cgroup able to read the fixtures, and confirmed release of six programs
and one map. A verifier-log overflow initially prevented the expanded rename hook
from loading; increasing the bounded log buffer resolved it. That failed report
remains incomplete evidence, not an enforcement pass.

**Integration decision:** production protection remains unchanged. This rejects
the tested cached-link-count classifier, not eBPF as an enforcement mechanism.
Replacing masks requires authoritative classification across host mutations and
all advertised rules, an explicit contract for existing handles, and runtime
capability qualification. A userspace watcher or successful rerun does not close
the measured interval. An owned VM could control kernel capabilities, but would
still need sharing semantics that supply authoritative policy inputs.

## Original candidates

| Check | Ordinary exact masks | Cgroup BPF candidate |
|---|---|---|
| Original canonical secret | Empty decoy | Denied |
| Original case/symlink/hardlink aliases | Case and hardlink exposure | Denied |
| Whole hidden directory, including case alias | Hidden | Original file denied |
| Host atomic save, fresh ASCII spelling | Exposed | Denied |
| New hardlink to replacement, opened first | Exposed | Exposed |
| Fresh Unicode case/normalization aliases after replacement | Exposed | Exposed |
| Policy write/truncate through a new hardlink | Host policy changed | Host policy changed |
| Host replaces ancestor directory | Exposed | Exposed |
| Public creation, rename and host edits | Preserved | Preserved |
| Separate service-style bind and cgroup | Same limitations | Same limitations |

The BPF test installed six hooks on each of two separate cgroups, then removed
the privileged loader container before checking access. Workloads had no
capabilities, `no-new-privileges`, no writable cgroup control, and no image
healthchecks. Renaming the probing process did not evade the original-file rule.
An unrelated container retained access to the same inert files. After workload
removal, a separate observer confirmed that all six kernel program IDs were gone.

The original BPF candidate improved on the earlier inode-only experiment: parent identity plus basename
protects ordinary atomic saves synchronously. It still cannot identify a new inode
through an unrelated hardlink name, loses parent identity when an ancestor is
replaced, and does not implement the host's Unicode equivalences. Those are
demonstrated bypasses, not reasons to suppress checks or silently weaken the
launch policy. Cached aliases sometimes retained old contents; reserved fresh
spellings and independent host policy observations prevent treating those stale
views as successful protection.

The command is reusable for subsequent candidates and other runtimes. These runs
do not qualify arbitrary nested mounts, the complete `.coopignore` glob policy,
Compose restart admission, inherited descriptors, or other Docker implementations.
Production planners and persistent exposure approvals are unchanged.
