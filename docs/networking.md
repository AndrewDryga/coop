# Restricted networking: what is supported

`coop <agent> --egress filtered` runs a box behind a per-run gateway: the box reaches the
destinations you approved and nothing else. This page is the support matrix — what the runtime
enforces today, what it refuses and why, what it measures, and how to ask for more.

Nothing here is a proxy setting. Every tool in the box — the provider CLI, `curl`, an SDK, a
subprocess — hits the same boundary, with no `HTTPS_PROXY` to set or forget.

Start with `coop net setup` once per machine, then `coop net` for what this project may reach and
`coop net --help` for the verbs.

## Supported today

| Rule | Meaning | Enforced by |
| --- | --- | --- |
| `to: {domain: "api.example.com"}` · `protocol: tls` · `ports: [443]` | that exact name over TLS 443 | visible-SNI routing to a validated IPv4 address |
| `to: {domain: "*.example.com"}` · `protocol: tls` · `ports: [443]` | one label under that name — `www.example.com`, but never the apex and never `a.b.example.com` | the same matcher in DNS admission and SNI routing |
| `to: {domain: "dns.google"}` · `protocol: tls` · `ports: [853, 8443]` | that name over TLS on those ports, and on no other | the capture chain redirects exactly those ports; the guard reads the port back from the kernel |
| `to: {ip: "10.42.8.12"}` / `to: {cidr: "10.42.9.0/24"}` · `protocol: tcp\|udp` · `ports: [...]` | raw TCP or UDP to those IPv4 addresses on those ports | an nftables accept rule with its own kernel counter |
| `to: {ip: ...}` / `to: {cidr: ...}` · `protocol: icmp` · `types: [echo-request]` | IPv4 ping to those addresses | an nftables accept rule; echo replies return on conntrack |
| `to: {provider: claude}` | that provider's maintained core endpoints | expanded from the trusted release bundle into concrete TLS rules |
| `to: {service: "db"}` · `protocol: tcp` · `ports: [5432]` | one Compose sidecar belonging to this project | the container's exact address, read from the runtime at launch |
| `serve: {ports: [3000]}` in `.agent/project.yaml` | the host browser reaches the box's dev server | the port is published on the gateway container, which owns the box's network namespace |

A `service:` grant is a request like any other: it is approved by a human, it names one service, and
it opens that container's address and ports only. Joining the services network does not grant
anything — every other container on it stays behind the same default deny as the public internet.
`box.network: true` alone is refused in filtered mode: name the sidecar you need.

The approval captures the *definition* a human reviewed, not just the name: `coop net approve`
records a digest of that service's Compose stanza, and a launch recomputes it from the file it is
about to run. Rewriting `db:` into something else — a proxy image with ordinary egress, say — is
refused with `the Compose service "db" changed since it was approved — review it with 'coop net approve'`,
and `coop net` shows it as a pending change. Editing an unrelated service changes nothing.

**TLS is not only 443.** A `tls` rule names the ports it wants — `[443]`, `[853]`, `[443, 8443]` —
and the gateway captures exactly that set. The port an upstream is dialed on comes from the
kernel's own record of the redirect (`SO_ORIGINAL_DST`), never from the client: the same name on a
port the rule does not name is refused with its port in the event, and a connection made straight
to the guard's listener declares no port at all, so it is refused and counted too. Port 53 stays
the gateway's own, and a `serve` port may not collide with a captured one.

**Inside the box, localhost is yours.** A test server on `127.0.0.1:3000` and a `curl` to it never
touch the boundary: the box's loopback is its own namespace, not a host surface. Only what leaves
the box meets the gateway.

**Your project's image, on coop's clients.** A repo whose `.agent/Dockerfile` starts with
`ARG COOP_BASE_IMAGE` / `FROM ${COOP_BASE_IMAGE}` runs its own image in filtered mode too. The
launch builds it with coop's locked client image as the base, under its own tag
(`coop-<repo>-filtered:<client image>`, never the tag `coop build` writes), and reuses it until the
Dockerfile or the client image changes. Two proofs stand between that image and the box, and
neither reads the Dockerfile — a `FROM` line is a claim about what was built, not evidence:

- the locked image's layers must be the first layers of the built image, so the box is that exact
  image plus your own; and
- every pinned client entry point — each launcher, what it execs, and the native binaries it needs —
  must be byte-for-byte the file in the locked image, read out of a container that is created and
  never started, so nothing from the image runs to describe itself.

Adding tools passes. Replacing, wrapping or deleting a pinned client is refused by name, and so is
an image that was not built on the locked one. The host qualification keeps naming coop's own image:
your image inherits nothing from it except through those two proofs, which re-run on every launch.
`COOP_IMAGE` stays refused — an arbitrary image has no such proof to offer.

Three practical notes. The base ends as the non-root box user, so a package install needs
`USER root` … `USER node` around it. A repo with an `.agent/Dockerfile` still needs `coop build`
once, exactly as every other coop command in that repo does. And the filtered build is run by the
launch, not by a human `coop build`: its `RUN` lines execute as root, with ordinary network access,
on your Docker. The two proofs bind what the box RUNS, not what a build may do — so treat
`.agent/Dockerfile` as the code it is, and coop says so out loud when nobody has committed it.

## Refused, with the reason you will see

| Requested | Message |
| --- | --- |
| IPv6 address, CIDR, or `icmpv6` | `IPv6 destinations are not supported yet — a filtered box runs on IPv4 only` |
| `tls` on port 53 | `TLS on port 53 is not allowed here — the gateway takes 53 for DNS; use 443 or another port` |
| raw `tcp`/`udp` on port 443 or 53 | `raw tcp to <dest> on port 443 is not allowed — the gateway takes 443 for TLS and DNS; use another port` |
| raw `tcp` on a port a `tls` rule also names | `raw tcp to <dest> on port 8443 clashes with a TLS rule on the same port — the gateway takes that port for TLS; use another port` |
| a `serve` port on a captured TLS/DNS port | `port 8443 is one the gateway uses for TLS and DNS — serve on another port in filtered mode` |
| ICMP beyond echo-request | `ICMP N to <dest> is not supported yet — only echo-request is` |
| `cidr: 0.0.0.0/0` | `a /0 rule allows everything, which is not filtering — use --egress open if that is what you want` |
| a host, loopback, link-local or metadata range | `<range> is a protected range (your host, loopback, link-local or cloud metadata) — no rule can allow it` |
| a project image not built on the client image | `this project's .agent/Dockerfile did not build on coop's client image — start it with ARG COOP_BASE_IMAGE and FROM ${COOP_BASE_IMAGE} …` |
| a project image that changes a pinned client | `this project's .agent/Dockerfile changes claude's cli client at /usr/local/bin/claude — a filtered box runs the clients this host's setup qualified …` |
| `COOP_IMAGE` | `a filtered box runs coop's own image — unset COOP_IMAGE to start one` |
| a runtime other than Docker | `this host is not set up for filtered runs with this Docker and these agents — run 'coop net setup'` |
| `box.network: true` with no `service:` grant | `a filtered box does not join the shared services network — ask for the one sidecar you need with a to: {service: <name>} rule …` |
| `-v /var/run:/x` (or any mount of `/run`, `/proc`, `/sys`, `/dev`, `/`, or the Docker socket's directory) | `a filtered box cannot mount …: it is or holds …, which reaches Docker or the kernel` |
| an approved project directory replaced by another at the same path | `the project directory at <path> was replaced since it was approved — review it with 'coop net approve'` |

A refused rule fails the launch itself, before any approval is written or any container is created.
Coop never accepts a rule it cannot enforce and then quietly drops the constraint.

Some destinations can never be granted, inside a `cidr:` grant or anywhere else: the host's own
interface addresses, every subnet the container runtime allocates and every gateway it holds in one
(so sibling containers, other sessions' boxes and the daemon's own address are out of reach), and
the loopback/link-local/metadata ranges. The inventory is taken from the daemon at launch. A
permitted CIDR does not beat it — those packets are dropped and counted as protected. The single
exception is an approved `service:` grant: that ONE container address is what a human approved, so
it is permitted before the protected drop and nothing else in its subnet is.

A published `serve` port is host ingress, and only host ingress: the rule matches the bridge
gateway address the host's traffic is NAT'd from, so a sibling container on the same bridge cannot
connect to it.

The gateway is the boundary for PACKETS, so a filtered box also refuses the mounts that would go
around it: a bind of the runtime's control surface (`/var/run`, `/run`, `/proc`, `/sys`, `/dev`,
`/`, or the Docker endpoint's own socket directory) is refused by name — one `curl` over a daemon
socket would start a container the gateway never sees.

## Observed versus counted

- **TLS flows are observed.** The gateway sees each connection, so `coop net inspect <run>` shows
  live connections, names, byte counts and the rule that admitted them, and a refused name becomes
  a retained event `coop net explain <event>` can open.
- **Raw transports are counted.** Every address grant has its own kernel counter, reported per
  grant (`Raw rules` in `coop net inspect`, `address_grants` in `--json`) as packets and bytes.
- **Raw refusals are counted, not attributed.** The packet filter drops a refused datagram
  without recording where it was going, so refused packets are a number — `N raw packets refused
  too, with no destination recorded` in the end-of-run summary — and there is nothing for
  `explain` to open. Coop will not invent a destination for them.
- **Unknown means unknown.** A metric nobody measured is reported as UNKNOWN, never as zero.

`coop net why <destination> --run <id>` checks the rules that run started with, without sending a
packet. A domain is TLS on 443 unless `--port <n>` names another; an address has no implied
transport, so pass `--protocol tcp|udp --port <n>`, or `--icmp` for echo-request.

## Asking for access

Put the rule in `.agent/project.yaml` under `box.egress_rules` and ask a human to run
`coop net approve` on the host. The repository file is a *request*: the approval is remembered
outside the repository, so editing or deleting the file cannot widen access, and nothing inside a
box can approve itself. Approvals apply to new runs; a box already running keeps the policy it
launched with.

For a single invocation, the operator can pass `--allow-domain <name>` (exact TLS 443) or
`--egress-rules <file>` with a full rule document. A file inside an agent mount is a request, not a
grant — where the file lives decides its authority. `coop net approve` applies the same capability
gate a launch does, so a rule this runtime could never enforce (an IPv6 destination, say) is
refused at review instead of being remembered and refused at every launch.

An approval is bound to the project DIRECTORY, not to its path: moving the approved checkout aside
and putting another there refuses the next launch until a human reviews it again.

## When a run is interrupted

A filtered run's gateway containers, volumes and receipt are owned by the coop process that started
it. If that process is killed (`SIGKILL`, a crash, a reboot) the run stays `cleanup pending`:

    coop net recover <run>     # settle one run
    coop net recover --all     # settle every pending run

Recovery removes exactly the resources that run recorded — by id and ownership labels, never by
name prefix, never a prune — and seals a final receipt marked `partial` with workload
`supervisor_lost`. A run whose supervisor is still alive is refused with its pid. The same
recovery runs automatically on the next `coop` start, alongside the ordinary orphan-box sweep.
