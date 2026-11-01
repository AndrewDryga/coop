# Filesystem qualification

This developer probe measures the actual Docker share selected by `--share`. It
does not change Coop's launch policy, approve exposure, or remove spelling masks.
Run it explicitly on each runtime and share that needs qualification:

```sh
go run ./tools/fsprobe --share "$PWD" --image <cached-coop-clients-image> --out result.json
go run ./tools/fsprobe --share "$PWD" --image <cached-coop-clients-image> --bpf --out result.json
```

Use a quiescent share while the diagnostic owns its temporary fixture. Identity
checks and preserved unexpected entries reduce accidental interference; they are
not an admission protocol against a concurrent malicious checkout writer.

Use an already-built, trusted Coop clients image. The probe installs nothing and
uses the image's existing Python and C compiler for development instrumentation.
The BPF loader uses Linux system calls directly: no libbpf, bpftool, clang BPF
target, FUSE, or VM configuration changes. This is qualification tooling, not a
new runtime dependency or a production filesystem backend.

Only a new directory of inert fixtures is shared. The tool binds the Docker
endpoint, daemon identity and immutable image ID, records the inspected mounts
and guest mount information, and removes its own containers and fixture. Ordinary
checks use no network or capabilities. `--bpf` additionally runs a separate,
trusted loader with `SYS_ADMIN` and an unrestricted seccomp profile; workload
containers retain no capabilities, a read-only cgroup filesystem, and
`no-new-privileges`. No real project files or credentials are mounted.

The BPF candidate retains original inode rules and adds basename `.env`, suffix
`*.pem`, and read-only `.coopignore` rules at any depth on the fixture device.
It also experimentally rejects multiply-linked regular files. That restriction
blocks legitimate public hardlinks, and **stale kernel link counts have bypassed
it on this share**. It is not an approved production compatibility change.

`bpf_d_path` records the resolved access path and kernel inode/link metadata inside
`file_open`; the host records its own metadata after the guest open. Warming a
guest path, adding/removing a link on the host, then opening without a guest stat
tests whether the enforcement decision sees fresh attributes. First and last
kernel observations remain visible even when a later run behaves differently.
Host and guest inode numbers belong to different namespaces and are not compared.
The ancestor-replacement improvement comes from basename/suffix rules; the path
helper is instrumentation, not a complete full-path policy compiler.

The candidate attaches to two separate workload cgroups, including a service-style
alternate bind target, before testing access. The loader exits before probes run.
Tests cover host saves, fresh and warmed hardlinks, Unicode custom rules, ancestor
replacement, guest secret-source link/rename, replacement over a secret path,
policy mutations, public edits, and an existing descriptor/mapping while the host
adds a protected alias and edits the inode. A separate observer dumps the bounded
telemetry map; final cleanup verifies six programs and the map have disappeared.

This is a small diagnostic, not Coop's full glob policy. It retains stale inode
identities, does not compile arbitrary directory rules or host Unicode collation,
and does not enforce every mutation destination. Basenames of 63 or more bytes,
paths that cannot fit a 256-byte telemetry buffer, or failed telemetry insertion
deny access in the diagnostic. Such a denial is not evidence of a matching secret
rule. Telemetry counters assume these sequential fixtures, not concurrent agents.
Missing hooks, incomplete observations, leaks, or cleanup failures cannot produce
a qualified result. Partial-attachment failure cleanup is source-reviewed; it has
not received the same live qualification as successful attachment and removal.

Exit 0 means the selected experiments completed and their evidence was saved;
inspect `Qualified`, `Complete` and individual observations. Exit 1 means incomplete evidence
or cleanup failure. Neither result authorizes changing production protection.
One finite fixture does not prove all Unicode equivalences, nested filesystems,
service layouts, or future share configurations.

The kernel interfaces are documented in [LSM BPF](https://docs.kernel.org/bpf/prog_lsm.html)
and exercised by the kernel's [cgroup LSM tests](https://github.com/torvalds/linux/blob/master/tools/testing/selftests/bpf/prog_tests/lsm_cgroup.c).

See [measured results](RESULTS.md) for the current qualification outcome.
