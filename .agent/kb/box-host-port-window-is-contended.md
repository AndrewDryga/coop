---
name: box-host-port-window-is-contended
description: coop's deterministic serve host ports sit inside the OS ephemeral range, so any process can be holding one — publishing is best-effort, and a test must never claim one for real
subsystem: box
sources: [internal/project/project.go, internal/box/run.go, internal/box/services_note.go, internal/box/filtered_services.go, internal/box/publish_test.go]
updated: 2026-11-03
---

`project.HostPortFor` hashes repo+key into `[hostPortBase, hostPortBase+hostPortSpan)` = **[20000,
60000)** (the `hostPortBase` and `hostPortSpan` constants in internal/project/project.go). The
window dodges the common dev ports (3000/5173/8080) but OVERLAPS the OS ephemeral range — macOS
49152-65535 (`sysctl net.inet.ip.portrange`), Linux 32768-60999 — so roughly a quarter of it is also
what the kernel hands out to any process asking for an ephemeral port. A coop-assigned host port can
therefore be occupied at any moment by something unrelated: an `httptest` server in another test
binary, a browser socket, anything.

That is why publishing is best-effort. `servePublicationPlan` (internal/box/services_note.go) probes
each port once, and `appendPublish` (internal/box/run.go) applies that plan: when a port is taken it
still exports `COOP_SERVE_URL_<port>` — the assigned URL is stable workspace discovery — but drops
the `-p` mapping for that box. The stderr warning `Could not publish box port P` with
`Host port N is already in use.` is NORMAL output of that branch, not a failure. Under
`go test ./...` (packages run in parallel, one process each, stderr interleaved) it can appear next
to an unrelated test's `--- FAIL` and read like its cause. A filtered box decides the same way in
`filteredPublish` (internal/box/filtered_services.go), for its gateway controller's `-p` options;
`launchServePlan` hands the agent's note that plan, so a filtered launch never probes twice.

**The trap:** never claim an assigned host port for real in a test. `servePublicationPlan` and
`appendPublish` take the probe as a parameter (`free func(int) bool`, `hostPortFree` in production)
exactly so the publish decision is testable without binding anything; `hostPortFree` itself is
covered on an OS-assigned `:0` port. A test that binds `project.HostPort(repo, …)` is a
cross-package flake waiting to happen — and its "port busy → skip" guard is worse than the flake,
because then the check quietly stops running.

An editor session does not race its own boxes for these ports. Its supervisor holds each stable port
and forwards it to the live box (`internal/cli/acp_serve.go`), and every box publishes on a private
port the supervisor picks (`RunSpec.ServeBinds`), so a warm box holds nothing a person could reach and
a switch keeps the same URL. Something else on the port is said in the thread with no link.
The supervisor also runs one watcher in the live box over `docker exec -i`, reading `/proc/net/tcp`
(`internal/cli/acp_serve_watch.go`), so the thread hears when a server comes up. It never probes the
port, because a dev server would log that.

## Changelog
- 2026-11-03 — an editor session now owns its stable ports and forwards them to its live box.
- 2026-11-03 — added the filtered box's own decision, now the one its agent's note reads.
- 2026-10-17 — replaced line-number citations with `hostPortBase`/`hostPortSpan`,
  `servePublicationPlan` and `appendPublish`; corrected the quoted stderr line to the current
  `ui.Warning` ("Could not publish box port P" / "Host port N is already in use."), and noted that
  the probe now runs once in `servePublicationPlan` (internal/box/services_note.go, added to
  sources) before `appendPublish` applies the plan.
- 2026-08-10 — created while removing the real bind from `TestAppendPublish`, traced from a
  `host port 53231 (for :5173) is in use` line seen during a parallel `-race ./...` run. Verified
  the window against the constants in internal/project/project.go and the macOS range via
  `sysctl net.inet.ip.portrange`.
