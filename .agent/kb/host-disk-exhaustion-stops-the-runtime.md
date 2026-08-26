---
name: host-disk-exhaustion-stops-the-runtime
description: a host out of disk OR memory kills the container runtime mid-run; both surface as unexplained "unexpected EOF" failures, never as a resource error
subsystem: loop
sources: [internal/box/run.go, internal/loop/loop.go, internal/cli/commands.go]
updated: 2026-08-24
---
A long loop can fill the host disk, and when it does the failure does NOT look like a disk problem.
It looks like the loop breaking: `error waiting for container: unexpected EOF`, then
`coop: iteration failed (n/5) — retrying in 10s` until the run gives up.

Observed 2026-08-03. The runtime's own log named the real cause:

```
I/O error, dev vdb, sector 50141824 op 0x1:(WRITE)
BTRFS error (device vdb1): error while writing out transaction: -5
BTRFS info (device vdb1 state EA): forced readonly
VM stopped
```

The chain: OrbStack (and Docker Desktop) keep the guest filesystem in a **sparse** host file that
grows on demand. When the host has nothing left to give, the guest's virtual disk starts failing
writes, its filesystem aborts the transaction and remounts read-only, and the VM stops — taking
every running box with it. Five iterations died before the socket was even checked.

**Diagnosing it.** `docker ...` failing with `no such file or directory` on the socket means the
engine is DOWN, not busy. Check the runtime's log tail (`~/.orbstack/log/vmgr*.log`) before
blaming the loop; the rotated file's mtime is the moment of death.

**Two traps when reclaiming.**
- `docker prune` frees space INSIDE the image; the sparse host file does not shrink. Only a guest
  TRIM returns it: `docker run --rm --privileged --pid=host alpine nsenter -t 1 -m -u -i -n fstrim
  /var/lib/docker`. A prune that reports gigabytes reclaimed while host free space does not move is
  this, not a no-op. Restarting the runtime also compacts.
- The conservative prune (dangling images + anonymous volumes) can reclaim **0 B** while the host is
  at 240 MiB. The space sits in unused-but-tagged images and build cache, so prune those by AGE
  (`--filter until=…`) rather than by type: an image backing a running container is protected by
  the runtime anyway, and the box image is rebuilt every run so a live one is always recent.

**Look past the runtime first.** The dominant consumer here was not the runtime at all — it was the
Go build cache at **27 GB**, grown by repeated full gate runs (one cold `make check` costs ~23 GB).
`go clean -cache` took the host from 851 MiB to 29 GiB instantly. Measure before reclaiming.

Coop's own contributions are bounded: boxes run with `--rm` (`internal/box/run.go`), and a fork's
compose stack is now torn down with the fork — see [[services-teardown-needs-the-workspace]].

## Memory exhaustion does the same thing, and looks identical

Observed 2026-08-24. Same `unexpected EOF`, same dead engine — but the disk was fine (53 GiB free).
The host ran out of memory, and macOS's jetsam killed processes wholesale; OrbStack's VM went
with them, one second before coop saw its first EOF:

```
03:05:50  [system/com.apple.rtcreportingd] exited with exit reason
          (namespace: 1 code: 0xc) - OS_REASON_JETSAM, ran for 204ms
          ... ~30 more system daemons in the same second ...
03:05:51  error waiting for container: unexpected EOF
```

`~/.orbstack/log/unified-kill.log` is the file that names it — mass `OS_REASON_JETSAM` exits at one
timestamp is memory, not disk. Check it before reclaiming disk: the two causes share every
downstream symptom, so the disk playbook above can "succeed" against a problem it never addressed.

**Interactive sessions die differently from the loop.** Where the loop retries and gives up, an ACP
session is torn down: every box respawn fails instantly, `acpproxy` hits its 5-rapid-failures cap
(`internal/acpproxy/proxy.go`), and the editor's stdio transport closes — Zed reports
`incoming_transport_closed` on whatever request was in flight. The editor holds the dead connection,
so every later session fails identically until that window is reloaded. Restarting the runtime is
not enough; the editor has to reconnect.

## Changelog
- 2026-08-03 — created after a full host disk stopped the OrbStack VM mid-run and presented as five unexplained iteration failures; records the diagnosis path and why prune alone does not return space.
- 2026-08-24 — memory exhaustion recorded as a second cause with the same signature (jetsam took the VM while the disk had 53 GiB free); adds `unified-kill.log` as the discriminator and the ACP teardown path, which fails differently from the loop's retry. Same run fixed the misleading diagnosis it caused: `image inspect` cannot tell a dead daemon from a missing image, so `resolveImage` (`internal/cli/commands.go`) and the loop reported "not built — run 'coop build'" while the image was present the whole time; all three call sites (`resolveImage`, the loop's image guard, and the fork merge gate) now probe `EnsureDaemon` on that branch first.
- 2026-08-10 — sources repointed: the loop engine moved out of `internal/cli` into `internal/loop`; the `iteration failed (n/5) — retrying`
  narration this card diagnoses from is now `internal/loop/loop.go`, byte-identical.
