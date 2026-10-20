---
name: network-gateway
description: the two helper containers that enforce a filtered run — controller (nftables) and guard (SNI/DNS) — how the helper image is built, what observation actually measures, and how cleanup seals a receipt
subsystem: networking
sources: [internal/networkgateway/native_broker.go, internal/networkgateway/open_broker.go, internal/networkgateway/controller.go, internal/networkgateway/guard.go, internal/networkgateway/hello.go, internal/networkgateway/destination_linux.go, internal/networkgateway/resolver.go, internal/networkgateway/envoy.go, internal/networkgateway/proxy.go, internal/networkgateway/service.go, internal/networkgateway/credential_broker.go, internal/networkgateway/collector.go, internal/networkgateway/kernel_events.go, internal/networkgateway/clock.go, internal/networkgateway/clock_linux.go, internal/networkgateway/control.go, internal/networkgateway/identity.go, internal/networkgateway/observe.go, internal/gatewayimage/image.go, internal/gatewayimage/image_test.go, internal/gatewayimage/Dockerfile, tools/gennetimage/main.go, cmd/coop-net/main.go, internal/box/filtered_launch.go, internal/box/filtered_cleanup.go, internal/box/network_setup.go, internal/box/network_recover.go, internal/cli/boxsweep.go, internal/cli/net_cmd.go, internal/forkctl/host.go]
updated: 2026-10-20
---

A filtered run adds two helper containers from one pinned image, both running `coop-net`
(`cmd/coop-net/main.go`), and puts the agent in the controller's network namespace.

Private helper IPC/observation uses protocol2 and kernel-qualified boot-clock coordinates.
`OpenBootClock` pins the reading thread and verifies its current/child namespaces agree with
the process leader's before and after reading bounded, canonical `timens_offsets`. Namespace
inodes stay provenance; different namespaces are peer-compatible only on the same boot with
verified zero boottime offsets. Missing proof, nonzero cross-namespace offsets and changed
run/epoch/policy remain denied. Local component domains and the host's pinned guard observations
still require exact identity. This fixes runtimes that isolate helper time namespaces without
changing their clock values; it is not a repair of the unexplained native Claude account stop.

The native broker chooses its permanent first denial while holding the snapshot comparison
mutex, clears retained access there, then tears down streams outside the mutex. Every stop
caller uses that same latch, so slow cleanup cannot admit a newer valid snapshot or replace
the initiating credential-free diagnostic with a later lifecycle reason.

**Controller** — UID `0:65532`, `CAP_ADD NET_ADMIN` and nothing else, on the bridge (`helperOptions`
in box/filtered_launch.go, re-checked by `verifyContainer`). It owns nftables table `coop_net`
(`Controller.initialRules` in networkgateway/controller.go): a nat/output `capture` chain redirects
the agent's (the host-selected non-root `skuid`: native Linux user UID, or 1000 on Docker Desktop)
TCP on every port the policy grants TLS on — `tcp dport { 443, 853, … }` — to the guard's `:15443`,
and 53 tcp+udp to its `:15353`; a filter/output chain drops by default. A policy with no `tls` grant
renders NO redirect rule at all (an empty nft set is a syntax error, and a default one would be an
invention). Its order is the contract (`initialRules` in `controller.go`): Docker's resolver address
127.0.0.11 is refused first, counted as `protected_agent` (port 53 there never reaches the filter,
because the capture rewrote it to the guard; see restricted-networking), then the agent's OWN
loopback is accepted (`oifname "lo"` plus `ip daddr 127.0.0.0/8`, so a test server on 127.0.0.1:3000
is not an attempt on a protected address), then an approved `service:` grant's one container
address, THEN the `protected4` interval set, then every other grant, then the deny. `protected4` is
host addresses, loopback, link-local, metadata AND every subnet/gateway the Docker daemon reports at
launch (`filteredProtectedAddresses` in box/filtered.go) — without that half, a granted
`cidr: 172.17.0.0/16` would reach sibling containers and other sessions' boxes. Being the namespace
owner it also publishes `serve.ports` (`validServePorts`, rendered in `initialRules`), and their
ingress rule matches ONLY the bridge gateway address host-published traffic is NAT'd from, so a
sibling container cannot reach a served port. Leases are relative kernel timeouts: the controller
reserves the whole 250 ms commit budget plus a 20 ms tick allowance out of every TTL and returns the
conservative lower bound to the guard (`leaseRules`), so a slow kernel commit can never extend DNS
authority. The controller commits ONE lease at a time and answers every concurrent request
`gateway_lease_capacity` (`control.go`) — contention, not a verdict — so the guard and every
credential broker admit through the same `admitLease` (`guard.go`): wait their turn within the
admission budget, and refresh an answer inside the commit margin once. (The broker once skipped
both, and a client opening its MCP servers beside its model call lost one of them to a spurious
`credential_broker_upstream_unavailable`.) The resolver asks once more, after a second, when an
answer arrives already expired: a caching upstream serves a record at TTL 0 at the end of its life
(Akamai's 20 s records through 1.1.1.1), which no lease can use.

**Guard** — UID `65532:65532`, capless, read-only rootfs, sharing the controller's namespace
(`helperOptions` again). It terminates nothing: it reads the connection's ORIGINAL destination
before any byte (`SO_ORIGINAL_DST` through a raw `getsockopt`, `readOriginalDestination` in
destination_linux.go), parses the ClientHello for SNI, refuses every ECH offer including empty and
GREASE ones (`Inspect` in hello.go), then replays the original bytes to Envoy over a private
filesystem socket with a PROXY v2 header carrying the validated destination AND port plus a
`PP2_TYPE_UNIQUE_ID` correlation TLV (`ProxyHeader` in proxy.go). Envoy's `original_dst` cluster
dials exactly that address:port — there is no `upstream_port_override` any more, so the port cannot
come from anywhere but the kernel. Its DNS side admits the NAME alone (`AdmitsName`, never a port: a
client must resolve before the kernel can record which granted port it dialed), then resolves
upstream over DoH to a pinned peer. A flow has two clocks that must never be crossed: the ADMISSION
budget (`GuardAdmissionTimeout`, 10 s — inspection, resolution, lease, private dial, replay) and the
flow's LIFETIME (the guard's own context). `forwardTLS(flow, admission, …)` takes both, and only
`flow` may close the private leg. Until 2026-09-18 the close was armed on the admission context, so
every guarded TLS flow died 10 s after admission — long streamed model responses were cut mid-body
and retried forever (`TestGuardFlowOutlivesTheAdmissionBudget`,
`TestServiceProxyFlowOutlivesTheAdmissionBudget`).

Filtered project services use the same guard through a second, bounded listener on `:15444`.
Compose puts only the approved service closure on an internal network and points standard HTTPS
proxy variables at the controller's `coop-gateway` alias. The controller admits that listener only
from the closure's fixed addresses. CONNECT names outside the frozen TLS policy are refused; an
allowed CONNECT receives `200`, then the guard parses the actual ClientHello and requires its SNI
and rule identity to match before using the ordinary resolver, lease, Envoy and observation path.
Direct service traffic cannot bypass the proxy because the Docker network itself is internal.

Brokered credentials use the guard too (`credential_broker.go`): one loopback listener per route of
the run's plan, `CredentialBrokerAddress(i)` = 15580+i, each holding only its own route's substitute
and credential (`CredentialBrokerSecrets` v2, bound to the run and gateway epoch and to each route
by name; substitutes must differ). A route's kind decides its shape — four of them: `provider` is one POST endpoint
with a 30 s response-header timeout; `mcp-sse` is the legacy transport (GET the stream's own path,
POST any clean path of that ONE host, since the server names its endpoint at runtime);
`download` carries public bytes with NO credential at all, on exact method+path+query lines
(`Allow`), refusing any credential the box brings; `mcp` is one streamable-HTTP endpoint — exact path,
POST/GET/DELETE, one secret header (any lower-case name HTTP, a proxy or the MCP protocol does not
own — `mcpSecretHeader`, which also refuses `x-forwarded-for`/`forwarded` because the proxy appends
to those AFTER the director sets the credential — after a literal prefix of at most 64 bytes;
`MCPSecretHeader` is the same judgment exported so the HOST refuses such a header first, by server
name), no query — with NO header
timeout, because a server answering a tool call with JSON sends headers only when the tool finishes
and a 502 makes the agent retry a mutation. A
request must fit its route's endpoint (`CredentialBrokerRoute.Admits`: method; a path already clean
— no dot segment, doubled slash or second encoding an upstream could normalize into a sibling
endpoint, and a trailing slash only as an exact route's own path (MCP endpoints like `/mcp/`) —
equal to or under the route's; a query only where the adapter declared one) and name its
own listener in `Host`, so a capability presented to another route's listener is refused before
any upstream dial; the key is injected on the route's fixed upstream through the ordinary
lease/Envoy path.

Facts the code cannot say twice, all still true:

- The upstream PORT is the kernel's, never the client's. `policy.Domain(name, port)` runs in the
  guard (`Inspect`, given the original destination's port by `Guard.forward`), again in the
  controller before any element is installed (`Controller.Admit`), and leases are
  `ipv4_addr . inet_service` pairs (`ip daddr . tcp dport @leases4 accept`), so one address on two
  granted ports is two leases. A connection whose original destination IS the listener was dialed
  straight at the guard — nothing redirected it, so it declares no port — and is refused as
  `tls_direct_dial_refused` and counted (`Guard.destination`). That is spec §5's "a direct dial
  cannot select an upstream port", and it is why the guard's own egress rule is port-agnostic only
  for ESTABLISHED flows.

- **`tls_direct_dial_refused` and `dns_query_invalid` carry no destination.** The box shares the
  gateway's namespace, so anything in it can dial `127.0.0.1:15443`/`:15353` directly; the guard
  refuses the first (no redirect ⇒ no declared port) and cannot read the second as a query. A direct
  dial never declared a destination and an unreadable query never named one — which is NOT the same
  as "nothing was trying to leave": a client whose DNS is merely malformed (a missing TCP length
  prefix, an oversized datagram, a multi-question query — `Guard.dnsTCP`, `Guard.dnsUDP`,
  `parseQuery`) was trying to resolve a real name. Investigated 2026-09-19/20 (task
  `…-find-why-a-filtered-run-s-teardown-can-record-di`): a run's 4+4 burst was read as a teardown
  artifact, but the evidence timeline put it 49 s into a 78 s run, 0.6 s before a SUCCESSFUL
  connection to the same host — and a live repro (a box dialing both listeners) produced exactly
  those two reasons mid-run. So every surface now separates them from blocked traffic
  (`networkreport.LocalRefusal`, used by `net inspect`, `net watch`, the run listing, the box
  summary and the loop's iteration block) instead of folding them into "remote addresses". Since
  2026-09-20 the event also carries the client's OWN port (`GuardEvent.SourcePort` →
  `networkview.Denial.SourcePort`, rendered as "from port N inside the box", and carried to a fleet
  reader through `workerproto.NetworkDenial.SourcePort`): it is the only handle on WHICH client,
  since there is no destination to look up, and it survives a destinations-withheld projection
  because a port inside the box names no endpoint. It is a SOURCE — never fed to `Destination()`,
  never counted as traffic.

- The pinned Envoy 1.39.1 `tls_inspector` caps ClientHello at 16 KiB, so both inspection layers use
  the same bound (`MaxHelloBytes` in hello.go, `max_client_hello_size` in envoy.go's
  `EnvoyBootstrap`).
- Envoy runs with `--disable-hot-restart` (`GuardRuntime.Run` in `service.go`): the box shares the
  guard's network namespace, and Envoy's hot-restart socket is an abstract Unix socket, which
  belongs to the namespace and ignores mounts, so the private tmpfs cannot hide it from the agent.
  Its access log format is a YAML block scalar because a single-quoted `\n` is literal text and
  silently stops line intake (the access log's `inline_string` in `EnvoyBootstrap`).
- An admitted AAAA question gets a local NOERROR/NODATA answer, never REFUSED: refusing makes a
  dual-stack musl client (the Codex image) discard its valid A answer (`Resolver.Answer`).
- Expiry authority is `BootInstant`, absolute nanoseconds on CLOCK_BOOTTIME (clock.go). Never
  convert one to a `time.Time` or a context deadline — an ordinary Go timeout is a wakeup, a
  boot-clock check independently rejects a suspended or late kernel.
- nft 1.0.2 rejects a table argument after `list counters inet`, and its JSON formatter prints
  UINT64_MAX as `-1` (`readKernelCounters` and `nftUnsigned` in kernel_events.go); only that private
  parser reinterprets it.
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
  records the grown envelope on the execution (with a context the run's end does not cancel: the
  address is already in `f.protected`, so no later tick would record it), and stops the run only
  when the update is refused
  or the 256-range cap is passed. A gone address keeps its denial; an IPv6 address is recorded but
  never rendered (the gateway drops IPv6 wholesale). The message still names the address.

**Observation** joins three unrelated sources in the collector: Envoy's per-flow access log (the
only place bytes are metered), sampled `/proc/net/tcp` rows, and nftables counters. TLS flows are
observed; raw grants are only counted, per kernel counter; refused datagrams are a number with no
destination, and Coop reports UNKNOWN rather than zero for anything unmeasured. Envoy is drained
before the guard so a proxy event's admission registration is already eligible in the same sample
(`Collector.sample`). A flow the proxy ended keeps its upstream socket in the kernel for a few
samples; since 2026-09-10 the collector retains up to 128 such closed tuples and lets one explain
exactly one lingering inode before it would be reported as a boundary gap
(`Collector.closeAccountsFor`). That was worth fixing: the gap marked a fully metered smoke receipt
partial and made `coop net setup` refuse a clean run about one time in three. A retained close
expires after `ObservationStaleAfter` (3 s, three sampling intervals) and is evicted then, so a
reused ephemeral port minutes later cannot hide a real gap; an unreadable clock folds nothing and
evicts nothing. The resolver's own DoH connection leaves the same remnant, and is explained the same
way — but only by exact identity. While a connection is owned, the first inventory row at its tuple,
claimed by nothing else, BINDS its inode. A connection dialed and released between two samples never
meets a sample, so `track()` also reads the inode from the connection's own fd (`socketInode`,
`fstat` — the number /proc/net/tcp lists) and `Close` appends (tuple, inode, boot-clock release
instant) to a bounded released list each sample drains after its inventory read (`drainReleased`).
Either way `reconcileMaintenance` retains the tuple+inode pair under the same bound and expiry as a
proxy close, `maintenanceAccountsFor` explains only that exact tuple, inode and UID 65532 within 3
s, and the bytes stay where they were metered, in the maintenance counters. What still explains
nothing: a dial the collector catches in SYN_SENT and that stays unregistered past the 3 s bound —
its only key is the peer address, and 1.1.1.1:443 is not an identity — and an inode-0 closing
remnant, which stays an ownerless `kernel_closing_remnant` (the report hides it). macOS's fstat
reports no socket inode; only the gateway's Linux container needs it.

**Cleanup** is exact-owned and ordered (`filteredExecution.cleanup` in box/filtered_cleanup.go):
remove the agent, prove the guard still answers AFTER the agent is gone, stop it, copy the single
`final.json` out of the helper-only observations volume, then the controller, then the volumes and
the artifact directory, then seal the receipt. Every exit runs the deferred containment, which
removes the containers AND attempts both named volumes once nothing can still mount them
(`containVolumes`) — an early return on lost host storage used to leak a volume pair per interrupted
run. Missing terminal evidence makes the receipt partial; it never blocks containment. When the
supervising PROCESS dies instead, nothing local can finish it: `box.RecoverNetworkRuns` is the only
path that settles such a run, and it takes every run still owed cleanup OR a receipt. A death after
the teardown removed the containers and volumes leaves at most the artifact and no receipt, and
retention keeps an unsealed run forever, so the pass finishes that one from the record alone: it
connects to the recorded daemon only for a resource still at the runtime (`recoverLeftovers`), so a
stopped or replaced daemon cannot hold the seal back, and `coop net recover` and `coop net inspect`
resolve a runtime only when cleanup is pending. There is no create/start journal (execution record
version 2): each resource has a fixed name, `coop-net-<run>-<role>`, and run labels only coop sets
(plus its supervisor's pid, start token and host, for a person reading `docker ps`). The launch asks
`AuthorizeLaunch` before each create and start, and teardown and recovery inspect each role by name
and run labels, remove what matches, and leave a holder of the name with other labels alone
(`runtime.ErrDockerOwnership`, reported as `NetworkRecovery.Unverified`). Whether a supervisor is
gone is judged from its record, never from a label. `NetworkRecovery.Sealed` means THIS pass wrote the
receipt, never an earlier one. A record names its supervisor's host (`Supervisor.Host`, the
supervisor labels' host field), and the pass judges only a supervisor recorded here
(`supervisorRecordedHere`): another host's run is left alone with `Elsewhere` set, and a record with
no host is judged only by a coop outside a container, as v1 labels are. It runs from `coop net recover`, from `coop net inspect` of a run
owed either (scoped to that run), from the orphan sweep at a loop, fork or build start, and before
every other filtered host launch — `runBox` in `cli/boxsweep.go`, and a fork or session review gate
through `forkctl.Host.SettleFilteredRuns` — never before an open or offline one, because finding the
pending runs reads every retained record. The guard and controller carry only `coop.network.*`
labels (the agent also carries `coop=box`), so the box sweep alone never reclaimed them: a run
killed mid-teardown, followed only by direct launches, kept both containers and both volumes until
2026-09-18. `coop net setup` drives its ONE smoke through this same engine behind a host-only
`networkSmokeLaunch` permit (`runSetupSmoke` hands it to `runWithNetworkSmoke`) — the seam exists so
the preflight proves the exact path a workload gets, and it can never appear on a `RunSpec` a caller
builds.

The image is built from an embedded source tar plus a pinned Dockerfile, with no host compiler and
no repository build context (`Context` in `gatewayimage/image.go`). `gatewayimage.Pack` defines the
tar and `tools/gennetimage` writes it: the non-test Go files of `sourceDirs` (`cmd/coop-net`,
`internal/egress`, `internal/networkgateway`, `internal/networkview`, `internal/randid`,
`internal/safefile`, `internal/safetext`), `LICENSE`, and the gateway's OWN go.mod and go.sum, which name only
`sourceModules` (`go.yaml.in/yaml/v3`, `golang.org/x/net`, `golang.org/x/sys`) at the root's
versions. A bump that leaves those three alone leaves the tar, `Fingerprint()` and every user's
network setup alone, but most other bumps
(x/crypto, the sqlite modules) also raise x/sys and so still move it; until 2026-10-16 the whole
root go.mod/go.sum rode along and every Dependabot PR failed the freshness test. After a change to those
packages, `LICENSE`, the root `go` line or a gateway module's version, run
`go generate ./internal/gatewayimage` — the canonical test compares the embedded tar with a fresh
`Pack` and names the files that drifted. Makefile's early `gateway-sources-check` refuses stale bytes
without regenerating during qualification, so a stale tar is a red gate, and a filtered launch only
ever runs the exact image pair a [[restricted-networking]] qualification names.

Traps: the gateway go.mod drops the root `toolchain` line on purpose (the golang image sets
`GOTOOLCHAIN=local`, so its pinned Go builds the gateway), and `Pack` refuses any other directive
(replace, exclude, godebug, tool) until someone decides whether coop-net needs it. When coop-net
starts loading a package from another module, `TestEmbeddedGatewayModuleLoadsOnItsOwn`
(`go list -deps -mod=readonly` for Linux on the extracted tar) fails until that module joins
`sourceModules`. The YAML module's own go.mod says `go 1.16`, so if it ever gains a requirement the
build needs that module's `/go.mod` sum, which `Pack` cannot carry and `sourceModules` cannot name
(the root does not require it): that failure means revisiting `Pack`, not the list. The pinned
golang image must be at least the packed `go` line (`TestGatewayRecipeBuildsThePackedGoVersion`).
The generator compiles `gatewayimage`, so `source.tar` must exist: restore a deleted one from git
before regenerating.

The Dockerfile is embedded on its own, not in the tar: editing it leaves `source.tar` byte-identical
but still moves `Fingerprint()` (recipe plus tar), so every host sets up networking again. Its first
`FROM` is the one Go pin a network setup records: `GoBase()` reads that digest-pinned `golang` image
from the recipe for the setup's `go_base`, since Coop's box images no longer build with Go. Its nine
Ubuntu packages are pinned to exact versions and install from a dated
`snapshot.ubuntu.com/ubuntu/<YYYYMMDDTHHMMSSZ>` archive written into `sources.list`. Six of the nine
versions are jammy's release versions, which never leave the archive; nftables, libnftables1 and
libxtables12 come from jammy-updates, which keeps only each package's newest version, so a pin
against the live archive breaks at the next nftables or iptables update. Move the date with the
versions. Canonical says it intends to keep snapshots for at least two years, so the date set on
2026-10-15 must move before 2028-10. The `ubuntu` path serves every architecture (`ubuntu-ports`
answers 401); the Release files carry no `Valid-Until` and are signed with the 2018 archive key the
jammy keyring trusts, so apt's signature check stays on with no override.

**Stop latency.** A run's teardown ends with the guard's terminal counters barrier
(`kernelEvents.after`): it needs a kernel sample STARTED after its cutoff, and the controller samples
nft once a second, so the barrier used to wait out up to that whole tick — the guard took ~0.5 s to exit
after SIGTERM on every filtered stop. The barrier now wakes the sampler (a sample starts at once, still
after the cutoff). On the host side (`box/filtered_cleanup.go`) the causal order is fixed — agent gone,
guard probed and stopped, final observation taken — and only independent steps overlap: the guard's
removal beside the controller's stop, and the two volumes; store writes stay serialized under `f.mu`.
Measured 2026-09-19: SIGINT→exit 1.87 s → ~1.03 s.

**Start latency.** A filtered launch (`box/filtered_launch.go`, `launch`) is ~110 Docker CLI calls of
~20–40 ms, with ~74 daemon-identity checks around them that are custody, not overhead: none may be
skipped (a review rejected skipping a read's check when nothing ran since the last one, because it
moves the check away from the start it guards). Each check is GET /info over the bound socket
(`runtime/docker.go` `readInfo`, ~4 ms) instead of a `docker info` process (~40 ms); the socket path
is the endpoint's literal bytes, and `validDockerEndpoint` refuses `%` and surrounding whitespace so
the CLI and the request cannot reach different sockets. Only
independent steps overlap, through `together`, which waits for EVERY step so a failing one never
strands its sibling's request mid-flight, and raises a step's panic again on the launch goroutine so
the run's deferred teardown still runs: the two volumes, and the guard's creation beside the
controller's start. Docker creates a container on `--network container:<id>` while the target merely
exists but refuses to start it ("cannot join network namespace of a non running container") until the
target runs, so the guard's start stays after the controller's. A guard still in registry state
`created` never ran, and cleanup skips its observation block: `docker cp` from a never-started
container reports the final file missing, a failure that never happened. Measured 2026-09-19 in this
repo: start p50 4.33 s → 3.90 s. The guard answers `probe` ~0.1 s after its start, but the launch
also waits for a READY snapshot, and the snapshot is the collector's last published sample: its first
comes before `markReady`, and on the 1 s tick the next came a whole `ObservationInterval` later — the
largest block of a start. `markReady` now wakes the collector (`Collector.Wake`, the kernel sampler's
pattern), so readiness is published when it happens: start p50 3.97 s → 3.00 s.

## Changelog
- 2026-10-20 — the watch's record of a grown envelope no longer shares the run's cancellation: a run
  ended between the kernel update and the record lost the address for good (CI, 2026-10-19;
  `TestFilteredWatchRecordsGrowthWhenTheRunEndsAtThatMoment`).
- 2026-10-18 — cleanup and recovery find each resource by its fixed name and run labels; the
  create/start journal and its ambiguous-create reconciliation are gone (task
  2026-10-15-replace-the-network-cleanup-journal-with-a-label).
- 2026-10-18 — coop-net now imports `internal/randid` (the shared random-ID helper), so it joined
  `sourceDirs`; four refactors had left `source.tar` stale until the batch gate caught it.
- 2026-10-17 — run records name the supervisor's host and recovery skips other hosts' runs (task
  2026-10-17-record-the-host-in-network-run-supervisor-record).
- 2026-10-17 — replaced every line-number citation with the function, constant or template it meant
  (`helperOptions`, `verifyContainer`, `initialRules`, `filteredProtectedAddresses`,
  `validServePorts`, `leaseRules`, `readOriginalDestination`, `Inspect`, `ProxyHeader`,
  `Controller.Admit`, `Guard.destination`, `Guard.dnsTCP`/`dnsUDP`, `parseQuery`, `Resolver.Answer`,
  `MaxHelloBytes`, `EnvoyBootstrap`, `readKernelCounters`, `nftUnsigned`, `Collector.sample`,
  `closeAccountsFor`, `filteredExecution.cleanup`, `containVolumes`, `runWithNetworkSmoke`), each
  re-verified at HEAD. The guard's domain check is now cited at `Inspect` (the TLS path that gets
  the kernel's port), not the service proxy's copy the old number had drifted onto.
- 2026-10-17 — `internal/safetext` joined `sourceDirs`: egress imports it for the characters
  provenance text may not hold (task 2026-10-17-escape-c1-bidi-invisible-and-line-separator-char).
  A package coop-net newly imports fails `TestEmbeddedGatewayModuleLoadsOnItsOwn` until it is
  listed there, and that test's message now names `sourceDirs` as well as `sourceModules`.
- 2026-10-17 — recovery also takes an unsealed run whose cleanup is complete (a supervisor killed
  between its artifact cleanup and its seal) and seals it `supervisor_lost` without contacting the
  daemon, which it now binds only for a resource still at the runtime, so a run left with only its
  artifact is finished with Docker down too. `coop net recover` runs a pass for such a run instead
  of reporting nothing to clean, and `coop net inspect` attempts one; both skip runtime detection
  when only the seal is owed. `NetworkRecovery.Sealed` now means this pass wrote the receipt, and
  recover adds `Wrote its final record.` under `already cleaned up`. Dropped the stale line
  reference to `RecoverNetworkRuns`; re-verified the paragraph against `filtered_cleanup.go`,
  `network_recover.go` and `cli/net_cmd.go`.
- 2026-10-16 — the output chain refuses the agent's 127.0.0.11 (Docker's resolver) before the
  loopback accepts; the order paragraph names `initialRules` instead of a stale line number.
- 2026-10-16 — the tar carries its own go.mod and go.sum, narrowed to coop-net's three modules, and
  `Pack` moved into gatewayimage so the source-dir list lives once instead of three times. Verified:
  a real golang.org/x/text bump keeps `gateway-sources-check` green while an x/crypto bump that pulls
  x/sys turns it red; the narrowed module builds a byte-identical linux/amd64 coop-net offline with
  `-mod=readonly`; removing any of its six go.sum lines or the x/sys requirement fails the load.
- 2026-10-16 — the network setup's Go base now comes from this recipe (`GoBase()` reads its
  build stage) instead of the box image's removed Go tools stage; verified against
  `gatewayimage/Dockerfile`, `image.go` and `verifyNetworkCandidate`.
- 2026-10-15 — the image's Ubuntu packages install from a dated snapshot. Recorded the snapshot facts
  (every architecture under `ubuntu`, no `Valid-Until`, intended two-year retention, which three pins
  come from jammy-updates) and that a Dockerfile edit moves the fingerprint while `source.tar` stays
  identical; verified against `gatewayimage/Dockerfile`, `image.go` and apt installs of all nine pins
  from the snapshot in throwaway arm64 and amd64 containers.
- 2026-10-15 — the `--disable-hot-restart` trap now cites `GuardRuntime.Run` and says why (abstract
  sockets belong to the shared network namespace); this card holds it since the retired egress
  experiment and its card are deleted. The rest of the card was not re-verified.
- 2026-10-13 — native broker denial now latches under its comparison mutex before teardown;
  first-stop metadata cannot be replaced by lifecycle cleanup. Deterministic admission test
  fails on the old source and passes after repair; real stream and blocked-log cleanup tests
  remain green. Separate from the historical Claude refusal investigation.
- 2026-10-13 — qualified helper clocks by same boot and verified zero boottime offsets
  across isolated namespaces, retaining exact local/host custody. Private protocol2 rejects
  legacy/missing proof. Regression failed before repair; gateway race, capless Linux kernel
  proof and all five real filtered setup checks passed on OrbStack/Docker29.8.2.
- 2026-10-09 — root module metadata is embedded too: v11 retry exposed a missed refresh after
  a tidy classification change. Reverified generator file selection, added the missing inputs
  above and an early nonmutating canonical freshness check; no networking logic changed.
- 2026-10-06 — identity checks are a socket request, not a CLI process (task 2026-10-05-cut-the-filtered-start-below-3-s-fewer-serial-do).
- 2026-09-25 — rechecked the owner-private launch configuration, nft rules and socket collector:
  the host now supplies the agent UID explicitly; zero and gateway UID 65532 are refused. Regenerated
  the embedded helper source after the source change (an unregenerated live smoke failed closed).
- 2026-09-20 — those two refusals now carry the client's loopback source port, through the
  collector and the projection, rendered as a source rather than a destination.
- 2026-09-20 — recorded what `tls_direct_dial_refused`/`dns_query_invalid` actually mean (a client
  talking to Coop's own listeners, not egress) and that the report no longer counts them as traffic.
- 2026-09-20 — two more route kinds: `download` (public bytes, no credential, exact request lines
  including the query) and `mcp-sse` (GET the stream's path, POST anywhere on that one host).
- 2026-09-20 — `coop-net broker`: the same broker, in a helper beside an OPEN box, with a direct
  dialer, its own config (`OpenBrokerConfig`, MCP routes only), an address printed when its listeners
  accept, and stdin EOF as its stop signal.
- 2026-09-20 — the host makes the same header judgment first (`MCPSecretHeader`), and the proxy's
  own forwarded headers are refused as secret carriers.
- 2026-09-19 — an `mcp` route may carry its secret in one header of its own (`mcpSecretHeader`),
  not only `Authorization: Bearer`.
- 2026-09-19 — the broker admits through the guard's `admitLease` (waits out a busy controller), and
  the resolver asks again after an answer that arrived already expired; both surfaced as spurious
  broker denials when a filtered session opened two MCP servers at once.
- 2026-09-19 — broker routes gained a kind (`provider`, `mcp`) and a method set; up to 72 routes;
  `Admits` lets an exact route's path end in a slash (MCP endpoints use them); a prefix route still
  refuses a slash-terminated path.
- 2026-09-19 — the credential broker serves one loopback listener per brokered route (15580+i),
  each bound to its own substitute and endpoint (`Admits`).
- 2026-09-19 — a resolver connection dialed and released between two samples no longer leaves a
  false "may be missing" warning: `track()` reads its inode from the fd and the collector retires
  its released identity like a bound one (human decision A on task
  2026-09-18-attribute-the-gateway-s-own-resolver-sockets-aft). Supersedes the 2026-09-10 note that
  such a connection stays `unattributed_socket` on purpose.
- 2026-09-19 — `markReady` wakes the collector, so the first ready snapshot no longer waits out the
  1 s observation tick (start 3.97 s → 3.00 s)
- 2026-09-19 — the filtered launch overlaps its independent steps (volumes; guard creation beside the
  controller's start) and cleanup skips the observation block of a guard that never started (start
  4.33 s → 3.90 s); re-pointed the controller/guard line references, which had drifted
- 2026-09-19 — the terminal counters barrier wakes the sampler; filtered cleanup overlaps independent
  steps (stop 1.87 s → ~1.03 s)
- 2026-09-18 — the guard's flow lifetime and admission budget are separate contexts; the private
  leg's close used to follow the 10 s admission deadline and cut every long guarded TLS flow. The
  credential broker's dial was checked: its admission context bounds only the dial.
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
