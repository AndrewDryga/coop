---
name: network-gateway
description: the two helper containers that enforce a filtered run — controller (nftables) and guard (SNI/DNS) — how the helper image is built, what observation actually measures, and how cleanup seals a receipt
subsystem: networking
sources: [internal/networkgateway/controller.go, internal/networkgateway/guard.go, internal/networkgateway/hello.go, internal/networkgateway/resolver.go, internal/networkgateway/envoy.go, internal/networkgateway/proxy.go, internal/networkgateway/service.go, internal/networkgateway/collector.go, internal/networkgateway/kernel_events.go, internal/networkgateway/clock.go, internal/gatewayimage/image.go, cmd/coop-net/main.go, internal/box/filtered_launch.go, internal/box/filtered_cleanup.go, internal/box/network_setup.go]
updated: 2026-09-10
---

A filtered run adds two helper containers from one pinned image, both running `coop-net`
(`cmd/coop-net/main.go`), and puts the agent in the controller's network namespace.

**Controller** — UID `0:65532`, `CAP_ADD NET_ADMIN` and nothing else, on the bridge
(`box/filtered_launch.go:86`, `:128`). It owns nftables table `coop_net`
(`networkgateway/controller.go:305`): a nat/output `capture` chain redirects the agent's
(skuid 1000) TCP 443 to the guard's `:15443` and 53 tcp+udp to its `:15353`; a filter/output chain
drops by default. Its order is the contract (`controller.go:368`): the agent's OWN loopback is
accepted first (`oifname "lo"` plus `ip daddr 127.0.0.0/8`, so a test server on 127.0.0.1:3000 is
not an attempt on a protected address), then an approved `service:` grant's one container address,
THEN the `protected4` interval set, then every other grant, then the deny. `protected4` is host
addresses, loopback, link-local, metadata AND every subnet/gateway the Docker daemon reports at
launch (`box/filtered.go:273`) — without that half, a granted `cidr: 172.17.0.0/16` would reach
sibling containers and other sessions' boxes. Being the namespace owner it also publishes
`serve.ports` (`controller.go:126`), and their ingress rule matches ONLY the bridge gateway address
host-published traffic is NAT'd from, so a sibling container cannot reach a served port. Leases are relative kernel timeouts: the controller reserves
the whole 250 ms commit budget plus a 20 ms tick allowance out of every TTL and returns the
conservative lower bound to the guard (`controller.go:268`), so a slow kernel commit can never
extend DNS authority.

**Guard** — UID `65532:65532`, capless, read-only rootfs, sharing the controller's namespace
(`filtered_launch.go:132`). It terminates nothing: it parses the ClientHello for SNI, refuses every
ECH offer including empty and GREASE ones (`hello.go:57`), then replays the original bytes to Envoy
over a private filesystem socket with a PROXY v2 header carrying the validated destination and a
`PP2_TYPE_UNIQUE_ID` correlation TLV (`proxy.go:28`). Envoy's `original_dst` cluster restores that
peer and pins `upstream_port_override: 443` (`envoy.go:69`). Its DNS side admits the name first,
then resolves upstream over DoH to a pinned peer.

Facts the code cannot say twice, all still true:

- The pinned Envoy 1.39.1 `tls_inspector` caps ClientHello at 16 KiB, so both inspection layers use
  the same bound (`hello.go:18`, `envoy.go:37`).
- Envoy runs with `--disable-hot-restart` (`service.go:248`); its access log format is a YAML block
  scalar because a single-quoted `\n` is literal text and silently stops line intake
  (`envoy.go:59`).
- An admitted AAAA question gets a local NOERROR/NODATA answer, never REFUSED: refusing makes a
  dual-stack musl client (the Codex image) discard its valid A answer (`resolver.go:398`).
- Expiry authority is `BootInstant`, absolute nanoseconds on CLOCK_BOOTTIME (`clock.go:11`). Never
  convert one to a `time.Time` or a context deadline — an ordinary Go timeout is a wakeup, a
  boot-clock check independently rejects a suspended or late kernel.
- nft 1.0.2 rejects a table argument after `list counters inet`, and its JSON formatter prints
  UINT64_MAX as `-1` (`kernel_events.go:122`, `:224`); only that private parser reinterprets it.
- On OrbStack every Docker network also gets a HOST address (`192.168.<n>.0` on the Mac), so
  another project's `compose up` while a filtered box runs adds an address outside that run's
  envelope and the watch stops it: "host address … appeared after this run's protection envelope
  was installed" (`box/filtered_launch.go:319`). It is the correct fail-closed answer, and the
  message names the address so an operator can see which one.

**Observation** joins three unrelated sources in the collector: Envoy's per-flow access log (the
only place bytes are metered), sampled `/proc/net/tcp` rows, and nftables counters. TLS flows are
observed; raw grants are only counted, per kernel counter; refused datagrams are a number with no
destination, and Coop reports UNKNOWN rather than zero for anything unmeasured. Envoy is drained
before the guard so a proxy event's admission registration is already eligible in the same sample
(`collector.go:182`). A flow the proxy ended keeps its upstream socket in the kernel for a few
samples; since 2026-09-10 the collector retains up to 128 such closed tuples and lets one explain
exactly one lingering inode before it would be reported as a boundary gap (`collector.go:448`).
That was worth fixing: the gap marked a fully metered smoke receipt partial and made `coop net
setup` refuse a clean run about one time in three. A retained close expires after
`ObservationStaleAfter` (3 s, three sampling intervals) and is evicted then, so a reused ephemeral
port minutes later cannot hide a real gap; an unreadable clock folds nothing and evicts nothing.
The sibling case is still open — a DoH maintenance socket the resolver already released is neither
in `owned` nor retained as a close, so its remnant can still expire as `socket_join_terminal`.

**Cleanup** is exact-owned and ordered (`box/filtered_cleanup.go:30`): remove the agent, prove the
guard still answers AFTER the agent is gone, stop it, copy the single `final.json` out of the
helper-only observations volume, then the controller, then the volumes and the artifact directory,
then seal the receipt. Every exit runs the deferred containment, which removes the containers AND
attempts both named volumes once nothing can still mount them (`filtered_cleanup.go:160`) — an
early return on lost host storage used to leak a volume pair per interrupted run. Missing terminal
evidence makes the receipt partial; it never blocks containment. When the supervising PROCESS dies
instead, nothing local can finish it: `box.RecoverNetworkRuns` (`box/network_recover.go:50`) is the
only path that settles such a run, and it runs from `coop net recover` and from the ordinary orphan
sweep at the next start. `coop net setup` drives its ONE smoke through this same engine behind a host-only
`networkSmokeLaunch` permit (`box/network_setup.go:225`) — the seam exists so the preflight proves
the exact path a workload gets, and it can never appear on a `RunSpec` a caller builds.

The image is built from an embedded source tar plus a pinned Dockerfile, with no host compiler and
no repository build context (`gatewayimage/image.go:43`). After ANY change to `cmd/coop-net`,
`internal/egress`, `internal/networkgateway` or `internal/networkview`, run
`go generate ./internal/gatewayimage` — the canonical test compares every embedded byte against the
checkout, so a stale tar is a red gate, and a filtered launch only ever runs the exact image pair a
[[restricted-networking]] qualification names.

## Changelog
- 2026-09-10 — S7c: chain order documented (agent loopback and approved services BEFORE the
  protected drop, address grants after), protected set now includes the daemon's subnets and
  gateways, served ports accept only the bridge gateway, the closed-flow fold expires, cleanup
  contains volumes on every exit, and interrupted runs are settled by `coop net recover`.
- 2026-09-10 — created from the shipped gateway, carrying forward the still-true facts (Envoy
  16 KiB ceiling and hot restart, the boot-clock domain, PROXY v2, AAAA NODATA, the nft parser)
  from the eleven deleted WIP cards; re-verified each against the sources above.
