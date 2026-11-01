# OrbStack qualification, 2026-11-01

**Neither candidate qualifies as a replacement for Coop's current protection.**
The probes ran on the actual project share, using Docker 29.8.2, arm64 Linux
`7.2.12-orbstack-00394-gbecdd6130068`, and cached clients image
`sha256:3331ac0b8fe9b4a1551248c4141caadb2a04e0a0b9d51d5b8f97e21a1ce2e2eb`.
No packages were installed and no filesystem or VM settings were changed.

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

This improves on the earlier inode-only experiment: parent identity plus basename
protects ordinary atomic saves synchronously. It still cannot identify a new inode
through an unrelated hardlink name, loses parent identity when an ancestor is
replaced, and does not implement the host's Unicode equivalences. Those are
demonstrated bypasses, not reasons to suppress checks or silently weaken the
launch policy. Cached aliases sometimes retained old contents; reserved fresh
spellings and independent host policy observations prevent treating those stale
views as successful protection.

The command is reusable for subsequent candidates and other runtimes. This run
does not qualify arbitrary nested mounts, the complete `.coopignore` glob policy,
Compose restart admission, inherited descriptors, or other Docker implementations.
Production planners and persistent exposure approvals are unchanged.
