---
name: network-gateway
description: the two helper containers that enforce a filtered run — controller (nftables) and guard (SNI/DNS) — how the helper image is built, what observation actually measures, and how cleanup seals a receipt
subsystem: networking
sources: [internal/networkgateway/controller.go, internal/networkgateway/guard.go, internal/networkgateway/hello.go, internal/networkgateway/destination_linux.go, internal/networkgateway/resolver.go, internal/networkgateway/envoy.go, internal/networkgateway/proxy.go, internal/networkgateway/service.go, internal/networkgateway/collector.go, internal/networkgateway/kernel_events.go, internal/networkgateway/clock.go, internal/gatewayimage/image.go, cmd/coop-net/main.go, internal/box/filtered_launch.go, internal/box/filtered_cleanup.go, internal/box/network_setup.go, internal/box/network_recover.go, internal/cli/boxsweep.go, internal/forkctl/host.go]
updated: 2026-09-18
---

A filtered run adds two helper containers from one pinned image, both running `coop-net`
(`cmd/coop-net/main.go`), and puts the agent in the controller's network namespace.

**Controller** — UID `0:65532`, `CAP_ADD NET_ADMIN` and nothing else, on the bridge
(`box/filtered_launch.go:86`, `:128`). It owns nftables table `coop_net`
(`networkgateway/controller.go:316`): a nat/output `capture` chain redirects the agent's
(skuid 1000) TCP on every port the policy grants TLS on — `tcp dport { 443, 853, … }` — to the
guard's `:15443`, and 53 tcp+udp to its `:15353`; a filter/output chain drops by default. A policy
with no `tls` grant renders NO redirect rule at all (an empty nft set is a syntax error, and a
default one would be an invention). Its order is the contract (`controller.go:368`): the agent's OWN loopback is
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
(`filtered_launch.go:132`). It terminates nothing: it reads the connection's ORIGINAL destination
before any byte (`SO_ORIGINAL_DST` through a raw `getsockopt`, `destination_linux.go:16`), parses
the ClientHello for SNI, refuses every ECH offer including empty and GREASE ones (`hello.go:61`),
then replays the original bytes to Envoy over a private filesystem socket with a PROXY v2 header
carrying the validated destination AND port plus a `PP2_TYPE_UNIQUE_ID` correlation TLV
(`proxy.go:15`). Envoy's `original_dst` cluster dials exactly that address:port — there is no
`upstream_port_override` any more, so the port cannot come from anywhere but the kernel. Its DNS
side admits the NAME alone (`AdmitsName`, never a port: a client must resolve before the kernel can
record which granted port it dialed), then resolves upstream over DoH to a pinned peer.

Filtered project services use the same guard through a second, bounded listener on `:15444`.
Compose puts only the approved service closure on an internal network and points standard HTTPS
proxy variables at the controller's `coop-gateway` alias. The controller admits that listener only
from the closure's fixed addresses. CONNECT names outside the frozen TLS policy are refused; an
allowed CONNECT receives `200`, then the guard parses the actual ClientHello and requires its SNI
and rule identity to match before using the ordinary resolver, lease, Envoy and observation path.
Direct service traffic cannot bypass the proxy because the Docker network itself is internal.

Facts the code cannot say twice, all still true:

- The upstream PORT is the kernel's, never the client's. `policy.Domain(name, port)` runs in the
  guard (`guard.go:173`), again in the controller before any element is installed
  (`controller.go:198`), and leases are `ipv4_addr . inet_service` pairs (`ip daddr . tcp dport
  @leases4 accept`), so one address on two granted ports is two leases. A connection whose original
  destination IS the listener was dialed straight at the guard — nothing redirected it, so it
  declares no port — and is refused as `tls_direct_dial_refused` and counted (`guard.go:144`). That
  is spec §5's "a direct dial cannot select an upstream port", and it is why the guard's own egress
  rule is port-agnostic only for ESTABLISHED flows.

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
  another filtered box's start — or another project's `compose up` — adds addresses outside a
  running box's launch envelope. The watch used to stop the run on that ("host address … appeared
  after this run's protected addresses were set"), which made two filtered runs kill each other in
  a loop: each respawn created the network that tripped the other (2026-09-12, ~60 ACP respawns in
  eight minutes). `reconcileTopology` (`box/filtered_launch.go`) now re-renders `protected4` in
  place through `protectedSetUpdate` — one atomic `flush set; add element` batch, verified on the
  pinned nftables 1.0.2 (nested prefixes auto-merge; a refused element leaves the set unchanged);
  the rendering mirrors `initialRules` and stays on the host so the embedded gateway sources (and
  therefore the qualified image) are byte-identical —
  records the grown envelope on the execution, and stops the run only when the update is refused
  or the 256-range cap is passed. A gone address keeps its denial; an IPv6 address is recorded but
  never rendered (the gateway drops IPv6 wholesale). The message still names the address.

**Observation** joins three unrelated sources in the collector: Envoy's per-flow access log (the
only place bytes are metered), sampled `/proc/net/tcp` rows, and nftables counters. TLS flows are
observed; raw grants are only counted, per kernel counter; refused datagrams are a number with no
destination, and Coop reports UNKNOWN rather than zero for anything unmeasured. Envoy is drained
before the guard so a proxy event's admission registration is already eligible in the same sample
(`collector.go:191`). A flow the proxy ended keeps its upstream socket in the kernel for a few
samples; since 2026-09-10 the collector retains up to 128 such closed tuples and lets one explain
exactly one lingering inode before it would be reported as a boundary gap (`collector.go:529`).
That was worth fixing: the gap marked a fully metered smoke receipt partial and made `coop net
setup` refuse a clean run about one time in three. A retained close expires after
`ObservationStaleAfter` (3 s, three sampling intervals) and is evicted then, so a reused ephemeral
port minutes later cannot hide a real gap; an unreadable clock folds nothing and evicts nothing.
The resolver's own DoH connection leaves the same remnant, and is explained the same way — but only
by exact identity. `owned` carries a tuple and no inode, so while a connection is owned the first
inventory row at that tuple, claimed by nothing else, BINDS its inode; releasing the connection
retains that tuple+inode pair under the same bound and the same expiry (`collector.go:462`,
`:503`), and its bytes stay where they were metered, in the maintenance counters. A connection no
sample ever bound explains nothing — the dial socket the collector can catch in SYN_SENT before
`track()` registers it (`sockets.go:278`) is still `unattributed_socket`, because the only thing
left to match it on is the peer address, and 1.1.1.1:443 is not an identity.

**Cleanup** is exact-owned and ordered (`box/filtered_cleanup.go:30`): remove the agent, prove the
guard still answers AFTER the agent is gone, stop it, copy the single `final.json` out of the
helper-only observations volume, then the controller, then the volumes and the artifact directory,
then seal the receipt. Every exit runs the deferred containment, which removes the containers AND
attempts both named volumes once nothing can still mount them (`filtered_cleanup.go:160`) — an
early return on lost host storage used to leak a volume pair per interrupted run. Missing terminal
evidence makes the receipt partial; it never blocks containment. When the supervising PROCESS dies
instead, nothing local can finish it: `box.RecoverNetworkRuns` (`box/network_recover.go:64`) is the
only path that settles such a run. It runs from `coop net recover`, from the orphan sweep at a loop,
fork or build start, and before every other filtered host launch — `runBox` in `cli/boxsweep.go`,
and a fork or session review gate through `forkctl.Host.SettleFilteredRuns` — never before an open
or offline one, because finding the pending runs reads every retained record.
The guard and controller carry only `coop.network.*` labels (the agent also carries `coop=box`), so
the box sweep alone never reclaimed them: a run killed mid-teardown, followed only by direct
launches, kept both containers and both volumes until 2026-09-18. `coop net setup` drives its ONE smoke through this same engine behind a host-only
`networkSmokeLaunch` permit (`box/network_setup.go:225`) — the seam exists so the preflight proves
the exact path a workload gets, and it can never appear on a `RunSpec` a caller builds.

The image is built from an embedded source tar plus a pinned Dockerfile, with no host compiler and
no repository build context (`gatewayimage/image.go:43`). After ANY change to `cmd/coop-net`,
`internal/egress`, `internal/networkgateway` or `internal/networkview`, run
`go generate ./internal/gatewayimage` — the canonical test compares every embedded byte against the
checkout, so a stale tar is a red gate, and a filtered launch only ever runs the exact image pair a
[[restricted-networking]] qualification names.

## Changelog
- 2026-09-18 — every filtered host launch settles interrupted runs before its own gateway starts
  (`runBox`; fork and session gates through `forkctl.Host.SettleFilteredRuns`), since a
  mid-teardown kill left the guard, controller and both volumes to launches that never swept. Proved live by `TestKilledFilteredRunIsSettledByTheNextLaunch`
  (`networkruntimee2e`); re-verified the cleanup paragraph against `filtered_cleanup.go` and
  `network_recover.go`.
- 2026-09-13 — filtered Compose service closures now use an internal network and the existing guard's
  bounded CONNECT path for approved TLS; startup waits for gateway readiness, and direct internet
  remains blocked independently of proxy variables.
- 2026-09-12 — topology growth is reconciled in place instead of ending the run: the host's
  `protectedSetUpdate` mirrors `initialRules`' rendering (kept out of the embedded sources so the
  qualified image is unchanged); verified the flush+add batch live on the pinned gateway image and
  against `TestFiltered*Protection*`
- 2026-09-10 — the retired-maintenance sibling is closed: an owned DoH connection binds its inode
  from the first unambiguous inventory row, the release retains that exact identity like a proxy
  close, and both folds share one bounded retain helper. A connection no sample bound (an in-flight
  dial) stays `unattributed_socket` on purpose — recorded here because the peer address is the
  tempting wrong key. Re-verified the closed-flow and cleanup facts above against their sources.
- 2026-09-10 — TLS on non-standard ports: the capture set, the lease set and the PROXY header all
  carry the port; the guard reads it with SO_ORIGINAL_DST and refuses a direct dial; Envoy's
  `upstream_port_override` is gone; `visible-sni-tls-ports-v3` is the new qualification contract, so
  every host re-runs `coop net setup`. Proved live against `dns.google:853`.
- 2026-09-10 — S7c: chain order documented (agent loopback and approved services BEFORE the
  protected drop, address grants after), protected set now includes the daemon's subnets and
  gateways, served ports accept only the bridge gateway, the closed-flow fold expires, cleanup
  contains volumes on every exit, and interrupted runs are settled by `coop net recover`.
- 2026-09-10 — created from the shipped gateway, carrying forward the still-true facts (Envoy
  16 KiB ceiling and hot restart, the boot-clock domain, PROXY v2, AAAA NODATA, the nft parser)
  from the eleven deleted WIP cards; re-verified each against the sources above.
