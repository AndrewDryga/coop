---
name: box-egress-poc
description: Transparent HTTPS experiment isolates packet policy in a shared network namespace; DNS high ports and control sockets need separate guards
subsystem: box
sources: [tools/egress_poc.py, tools/egress-poc/runtime.py, tools/egress-poc/README.md, tools/test_egress_poc.py]
updated: 2026-09-08
---

The opt-in `tools/egress_poc.py` experiment is not a production launch mode.
It shares only the gateway network namespace with a capless client and uses
namespace-local nftables to redirect HTTPS/DNS. Static visible-SNI routes select
fixed upstreams; no upstream DNS resolver is reachable. See its README for limits.

Three traps established by design review and live Docker/OrbStack probes:

- Docker's embedded DNS has discoverable real high-port sockets on 127.0.0.11.
  Redirecting port53 alone does not isolate DNS; deny access to those ports too.
- Shared network namespaces also share abstract Unix sockets. Separate mounts
  do not hide Envoy hot-restart sockets; disable hot restart and omit TCP admin.
- OUTPUT filtering can still observe the original output-interface metadata after
  destination rewriting. The recorded trace had eth0 plus127.0.0.1:15001; match
  the rewritten destination tuple instead of requiring loopback interface metadata.

The gateway UID is distinct from the client and may reach only frozen upstream
IP/port pairs. Init drops all capabilities before services start. Gateway death
does not remove the namespace rules held alive by the client; never restart it
under an already-running client or flush rules during teardown.

No verified-TLS 401/405 response proves authenticated native provider/MCP execution.
SNI routing is not encrypted Host/path authorization or ECH/fronting protection.

## Changelog

- 2026-09-08 — created after Fable5.1 consultation, native security review and
  synthetic/public TLS experiments on Docker29.4.0 Linux/arm64 under OrbStack.
  Production networking unchanged; POC source and README are the bounded contract.
