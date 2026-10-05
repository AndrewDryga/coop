# Restricted networking: what is supported

`coop <agent> --egress filtered` runs the box behind a per-run gateway. The box reaches the
destinations you approved and nothing else. This page is the support matrix: what the runtime
enforces today, what it refuses and why, what it measures, and how you ask for more.

Filtering works on the network itself, not through a proxy setting. Every tool in the box hits the
same packet boundary: the provider CLI, `curl`, an SDK, a subprocess. There is no `HTTPS_PROXY` to
set or forget. The [credential broker](#api-keys-stay-outside-the-box) for provider API keys runs
behind that boundary. It doesn't replace it.

`coop net` says what a new run in this project can reach, and why. `coop net runs` lists what
recorded runs did. `coop net --help` lists the verbs.

## Network modes

A new project asks for filtered access from the start: `coop init` writes `box.egress: filtered`
into `.agent/project.yaml`. The file's other spellings are `offline` (no network at all; the
command line calls it `--egress none`) and `open` (everything, unfiltered). `open` is a request like any rule, so it takes effect only after a human
approves it on the host. Running `coop init` again never rewrites an existing project file.

## Setting up a host

A filtered box needs its machine qualified once. co:op builds (or reuses) the pinned gateway and
locked client images for the Docker daemon it's bound to. Then it runs one smoke test through the
ordinary launch engine and records what it proved.

The first filtered launch does this on its own. It shows the transcript and continues into the box
once the five checks pass. A later launch on a host whose proof is current adds no output and no
work. If Docker isn't running, or a check fails, the launch stops before any box starts and says
why. Qualification grants no project access. [Approval](#asking-for-access) is a separate decision
that a human makes.

`coop net setup` runs the same qualification now. Use it to pay the cost before an unattended run,
to recheck a host after a Docker or co:op upgrade, or to diagnose a host:

    Setting up restricted networking using Docker 29.4.0 on linux/arm64.
    The gateway and client images are already available and will be reused.

    Checking filtered access:
      ✓ approved TLS access to example.com works
      ✓ unapproved domains are blocked
      ✓ direct IP connections cannot bypass domain rules
      ✓ the cloud metadata address is blocked
      ✓ DNS does not resolve unapproved domains

    ✓ All 5 checks passed — this host is ready for filtered runs

A failed check keeps the passes before it and names what actually happened in its place. It claims
nothing about the checks after it. The output ends with `✗ this host is not ready for filtered runs`
and `No setup was saved`. If images need building, the output says so up front, and Docker's build
output follows.

## Supported today

| Rule | Meaning | Enforced by |
| --- | --- | --- |
| `to: {domain: "api.example.com"}` · `protocol: tls` · `ports: [443]` | that exact name over TLS 443 | visible-SNI routing to a validated IPv4 address |
| `to: {domain: "*.example.com"}` · `protocol: tls` · `ports: [443]` | one label under that name (`www.example.com`), but never the apex and never `a.b.example.com` | the same matcher in DNS admission and SNI routing |
| `to: {domain: "dns.google"}` · `protocol: tls` · `ports: [853, 8443]` | that name over TLS on those ports, and on no other | the capture chain redirects exactly those ports; the guard reads the port back from the kernel |
| `to: {ip: "10.42.8.12"}` / `to: {cidr: "10.42.9.0/24"}` · `protocol: tcp\|udp` · `ports: [...]` | raw TCP or UDP to those IPv4 addresses on those ports | an nftables accept rule with its own kernel counter |
| `to: {ip: ...}` / `to: {cidr: ...}` · `protocol: icmp` · `types: [echo-request]` | IPv4 ping to those addresses | an nftables accept rule; echo replies return on conntrack |
| `to: {provider: claude}` | that provider's maintained core endpoints | expanded from the trusted release bundle into concrete TLS rules |
| `to: {service: "db"}` · `protocol: tcp` · `ports: [5432]` | one Compose sidecar belonging to this project | the container's exact address, read from the runtime at launch |
| `serve: {ports: [3000]}` in `.agent/project.yaml` | the host browser reaches the box's dev server | the port is published on the gateway container, which owns the box's network namespace |

In a filtered run, `COOP_RUN_ARGS` and extra runtime arguments accept bind mounts, `-e KEY=VALUE`
and `--label KEY=VALUE` only. Any other flag is refused by name. co:op's own tracking labels take
precedence over your labels with the same key.

## Provider access

Each agent brings the provider access its client needs to work, and nothing it merely chats to.

| Agent | Hosts a filtered run reaches |
| --- | --- |
| `coop claude` | `api.anthropic.com`, `platform.claude.com` (the OAuth refresh) and `mcp-proxy.anthropic.com` (the claude.ai connectors a login has on by default) |
| `coop codex` | `chatgpt.com` and `auth.openai.com` |
| `coop gemini` | `generativelanguage.googleapis.com`, with a portable AI Studio API key |
| `coop grok` | `cli-chat-proxy.grok.com`, `code.grok.com` and `auth.x.ai` (the OAuth refresh) |

Gemini OAuth and Vertex AI credentials aren't supported in filtered mode, and they're refused before
launch.

A client's own release feed, package registry, update check and telemetry
(`raw.githubusercontent.com`, `registry.npmjs.org`, `api.github.com`, the Datadog intakes) are left
out of every bundle on purpose. Instead, every box switches that traffic off with the client's own
controls. These settings apply in the box only, and co:op doesn't edit your host profiles. co:op's
images carry each client's update switch as well, so no client in any box updates itself, whether
its home is mounted or not.

| Client | In the box | Update switch in co:op's image |
| --- | --- | --- |
| Claude | runs with `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` and `DISABLE_UPDATES=1` | `DISABLE_UPDATES` |
| Codex | reads a generated `config.toml` that sets `check_for_update_on_startup = false`, `analytics.enabled = false` and the `otel` exporters to `none`, on top of your own settings | `/etc/codex/managed_config.toml` |
| Gemini | gets `general.enableAutoUpdate`, `general.enableAutoUpdateNotification` and `privacy.usageStatisticsEnabled` off, plus `GEMINI_TELEMETRY_ENABLED=false` | `/etc/gemini-cli/settings.json` |
| Grok | runs with `GROK_TELEMETRY_ENABLED=false` and `GROK_DISABLE_AUTOUPDATER=1` | `GROK_DISABLE_AUTOUPDATER` |

So a session that only answers a prompt records no refusals. If an agent or your project later
reaches for one of those hosts on purpose, that refusal is recorded, shown and approvable like any
other, and nothing is filtered out of the report.

## API keys stay outside the box

Every provider account a run selects that holds a supported API key becomes one route of the run's
broker. That includes the lead's account, a peer's and a preset role's. co:op replaces each key with
a random credential that's valid only for this gateway generation and that route. It points every
process of that provider in the box at the route's own loopback listener: the lead, the consult and
delegate helpers, and the editor adapter.

The capless guard injects the real key only for that provider's qualified API route. The agent's
policy itself doesn't grant the API domain. The broker uses the same guarded Envoy path, so provider
traffic keeps normal attribution. Stopping the box revokes every substitute and cancels open
streams.

A box holds one account per provider, so every teammate of a provider in the box shares its one key.
Keys and signed-in accounts of different providers mix freely. Several accounts of one provider run
side by side in separate boxes, each with its own broker, as in loop rotation, editor account
switches and remote sessions.

One filtered policy can't switch a provider between an API key and a sign-in. The sign-in needs the
API granted, and that is the grant the broker withholds. So a loop or preset ladder that mixes them
stops before launch. An editor session offers a provider's accounts of one kind only: the kind of
the account it names, or else the kind of the first one it can use.

These are the qualified routes. Each is shaped to the request its pinned client actually sends.

| Provider | Key | Route |
| --- | --- | --- |
| Claude | `ANTHROPIC_API_KEY` | `api.anthropic.com/v1/messages` |
| Gemini | `GEMINI_API_KEY` | `generativelanguage.googleapis.com/v1beta/models/` |
| Codex | `OPENAI_API_KEY` | `api.openai.com/v1/responses` |

The pinned Grok client has no qualified API base override, so `XAI_API_KEY` is refused. Claude's
alternate token variables, Gemini's Vertex `GOOGLE_API_KEY` and Codex's alternate key and
access-token variables are refused too, instead of entering a box.

A direct Claude run with `--readonly --egress filtered` can broker `ANTHROPIC_API_KEY` too. That
doesn't qualify other providers' read-only or bare modes. A controller job in read-only or bare
mode still can't use filtered networking. These stop before launch:

- an API-key run in login or bare mode, or with open or offline networking;
- a configured custom provider base URL;
- a key that only a client's own credential file holds.

Ordinary provider-native OAuth and access-token files keep their existing handling, and the broker
doesn't protect them. Restricted and session projections keep their existing access-only copies. A
remote session hands its child a selected API key the way the host keeps it, in the session's
private host-side config. It removes the key after each turn and when the session closes.

## A client's own downloads

A brokered client's own public downloads go through co:op too. A client sometimes fetches something
on its own that isn't the model API and that no login grants. Codex's curated plugin store is one:
an API-key run fetches it from chatgpt.com and github.com on every start.

co:op brokers those fetches on a `download` route. No credential is sent, and any credential the box
offers is refused. The route forwards only the exact request lines the adapter declared, query
included. For Codex those are:

| Host | Request lines |
| --- | --- |
| chatgpt.com | `GET /backend-api/plugins/featured?platform=codex` |
| github.com | `GET /openai/plugins.git/info/refs?service=git-upload-pack` and `POST /openai/plugins.git/git-upload-pack` |

The github.com lines fetch one public repository and never push. The same discovery path with
`service=git-receive-pack` isn't in the set.

The client's own configuration points it at those listeners. For Codex, that's `chatgpt_base_url` in
its managed layer and an `insteadOf` for that one repository in the box's co:op-owned git config.
Those hosts aren't added to the agent's policy, so anything the routes don't name is still refused
and still shows in `coop net inspect`.

## MCP servers

### Filtered runs

MCP servers' bearer tokens stay outside a filtered box too. Each shared MCP server that
authenticates with `bearer_token_env_var` becomes one more route of the run's broker, after the
provider routes. The box's copy of the MCP configuration points the server at the route's loopback
listener. It names a co:op-owned variable, `COOP_MCP_TOKEN_<n>`, that holds a stand-in valid only
for that route and run. The guard sends the real token upstream.

The route admits the server's exact URL path and the methods the MCP protocol uses (POST, GET,
DELETE). It waits as long as a tool call takes. Each bearer server's host is withheld from the
agent's own policy.

Your own variable in co:op's env file never reaches a filtered box, whether or not that box loads
MCP. A session's ACP adapter gets the stand-in. A missing token, or one set through `-e`, stops the
launch and names the server. A remote session's controller-provided tools use this same exact-path
route.

### Legacy SSE servers

A server on the legacy SSE transport gets a wider route, because it can't take a narrow one. It
names its own message endpoint at runtime, on a path only that connection knows. Its route admits
`GET` on the stream's own path and `POST` on any path of the same host. Nothing else gets through:
no other host, no other method, and still one secret header.

The credential stays outside the box, which is the point. But the box can reach more of that host
than a streamable-HTTP server's route allows, so the launch names those servers and says what to ask
their vendor for. This is compatibility support. When the server offers a streamable HTTP url, use
it, and the route narrows again.

If such a server answers with an absolute endpoint url instead of a path, the client posts straight
to it and skips co:op. A filtered run's policy refuses that post, and in an open run it carries only
the stand-in. It fails closed, but it reads as "that server does not work". The server's streamable
HTTP url is the fix.

### Offline runs

Offline runs leave remote MCP servers out. A box with no network can't reach a server by URL, so an
offline run drops every remote server from the box's MCP configuration. It says so at launch ("MCP
servers that need internet are left out: …"). Local (command) servers stay, and the remote servers'
token variables never enter the box. An offline remote session's editor adapter gets only the local
servers.

### Open runs

Open runs broker their MCP secrets through a helper beside the box. An open box reaches the internet
directly, so nothing on its path can hold a secret for it.

When its MCP configuration has a secret-bearing remote server, co:op starts one helper container
from the same gateway image (`coop-net broker`) on the box's own network, and gives the box a hosts
entry for it. The helper is unprivileged (`--cap-drop ALL`, uid 65532, read-only root, 256 MB) and
labelled `coop=broker` with the box's own supervisor labels. It ends with the `coop` process that
started it: it exits when its input closes, however that process ends. In the rare case one is left
behind, the box sweep reaps it.

The box's copy of the configuration points each brokered server at `http://coop-broker:<port>`, where
the port is 15580 plus the server's index, with a stand-in. The helper replaces the stand-in with the real credential and dials only that server's
host. Your variable never enters the box.

That hop is plain HTTP on a container network. When your project has no co:op-managed network, the
helper joins the default bridge, which every un-networked container on the host shares. There, the
stand-in itself protects the credential, not the network. It's 256 bits, minted per run and accepted
only on its own listener, for its own server's route. co:op owns the box's hosts entry for that name
and refuses a run whose own `--add-host` would rebind it.

A remote open session's child hands over its adapter list the same way, so the controller-provided
tools are brokered too. The helper's image is built on first use, like a first filtered run's
images.

An open run's helper carries MCP routes only. A provider API key still needs `--egress filtered`,
where the gateway holds the agent to its route. The helper uses exact routes for streamable HTTP and
the same-host route described above for legacy SSE. The servers below keep today's behaviour, with
the secret in the box, and the launch names each one:

- a server whose secret is in two places or mixes text with two references;
- a URL that isn't plain `https` on port 443;
- every server, when the box has no network to reach a helper on (Apple's `container` runtime,
  `--network none` or `--network container:…` in your runtime arguments).

### Read-only sessions

A read-only remote session brokers the same way. Its box loads nothing from the project: no MCP
file, no hooks, no skills. But the servers the daemon projects for it are your own. So its child
starts the same helper beside the box, and the list the session sends its adapter names the helper
and a stand-in for every server co:op can broker. A server it can't broker (the shapes listed above)
is sent as it always was. The box itself carries none of those variables either way: it loads no MCP
file, so nothing in it could read them.

A read-only run you start yourself (`coop <agent> --readonly`) is unchanged. No session asks it for
that list, so it loads no MCP at all. A bare session has none.

## Project services

A `service:` grant is a request like any other: a human approves it, and it names one service. For a
filtered run, co:op recreates that service and its dependencies on a project-owned internal network
before starting them. Those services can talk to each other directly, but the network has no direct
internet route.

Standard HTTPS proxy variables point the services at the existing co:op guard. The guard accepts
only approved TLS names and verifies the real ClientHello name before forwarding. Removing the proxy
variables removes connectivity. It doesn't restore direct internet access.

Allowed and blocked external requests appear in `coop net watch`, `inspect`, `blocked` and the JSON
views, with the name of the Compose service they came from. Direct traffic between services stays on
the internal network and isn't reported as external traffic.

In filtered mode, `box.network: true` alone is refused: name the sidecar you need. A filtered box
that uses project services waits until no other box in this project is running, so a second one
waits for the first to stop. Then it starts its services. A loop run and an editor session each get
their own Compose project, network and volumes. A run you start directly uses the project's
development stack.

The approval covers the definition a human reviewed, along with the service's name. `coop approve`
records a digest of that service's Compose stanza, and a launch recomputes it from the file it's
about to run. Rewriting `db:` into something else (a proxy image with ordinary egress, say) is a
pending change like any other. A launch refuses with
`the Compose service "db" changed since it was approved` and the review command. `coop net` shows
it under `⚠ New runs need your approval` as
`The Compose service "db" changed after its network access was approved.` Editing an unrelated
service changes nothing.

## TLS ports and localhost

TLS isn't only port 443. A `tls` rule names the ports it wants (`[443]`, `[853]`, `[443, 8443]`),
and the gateway captures exactly that set. The port an upstream is dialed on comes from the kernel's
own record of the redirect (`SO_ORIGINAL_DST`), never from the client. The same name on a port the
rule doesn't name is refused, with its port in the event. A connection made straight to the guard's
listener declares no port at all, so it's refused and counted too. Port 53 stays the gateway's own,
and a `serve` port may not collide with a captured one.

Inside the box, localhost is yours. A test server on `127.0.0.1:3000` and a `curl` to it never touch
the boundary, because the box's loopback is its own namespace, not a host surface. Only what leaves
the box meets the gateway.

## Your project's image

A repo whose `.agent/Dockerfile` starts with `ARG COOP_BASE_IMAGE` / `FROM ${COOP_BASE_IMAGE}` runs
its own image in filtered mode too, on co:op's clients. You build it explicitly on the host with
`coop build --egress filtered`. That build uses co:op's locked client image as the base, under its
own tag, `coop-<repo>-filtered:<hash>`, where the hash is the first 16 hex characters of a sha256
of the client image reference. Plain `coop build` chooses this image when filtered
is the project's effective network mode.

Launches only reuse an image you built explicitly. A launch stops with instructions to review and
build again when the sanitized build-context inputs changed (copied scripts and file modes
included), when the clients changed, or when the image is missing. An old automatic-build cache
record isn't approval, so an existing project needs one explicit filtered build.

Two proofs stand between that image and the box. Neither reads the Dockerfile, because a `FROM` line
is a claim about what was built, not evidence:

1. The locked image's layers must be the first layers of the built image, so the box is that exact
   image plus your own.
2. Every pinned client entry point must be byte for byte the file in the locked image. That covers
   each launcher, what it execs, and the native binaries it needs. They're read out of a container
   that is created and never started, so nothing from the image runs to describe itself.

Adding tools passes. Replacing, wrapping or deleting a pinned client is refused by name, and so is
an image that wasn't built on the locked one. The host qualification keeps naming co:op's own image.
Your image inherits nothing from it except through those two proofs, which run again on every
launch. `COOP_IMAGE` stays refused, because an arbitrary image has no such proof to offer.

Copying those entry points out of an image takes a few seconds, so co:op records what each read
found, outside every agent mount and next to your approvals. Host setup records what the locked
image holds. A launch records what it read out of the image your Dockerfile built. Both records are
keyed by the image ID, which is a content address. A rebuilt image gets a new ID and is read again.
A record that is missing, damaged or for another image is skipped, never trusted. The comparison
itself still runs on every launch.

The base ends as the non-root box user, so a package install needs `USER root` … `USER node` around
it. An explicit project build runs repository instructions with ordinary Docker build networking.
`--egress` chooses the image to prepare. It isn't a build-time firewall. Review the Dockerfile and
the files it copies before building. co:op stages a copy without shadowed secrets or Git metadata,
proves the resulting image, and saves approval for the exact staged inputs outside agent mounts. A
failed approval write is a failed command, even if Docker finished building the image.

Filtered launches never run those build instructions themselves. An open-network editor connection
may build a missing project image. Ordinary runs warn about stale inputs and require an explicit
`coop build --egress open` to rebuild. Offline launches refuse automatic project builds. Automatic
preparation of co:op's own embedded base, gateway and client images is separate, and it runs no
repository instructions.

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
| a project image that changes a pinned client | `this project's .agent/Dockerfile changes claude's cli client at /opt/coop/bin/claude — a filtered box runs the clients this host's setup qualified …` |
| `COOP_IMAGE` | `a filtered box runs coop's own image — unset COOP_IMAGE to start one` |
| a runtime other than Docker | `restricted networking needs docker; <runtime> cannot serve the qualified gateway — run this with --egress open or none, or set COOP_RUNTIME=docker` |
| `box.network: true` with no `service:` grant | `a filtered box does not join the shared services network — ask for the one sidecar you need with a to: {service: <name>} rule …` |
| `-v /var/run:/x` (or any mount of `/run`, `/proc`, `/sys`, `/dev`, `/`, or the Docker socket's directory) | `a filtered box cannot mount …: it is or holds …, which reaches Docker or the kernel` |
| a project file with a rule nobody approved, or a changed rule or mode | `<Agent> cannot start because this project asks for network access that has not been approved` · `Review it: coop approve` |
| `open`, in a project with no approval yet | `<Agent> cannot start because this project asks for unrestricted internet access, which has not been approved` · `Review it: coop approve` |
| an approved Compose service whose definition changed | `<Agent> cannot start because the Compose service "db" changed since it was approved` · `Review it: coop approve` |
| an approved project directory replaced by another at the same path | `<Agent> cannot start because the project directory at <path> was replaced since it was approved` · `Review it: coop approve` |

A refused rule fails the launch itself, before any approval is written or any container is created.
co:op never accepts a rule it can't enforce and then quietly drops the constraint.

### Ranges no rule can reach

Some destinations can never be granted, inside a `cidr:` grant or anywhere else:

- the host's own interface addresses;
- every subnet the container runtime allocates and every gateway it holds in one, so sibling
  containers, other sessions' boxes and the daemon's own address are out of reach;
- the loopback, link-local and metadata ranges.

co:op takes this inventory from the daemon at launch, and it follows the host while the box runs.
When another filtered box starts and creates its network, or a VPN brings an interface up, the new
subnets, gateways and host addresses join the running box's protected set in place. That is one
atomic kernel update, and they're refused from that moment. A box is stopped for topology only when
that update is refused or the envelope would pass 256 ranges. An address that disappears keeps its
denial.

A permitted CIDR doesn't beat the protected set: those packets are dropped and counted as protected.
The single exception is an approved `service:` grant. A human approved that one container address,
so it's permitted before the protected drop, and nothing else in its subnet is.

### Serve ports and mounts

A published `serve` port is host ingress, and only host ingress. The rule matches the bridge gateway
address the host's traffic is NAT'd from, so a sibling container on the same bridge can't connect to
it.

The gateway is the boundary for packets, so a filtered box also refuses the mounts that would go
around it. A bind of the runtime's control surface (`/var/run`, `/run`, `/proc`, `/sys`, `/dev`,
`/`, or the Docker endpoint's own socket directory) is refused by name. One `curl` over a daemon
socket would start a container the gateway never sees.

## Observed versus counted

`coop net inspect [<run>]` shows a run, or this project's newest run when you don't name one. It
leads with the traffic: the allowed aggregate, then every destination the workload reached. A
destination shows its hostname and port, its transport, and each resolved peer with its connection
count and bytes sent and received. After that comes only what went wrong: blocked attempts, alerts,
gaps in the evidence, a layer that didn't run normally, a cleanup still owed. A clean run prints
nothing else. `--json` keeps every field.

An interactive filtered box prints that same view itself, on stderr, once its teardown has sealed
the receipt. It appears under `Networking stats:`, with each destination's own totals on its row. It
has no tool prefix, even though it follows the agent's own output. Before it, one line says why the
box stopped: `The Coop box has stopped — main process exited with status 0`, or
`— interrupted by Ctrl-C` when a signal reached `coop`. co:op never guesses an interruption from an
exit status. The sections before the agent started carry no prefix at all: `Protecting secrets`,
`Configuring network access` listing each provider's endpoints, `Applied N approved network rules`
and the configured MCP servers before `✓ Everything else blocked`, then `Starting <agent>`.

- TLS flows are observed. The gateway sees each connection, so a destination row is the name the
  workload asked for, the address it resolved to, and the bytes that crossed. A blocked name becomes
  a retained event. `coop net blocked <host>` finds the newest run in this project that blocked that
  host. It groups that run's refusals of the host by cause and counts the repeats.
  `--run <run>` pins one run, and with `--run` an exact event ID replaces the host.
- Raw transports are counted. Every address grant has its own kernel counter, reported per grant as
  packets and bytes (`Raw traffic` in `coop net inspect`, `address_grants` in `--json`). No host
  inside a CIDR is recorded, so none is shown.
- Raw refusals are counted, not attributed. The packet filter drops a refused datagram without
  recording where it was going. So refused packets are a number
  (`N raw packets were blocked with no remote address recorded`), and there is nothing for
  `coop net blocked` to open. co:op won't invent a destination for them.
- A metric nobody measured is reported as UNKNOWN, never as zero. A group with one unmeasured member
  is UNKNOWN too, instead of a total that quietly counted it as zero.
- Only proven workload traffic is `Allowed`. co:op's own resolver connection and an ownerless
  closing kernel socket stay in `--json`. A socket that genuinely couldn't be attributed is listed
  under `Other observed endpoints` with the reason, never as allowed traffic.

## Runs, checks and exports

You name a run by any unique prefix of its id. The eight characters `coop net runs` shows are enough
for every run command. An ambiguous prefix is refused, with the prefixes that would settle it.
`coop net runs` shows this project's 25 newest runs, with a count and `--all` only when there are
more. `--all-projects` shows every project's runs, labeled by name and path.

`coop net check <url-or-host>` says whether a normal new run in this project can reach a
destination, with one verdict and one cause. It answers from the approved project rules and the
provider access each agent brings. `--run <run>` asks the same of the rules that recorded run
started with. Nothing is sent either way. An address has no implied transport, so it takes
`--protocol tcp|udp --port <n>` (or `--icmp`) and a run, because a run's protected ranges are part
of the answer.

`coop net export <run>` writes the sealed record as JSON with destination names and addresses
withheld, because even a blocked name can carry a secret. `--include-addresses` puts them back on
your own machine. Its `digest` is a content checksum of that projection. It isn't a signature or a
claim that partial evidence is complete.

## Asking for access

Put the rule in `.agent/project.yaml` under `box.egress_rules` and ask a human to run `coop approve`
on the host. The repository file is only a request. The approval is remembered outside the
repository, so editing or deleting the file can't widen access, and nothing inside a box can approve
itself. Approvals apply to new runs. A box that's already running keeps the policy it launched with.

### Reviewing a request

The approval is the exact snapshot of the file: its mode, its rules and the reviewed definition of
every `service:` it names. `coop approve` has no flags. To change access, edit the file and review
it again. `coop approve` applies the same capability gate a launch does, so a rule this runtime
could never enforce (an IPv6 destination, say) is refused at review, instead of being remembered and
refused at every launch.

The review is one diff against what's already approved. Unchanged rows are marked
`already approved`, additions `new request` and removals `no longer requested`. A mode change is
explained in plain words. A request for `open` gets a red warning, because nothing would be blocked.
You confirm at `Approve this access for new runs? [y/N]`.

If the file is already exactly what was approved, `coop approve` prints
`No approval needed — this project's requested access has not changed.` and grants nothing new.
Provider endpoints an agent brings with it aren't part of this diff. co:op grants those itself, and
they're never shown as project access.

### Pending requests

While the file and the approval differ, the request is pending:

- `coop init` ends with `⚠ New runs need your approval`, the reason, and
  `Review changes: coop approve`.
- Bare `coop net` shows the same diff under the same `⚠ New runs need your approval` line.
- Every launch refuses before any box or main process starts. It says it cannot start and names the
  cause, followed by `Review it: coop approve`.
  That covers `coop run`, a named agent, `coop acp`, `coop loop`, an interactive fork, fork ACP, and
  a fork review or merge gate.

A project with nothing pending sees none of these lines.

### Access for one run

For a single invocation, you can pass `--allow-domain <name>` (exact TLS 443) or
`--egress-rules <file>` with a full rule document. Where the file lives decides its authority: a
file inside an agent mount is a request, not a grant.

### The approval follows the directory

An approval is bound to the project directory, not to its path. If you move the approved checkout
aside and put another one there, the next launch is refused until a human reviews it again.
`coop net` reports that as blocked and names the command that fixes it.

The binding is a hidden `0600` marker hard-linked to owner-private co:op state, so the checkout and
`$XDG_STATE_HOME` (or `~/.local/state`) must be on the same hardlink-capable filesystem. The marker
contains identifiers, not credentials. A manual `git clean -x` removes ignored files, this marker
included. Run `coop approve` again to review and restore access, instead of copying marker bytes.

Moving the checkout together with its intact marker is recoverable. `coop approve` verifies the old
marker against co:op's private link, retires that pair, and asks you to approve the new canonical
path. A copied marker isn't a move, and it can't retire or inherit the original checkout's access.

If the marker itself is malformed, inspect it with `ls -l -- ./.coop-network-approval`. After you
verify that it's stale, remove only that checkout-side name with `rm -- ./.coop-network-approval`,
then run `coop approve` again. co:op prints these same guarded recovery commands with the refusal.

Because the marker is visible to the box, final runtime arguments may mount the exact project, but
not a path inside it that an existing agent could swap before Docker resolves the bind, or a
writable ancestor that could replace the project. They may never expose co:op's private state, even
read-only. That includes prospective first-run network, fork, execution-record and launch-lock
paths. Existing named volumes are inspected. `--volumes-from` and custom volume-driver options are
refused while anchored authority is active. Put a cache outside the checkout and mount that
explicit, inspectable path instead.

### Forgetting an approval

`coop net forget` takes an approval back. It shows what the project remembered, asks on the
terminal, and removes exactly that one record. Run it in the project, or pass `--project <path>` for
a checkout that's already gone. The runs recorded here, their receipts and this host's setup stay,
and the next filtered run asks for approval again.

A path that was a symlink was approved as the directory it pointed at. Once both are gone, the
record can no longer be found from that path, and `coop net forget` says so instead of removing
another project's.

## When a run is interrupted

A filtered run's gateway containers, volumes and receipt are owned by the `coop` process that
started it. If that process is killed (`SIGKILL`, a crash, a reboot), the run stays
`cleanup pending` until co:op settles it. co:op does that on its own:

- `coop net inspect` of that run makes one bounded recovery attempt before it says anything about
  cleanup.
- Every loop, fork or build start sweeps pending runs.
- So does every filtered launch, before its own box starts.

A successful recovery needs nothing from you. Only something external is reported: Docker stopped, a
different daemon at the recorded endpoint, a removal that failed. `coop net inspect` shows it as
`⚠ Cleanup incomplete — <the blocker>` with what to do about it, and co:op retries on its own.

To settle runs now:

    coop net recover           # settle every pending run now
    coop net recover <run>     # settle one run now

When `coop net recover` can't finish a run, it prints `⚠ Cleanup incomplete for network run <run>`,
then the causes and the next step.

Recovery removes exactly the resources that run recorded, by id and ownership labels. It never
matches by name prefix and never prunes. It seals a final receipt marked `partial` with workload
`supervisor_lost`. A run whose supervisor is still alive is left to that process.
