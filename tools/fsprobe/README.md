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

The BPF candidate combines original inode identity with parent identity and a
case-folded basename. It attaches directly to two separate workload cgroups,
including a service-style alternate bind target, before testing access. It tests
host atomic saves, newly introduced hardlinks, ancestor replacement, policy
mutations, public edits, process renaming and loader removal. It is deliberately a
small candidate whose limitations must remain visible, not an implementation of
Coop's complete glob policy. Missing hooks, incomplete observations, leaks, or
cleanup failures cannot produce a qualified result.

Exit 0 means the selected experiments completed and their evidence was saved;
inspect `Qualified`, `Complete` and individual observations. Exit 1 means incomplete evidence
or cleanup failure. Neither result authorizes changing production protection.
One finite fixture does not prove all Unicode equivalences, nested filesystems,
service layouts, or future share configurations.

The kernel interfaces are documented in [LSM BPF](https://docs.kernel.org/bpf/prog_lsm.html)
and exercised by the kernel's [cgroup LSM tests](https://github.com/torvalds/linux/blob/master/tools/testing/selftests/bpf/prog_tests/lsm_cgroup.c).

See [measured results](RESULTS.md) for the current qualification outcome.
