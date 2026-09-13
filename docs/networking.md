# Restricted networking: what is supported

`coop <agent> --egress filtered` runs a box behind a per-run gateway: the box reaches the
destinations you approved and nothing else. This page is the support matrix — what the runtime
enforces today, what it refuses and why, what it measures, and how to ask for more.

Network enforcement is not a proxy setting. Every tool in the box — the provider CLI, `curl`, an
SDK, a subprocess — hits the same packet boundary, with no `HTTPS_PROXY` to set or forget. The
narrow Claude API-key credential broker described below runs behind that boundary; it does not
replace it.

`coop net` says what a new run in this project can reach and why; `coop net runs` lists what
recorded runs did; `coop net --help` has the verbs.

A new project asks for filtered access from the start: `coop init` writes `box.egress: filtered`
into `.agent/project.yaml`. The file's other spellings are `offline` (no network at all) and
`open` (everything, unfiltered) — and `open` is a request like any rule: it takes effect only after
a human approves it on the host. An existing project file is never rewritten by a re-init.

## Setting up a host

A filtered box needs this machine qualified once: coop builds (or reuses) the pinned gateway and
locked client images for the bound Docker daemon, runs one smoke through the ordinary launch engine
and records what it proved. The first filtered launch does this on its own, shows the transcript,
and continues into the box once the five checks pass; a later launch on a host whose proof is
current adds no output and no work. When Docker is not running, or a check fails, the launch stops
before any box starts and says why. Qualification grants no project access — the approval below is
a separate, human decision.

`coop net setup` runs the same qualification now, for paying the cost ahead of an unattended run,
rechecking a host after a Docker or coop upgrade, or diagnosing one:

    Setting up restricted networking using Docker 29.4.0 on linux/arm64.
    The gateway and client images are already available and will be reused.

    Checking filtered access:
      ✓ approved TLS access to example.com works
      ✓ unapproved domains are blocked
      ✓ direct IP connections cannot bypass domain rules
      ✓ the cloud metadata address is blocked
      ✓ DNS does not resolve unapproved domains

    ✓ All 5 checks passed — this host is ready for filtered runs

A failed check keeps the passes before it, names what actually happened in its place, claims
nothing about the checks after it, and ends with `✗ this host is not ready for filtered runs` and
`No setup was saved`. Images that need building say so up front, and Docker's build output follows.

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

**Provider access is what the client needs to work, and nothing it merely chats to.** A filtered
`coop claude` reaches `api.anthropic.com`, `platform.claude.com` (the OAuth refresh) and
`mcp-proxy.anthropic.com` (the claude.ai connectors a login has on by default); a filtered
`coop codex` reaches `chatgpt.com` and `auth.openai.com`; Gemini reaches
`generativelanguage.googleapis.com` with a portable AI Studio API key; and Grok reaches
`cli-chat-proxy.grok.com` and `code.grok.com` with a portable access file. Gemini OAuth and Vertex
AI credentials are not supported in filtered mode and are refused before launch. The client's own
release feed, package registry, update check
and telemetry — `raw.githubusercontent.com`, `registry.npmjs.org`, `api.github.com`, the Datadog
intakes — are deliberately not in any bundle. Instead, every box switches that traffic off with
the client's own controls, in the box only: Claude runs with
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` and `DISABLE_UPDATES=1`, codex reads a generated
`config.toml` that sets `check_for_update_on_startup = false`, `analytics.enabled = false` and the
`otel` exporters to `none` on top of your own settings, and gemini gets `general.enableAutoUpdate`,
`general.enableAutoUpdateNotification` and `privacy.usageStatisticsEnabled` off plus
`GEMINI_TELEMETRY_ENABLED=false`. Your host profiles are not edited. So a session that only answers
a prompt records no refusals — and if an agent or your project later does reach for one of those
hosts on purpose, that refusal is recorded, shown and approvable like any other; nothing is
filtered out of the report.

**A Claude API key stays outside a direct filtered box.** With the locked Claude CLI 2.1.260, the
default profile, and `ANTHROPIC_API_KEY` as the only active Claude credential, Coop automatically
replaces the reusable key with a random credential valid for this gateway generation and points
the client at a loopback broker. The capless guard injects the real key only for
`POST /v1/messages` to `api.anthropic.com`; the agent policy itself does not grant that domain.
The broker uses the same guarded Envoy path, so its provider connection keeps normal traffic
attribution. Stopping the box revokes the substitute and cancels open streams.

This first slice is deliberately narrow. Ordinary Claude OAuth runs retain their existing
credential handling and are not broker-protected; restricted and session projections keep their
existing access-only copies. ACP, login, read-only/bare, peer, preset, and remote-session API-key
runs are refused instead of silently receiving the reusable key. A configured custom
`ANTHROPIC_BASE_URL` is refused rather than silently redirected to Anthropic. Other providers need
their own pinned client and protocol qualification before they can use this boundary.

A `service:` grant is a request like any other: it is approved by a human, it names one service, and
it opens that container's address and ports only. Joining the services network does not grant
anything — every other container on it stays behind the same default deny as the public internet.
`box.network: true` alone is refused in filtered mode: name the sidecar you need.

The approval captures the *definition* a human reviewed, not just the name: `coop net approve`
records a digest of that service's Compose stanza, and a launch recomputes it from the file it is
about to run. Rewriting `db:` into something else — a proxy image with ordinary egress, say — is a
pending change like any other: a launch refuses with `the Compose service "db" changed since it was
approved` and the review command, and `coop net` shows it the same way. Editing an unrelated
service changes nothing.

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

Copying those entry points out of an image costs a few seconds, so what a read found is recorded
outside every agent mount, next to your approvals: host setup records what the locked image
holds, and a launch records what it read out of the image your Dockerfile built. Both records are
keyed by the image ID, which is a content address — a rebuilt image is a new ID and is read again,
and a record that is missing, damaged or for another image is read past, never trusted. The
comparison itself still runs on every launch.

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
| a runtime other than Docker | `restricted networking requires a local Docker runtime` |
| `box.network: true` with no `service:` grant | `a filtered box does not join the shared services network — ask for the one sidecar you need with a to: {service: <name>} rule …` |
| `-v /var/run:/x` (or any mount of `/run`, `/proc`, `/sys`, `/dev`, `/`, or the Docker socket's directory) | `a filtered box cannot mount …: it is or holds …, which reaches Docker or the kernel` |
| a project file that asks for access nobody approved (a rule, a change to one, or `open`) | `<Agent> cannot start because this project asks for network access that has not been approved` · `Review it: coop net approve` |
| an approved project directory replaced by another at the same path | `<Agent> cannot start because the project directory at <path> was replaced since it was approved` · `Review it: coop net approve` |

A refused rule fails the launch itself, before any approval is written or any container is created.
Coop never accepts a rule it cannot enforce and then quietly drops the constraint.

Some destinations can never be granted, inside a `cidr:` grant or anywhere else: the host's own
interface addresses, every subnet the container runtime allocates and every gateway it holds in one
(so sibling containers, other sessions' boxes and the daemon's own address are out of reach), and
the loopback/link-local/metadata ranges. The inventory is taken from the daemon at launch, and it
follows the host while the box runs: when another filtered box starts and creates its network, or a
VPN brings an interface up, the new subnets, gateways and host addresses are added to the running
box's protected set in place — one atomic kernel update — and refused from that moment. A box is
stopped for topology only when that update is refused or the envelope would pass 256 ranges; an
address that disappears keeps its denial. A permitted CIDR does not beat the protected set — those
packets are dropped and counted as protected. The single exception is an approved `service:` grant:
that ONE container address is what a human approved, so it is permitted before the protected drop
and nothing else in its subnet is.

A published `serve` port is host ingress, and only host ingress: the rule matches the bridge
gateway address the host's traffic is NAT'd from, so a sibling container on the same bridge cannot
connect to it.

The gateway is the boundary for PACKETS, so a filtered box also refuses the mounts that would go
around it: a bind of the runtime's control surface (`/var/run`, `/run`, `/proc`, `/sys`, `/dev`,
`/`, or the Docker endpoint's own socket directory) is refused by name — one `curl` over a daemon
socket would start a container the gateway never sees.

## Observed versus counted

`coop net inspect [<run>]` (the newest run of this project when no run is named) leads with the
traffic: the allowed aggregate, then every destination the workload reached — hostname and port,
transport, each resolved peer with its connection count and bytes sent and received — and then
only what went wrong: blocked attempts, alerts, gaps in the evidence, a layer that did not run
normally, a cleanup still owed. A clean run prints nothing else; `--json` keeps every field.

An interactive filtered box prints that same view itself, on stderr, once its teardown has
sealed the receipt — under `Networking stats:`, with each destination's own totals on its row
and no tool prefix even though it follows the agent's own output. Before it, one line says why
the box stopped: `The Coop box has stopped — main process exited with status 0`, or
`— interrupted by Ctrl-C` when a signal reached coop (never guessed from an exit status). The
sections before the agent started — `Protecting secrets`, `Configuring network access` listing
each provider's endpoints, `Applied N approved network rules` and the configured MCP servers
before `✓ Everything else blocked`, then `Starting <agent>` — carry no prefix at all.

- **TLS flows are observed.** The gateway sees each connection, so a destination row is the name
  the workload asked for, the address it resolved to, and the bytes that crossed. A blocked name
  becomes a retained event, and `coop net blocked <host>` opens the newest one for that host in
  this project (`--run <run>` pins one run; an exact event ID replaces the host with `--run`).
- **Raw transports are counted.** Every address grant has its own kernel counter, reported per
  grant (`Raw traffic` in `coop net inspect`, `address_grants` in `--json`) as packets and bytes.
  No host inside a CIDR is recorded, so none is shown.
- **Raw refusals are counted, not attributed.** The packet filter drops a refused datagram
  without recording where it was going, so refused packets are a number — `N raw packets were
  blocked with no remote address recorded` — and there is nothing for `coop net blocked` to
  open. Coop will not invent a destination for them.
- **Unknown means unknown.** A metric nobody measured is reported as UNKNOWN, never as zero; a
  group with one unmeasured member is UNKNOWN rather than a total that quietly counted it as zero.
- **Only proven workload traffic is `Allowed`.** Coop's own resolver connection and an ownerless
  closing kernel socket stay in `--json`; a socket that genuinely could not be attributed is
  listed under `Other observed endpoints` with the reason, never as allowed traffic.

Runs are named by any unique prefix of their id — the eight characters `coop net runs` shows are
enough for every run command; an ambiguous prefix is refused with the prefixes that would settle
it. `coop net runs` shows this project's 25 newest runs, with a count and `--all` only when
there are more (`--all-projects` shows every project's, labeled by name and path).

`coop net check <url-or-host>` says whether a normal new run in this project can reach a
destination — from the approved project rules and the provider access each agent brings — with one
verdict and one cause. `--run <run>` asks the same of the rules that recorded run started with.
Nothing is sent either way. An address has no implied transport, so it takes `--protocol tcp|udp
--port <n>` (or `--icmp`) and a run, because a run's protected ranges are part of the answer.

`coop net export <run>` writes the sealed record as JSON with destination names and addresses
withheld — even a blocked name can carry a secret — and `--include-addresses` puts them back on
your own machine. Its `digest` is a content checksum of that projection, not a signature or a
claim that partial evidence is complete.

## Asking for access

Put the rule in `.agent/project.yaml` under `box.egress_rules` and ask a human to run
`coop net approve` on the host. The repository file is a *request*: the approval is remembered
outside the repository, so editing or deleting the file cannot widen access, and nothing inside a
box can approve itself. Approvals apply to new runs; a box already running keeps the policy it
launched with.

The approval is the exact snapshot of the file — its mode, its rules and the reviewed definition
of every `service:` it names. `coop net approve` has no flags: to change access, edit the file and
review it again. The review is one diff against what is already approved, unchanged rows marked
`already approved`, additions `new request`, removals `no longer requested`; a mode change is
explained in plain words, and a request for `open` gets a red warning because nothing would be
blocked. Confirm with `Approve these changes for new runs? [y/N]`. A file that is already exactly
what was approved prints `No approval needed — this project's network access has not changed.`
and writes nothing. Provider endpoints an agent brings with it are not part of this diff: coop
grants those itself, and they are never shown as project access.

While the file and the approval differ, the request is *pending*: `coop init` ends with
`⚠ This project asks for network access that has not been approved` and `Review it: coop net
approve`, bare `coop net` shows the same diff under `New runs cannot start until this project's
network request is approved`, and every launch — `coop run`, a named agent, `coop acp`,
`coop loop` — refuses before any box or main process starts, naming the same review. A project
with nothing pending pays no line for any of this.

For a single invocation, the operator can pass `--allow-domain <name>` (exact TLS 443) or
`--egress-rules <file>` with a full rule document. A file inside an agent mount is a request, not a
grant — where the file lives decides its authority. `coop net approve` applies the same capability
gate a launch does, so a rule this runtime could never enforce (an IPv6 destination, say) is
refused at review instead of being remembered and refused at every launch.

An approval is bound to the project DIRECTORY, not to its path: moving the approved checkout aside
and putting another there refuses the next launch until a human reviews it again. `coop net` reports
that as blocked and names the command that fixes it.

`coop net forget` takes an approval back. It shows what the project remembered, asks on the terminal,
and removes exactly that one record: run it in the project, or pass `--project <path>` for a checkout
that is already gone. The runs recorded here, their receipts and this host's setup stay, and the next
filtered run asks for approval again. A path that was a *symlink* was approved as the directory it
pointed at, so once both are gone the record can no longer be located from that path — forget says so
instead of removing another project's.

## When a run is interrupted

A filtered run's gateway containers, volumes and receipt are owned by the coop process that started
it. If that process is killed (`SIGKILL`, a crash, a reboot) the run stays `cleanup pending` until
coop settles it — which it does on its own: `coop net inspect` of that run makes one bounded
recovery attempt before it says anything about cleanup, and every loop or fork start sweeps
pending runs. A successful recovery is silent. Only something external — Docker stopped, a
different daemon at the recorded endpoint, a removal that failed — is reported, as
`⚠ Cleanup incomplete — <the blocker>` with what to do about it; coop retries itself.

    coop net recover           # settle every pending run now
    coop net recover <run>     # settle one run now

Recovery removes exactly the resources that run recorded — by id and ownership labels, never by
name prefix, never a prune — and seals a final receipt marked `partial` with workload
`supervisor_lost`. A run whose supervisor is still alive is left to that process.
