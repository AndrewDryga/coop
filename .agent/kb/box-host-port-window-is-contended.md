---
name: box-host-port-window-is-contended
description: coop's deterministic serve host ports sit inside the OS ephemeral range, so any process can be holding one — publishing is best-effort, and a test must never claim one for real
subsystem: box
sources: [internal/project/project.go, internal/box/run.go, internal/box/publish_test.go]
updated: 2026-08-10
---

`project.HostPortFor` hashes repo+key into `[hostPortBase, hostPortBase+hostPortSpan)` = **[20000,
60000)** (internal/project/project.go:47-48). The window dodges the common dev ports (3000/5173/8080)
but OVERLAPS the OS ephemeral range — macOS 49152-65535 (`sysctl net.inet.ip.portrange`), Linux
32768-60999 — so roughly a quarter of it is also what the kernel hands out to any process asking for
an ephemeral port. A coop-assigned host port can therefore be occupied at any moment by something
unrelated: an `httptest` server in another test binary, a browser socket, anything.

That is why publishing is best-effort. `appendPublish` (internal/box/run.go:1392) probes the port and,
when it is taken, still exports `COOP_SERVE_URL_<port>` — the assigned URL is stable workspace
discovery — but drops the `-p` mapping for that box. The stderr line `coop: host port N (for :P) is
in use` is NORMAL output of that branch, not a failure. Under `go test ./...` (packages run in
parallel, one process each, stderr interleaved) it can appear next to an unrelated test's `--- FAIL`
and read like its cause.

**The trap:** never claim an assigned host port for real in a test. `appendPublish` takes its probe
as a parameter (`free func(int) bool`, `hostPortFree` in production) exactly so the publish decision
is testable without binding anything; `hostPortFree` itself is covered on an OS-assigned `:0` port.
A test that binds `project.HostPort(repo, …)` is a cross-package flake waiting to happen — and its
"port busy → skip" guard is worse than the flake, because then the check quietly stops running.

## Changelog
- 2026-08-10 — created while removing the real bind from `TestAppendPublish`, traced from a
  `host port 53231 (for :5173) is in use` line seen during a parallel `-race ./...` run. Verified the
  window against internal/project/project.go:47-48 and the macOS range via `sysctl
  net.inet.ip.portrange`.
