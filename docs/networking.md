# Restricted networking: what is supported

`coop <agent> --egress filtered` runs the box behind a per-run gateway. The box can reach its
selected providers and the destinations you approved. Other connections are blocked.

Filtering works on the network itself, not through a proxy setting. The same network rules apply
to every tool in the box: the provider CLI, `curl`, an SDK or a subprocess. Changing proxy settings
cannot bypass these rules. The [credential broker](#provider-credentials-stay-outside-the-box)
handles API-key and subscription accounts behind the packet filter; it does not replace it.

`coop net` shows what a new run in this project can reach, and why. `coop net runs` lists
recorded runs. `coop net --help` lists the commands.

## Network modes

New projects use filtered networking. `coop init` sets `box.egress: filtered` in
`.agent/project.yaml`. You can also choose `offline` to disable networking (called
`--egress none` on the command line), or `open` to allow all network traffic.
Switching to `open` needs your approval on the host. Running `coop init` again leaves an
existing project file unchanged.

## Setting up a host

Before your first filtered session, co:op runs five checks to make sure Docker can enforce the
network rules. It shows you the results, then starts your session if all five pass. If Docker
isn't running or a check fails, co:op explains the problem and won't start the session.

Later runs skip these checks while the saved results are still valid. You still need to
[approve](#asking-for-access) what each project can access on the network.

The checks use co:op's pinned gateway and locked client images with the selected Docker daemon.
co:op builds these images if needed, tests them through the normal launch process, and saves the
results.

Run `coop net setup` to do these checks ahead of time. This is useful before an unattended run,
after upgrading Docker or co:op, or when troubleshooting network setup:

    Setting up restricted networking using Docker 29.4.0 on linux/arm64.
    The gateway and client images are already available and will be reused.

    Checking filtered access:
      ✓ approved TLS access to example.com works
      ✓ unapproved domains are blocked
      ✓ direct IP connections cannot bypass domain rules
      ✓ the cloud metadata address is blocked
      ✓ DNS does not resolve unapproved domains

    ✓ All 5 checks passed — this host is ready for filtered runs

If a check fails, co:op shows the error and the checks that passed before it. Later checks are
not reported as passed. The output ends with `✗ this host is not ready for filtered runs`
and `No setup was saved`. If co:op needs to build images first, it tells you before showing
Docker's build output.

### Fleet workers and Docker-in-Docker

Install a Docker CLI and matching Buildx plugin that work together through `docker build`.
Installing Buildx alone is not enough: Debian bookworm's Docker CLI 20.10 does not pass that
command to the plugin. Docker CLI 27.5.1 with Buildx 0.20.1 is a supported pairing for a
Docker 27.5.1 DinD sidecar.

Filtered runs need a Linux Docker daemon running as root on amd64 or arm64, accessed through a
local Unix socket. Rootless Docker and daemons with user-namespace remapping are not supported.
With DinD, mount the worker's state and temporary directories at the same absolute paths in the
daemon container. Docker looks for bind-mount sources in its own filesystem. Never mount the
Docker socket into the agent's container.

Run `coop net setup` as the worker's OS user, using the same Docker endpoint and network state
directory as the worker. Session API requests do not build images or run these setup checks.
If setup fails, the worker cannot accept filtered jobs; do not retry them with open networking.
Before accepting a job, co:op checks that the saved results still match the daemon, runtime
requirements and exact images in use.

## Supported today

| Rule | Meaning | Enforced by |
| --- | --- | --- |
| `to: {domain: "api.example.com"}` · `protocol: tls` · `ports: [443]` | that exact name over TLS 443 | the gateway checks the visible TLS server name (SNI) and routes to a validated IPv4 address |
| `to: {domain: "*.example.com"}` · `protocol: tls` · `ports: [443]` | one label under that name (`www.example.com`), but never the apex and never `a.b.example.com` | the same hostname check applies to DNS requests and the TLS server name (SNI) |
| `to: {domain: "dns.google"}` · `protocol: tls` · `ports: [853, 8443]` | that name over TLS on those ports, and on no other | the gateway redirects only those ports; the guard checks the original port reported by the kernel |
| `to: {ip: "10.42.8.12"}` / `to: {cidr: "10.42.9.0/24"}` · `protocol: tcp\|udp` · `ports: [...]` | raw TCP or UDP to those IPv4 addresses on those ports | an nftables rule allows the traffic and keeps its own packet and byte counts |
| `to: {ip: ...}` / `to: {cidr: ...}` · `protocol: icmp` · `types: [echo-request]` | IPv4 ping to those addresses | an nftables rule allows echo requests; connection tracking (conntrack) allows the replies |
| `to: {provider: claude}` | that provider's maintained core endpoints | co:op turns the trusted release's provider definitions into explicit TLS rules |
| `to: {service: "db"}` · `protocol: tcp` · `ports: [5432]` | one Compose sidecar belonging to this project | co:op reads the service container's exact IP address from the runtime at launch |
| `serve: {ports: [3000]}` in `.agent/project.yaml` | the host browser reaches the box's dev server | Docker publishes the port on the gateway; the box shares its network namespace |

In a filtered run, `COOP_RUN_ARGS` and extra runtime arguments accept bind mounts, `-e KEY=VALUE`
and `--label KEY=VALUE` only. Any other flag is refused by name. co:op's own tracking labels take
precedence over your labels with the same key.

## Provider access

Each agent gets access to the provider endpoints its client needs:

| Agent | Hosts a filtered run reaches |
| --- | --- |
| `coop claude` | `api.anthropic.com`, `platform.claude.com` (the OAuth refresh) and `mcp-proxy.anthropic.com` (the claude.ai connectors a login has on by default) |
| `coop codex` | `chatgpt.com` and `auth.openai.com` |
| `coop gemini` | adapter-declared Google model and Code Assist routes for the selected credential family |
| `coop grok` | `cli-chat-proxy.grok.com`, `code.grok.com` and `auth.x.ai` (the OAuth refresh) |

Portable Gemini OAuth credentials and AI Studio keys use the routes defined by the Gemini adapter.
Vertex credentials are not supported, and co:op refuses to start a run that uses them.

Provider rules do not include release feeds, package registries, update checks or telemetry
(`raw.githubusercontent.com`, `registry.npmjs.org`, `api.github.com` and the Datadog intakes).
co:op disables this traffic through each client's settings inside the box. Your host profiles
stay unchanged. Updates are also disabled in co:op's images, so clients cannot update themselves
even when their home folders are not mounted.

| Client | In the box | Update switch in co:op's image |
| --- | --- | --- |
| Claude | runs with `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` and `DISABLE_UPDATES=1` | `DISABLE_UPDATES` |
| Codex | reads a generated `config.toml` that sets `check_for_update_on_startup = false`, `analytics.enabled = false` and the `otel` exporters to `none`, on top of your own settings | `/etc/codex/managed_config.toml`, which also turns off Codex's background server and its updater |
| Gemini | gets `general.enableAutoUpdate`, `general.enableAutoUpdateNotification` and `privacy.usageStatisticsEnabled` off, plus `GEMINI_TELEMETRY_ENABLED=false` | `/etc/gemini-cli/settings.json` |
| Grok | runs with `GROK_TELEMETRY_ENABLED=false` and `GROK_DISABLE_AUTOUPDATER=1` | `GROK_DISABLE_AUTOUPDATER` |

A session that only answers a prompt should therefore produce no blocked requests from these
background features. If the agent or your project requests one of these hosts directly, co:op
records the blocked request as usual. You can inspect it and approve access; it is never hidden
from the report.

## Provider credentials stay outside the box

Both API keys and OAuth or subscription accounts use a provider broker for each Docker run,
whether networking is open or filtered. The broker accepts requests only for the selected
providers and accounts. Sign-in, credential renewal and account removal all use the same
host-managed credential store. The provider folders mounted into the project never contain
reusable credentials.

The host can refresh credentials during a long run without restarting the client. Removing or
replacing an account prevents the old run from continuing to use it.

Clients keep using their original provider URLs. A private loopback listener handles TLS for
the provider origins listed by the adapter. It checks each request's path, method and query,
then adds the selected account's credentials before forwarding it. Requests to undeclared
routes on those origins are blocked.

Only that run receives the broker's certificate authority; co:op never installs it on the host
or in a global trust store. Other origins follow the run's open or filtered network rules.
The broker still verifies the provider's TLS certificate.

A separate container with no Linux capabilities holds the short-lived credential snapshot.
The workload joins that container's network namespace after Coop inspects it, but cannot read the
private snapshot or CA signing key. Stopping the run removes the broker entrance. If the host stops
renewing or publishing snapshots, they expire and the broker refuses access. Filtered runs keep
their approved egress policy and traffic attribution; open runs still allow ordinary outbound traffic.

The lead and its teammates share one account per provider inside a box. Switching accounts
starts a new broker. You can include both API-key and subscription accounts in a fallback list.
Run teammates in separate boxes if they should not trust one another.

Supported API variables are `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` and `GEMINI_API_KEY`.
Alternate token variables, Vertex credentials and `XAI_API_KEY` are refused rather than exposed.
Plain native Gemini OAuth credentials are supported; an encrypted-only cache without an importable
grant is not. Custom provider base URLs are refused.

Offline runs receive no provider keys and do not start a broker. Online provider runs need
Docker with an ordinary bridge or a project services network. Host networking, arbitrary joins
to another container's network, and Apple's `container` runtime do not support this setup.
Native read-only/bare runs support only Claude. Remote restricted jobs cannot use filtered
networking.

## A client's own downloads

A client may need to download public files as well as call its model API. These downloads also
go through co:op, without using your account credentials. For example, Codex fetches its curated
plugin store from chatgpt.com and github.com whenever an API-key run starts.

The broker uses a `download` route for these requests. It sends no credentials and rejects any
that the box supplies. Only the exact requests listed by the adapter are allowed, including
their query strings. For Codex, these are:

| Host | Request lines |
| --- | --- |
| chatgpt.com | `GET /backend-api/plugins/featured?platform=codex` |
| github.com | `GET /openai/plugins.git/info/refs?service=git-upload-pack` and `POST /openai/plugins.git/git-upload-pack` |

The github.com lines fetch one public repository and never push. The same discovery path with
`service=git-receive-pack` isn't in the set.

The native broker serves those original URLs. It never turns the public Git download route into
general GitHub access, and never attaches the selected account's grant to it. Other GitHub requests
still require ordinary network approval in filtered mode.

## MCP servers

### Filtered runs

Bearer tokens for MCP servers stay outside filtered boxes. When a shared server uses
`bearer_token_env_var`, co:op adds a route for it to the run's broker, after the provider routes.
The MCP configuration inside the box points to that route's loopback listener and uses
`COOP_MCP_TOKEN_<n>` instead of your token. This stand-in works only for that server and run.
The broker adds the real token when it forwards the request.

The route allows only the server's exact URL path and the methods MCP uses: POST, GET and DELETE.
It waits for long tool calls to finish. The agent cannot bypass the broker by connecting to the
bearer server's host directly.

Your token variable never enters a filtered box, even if that box does not load MCP. A session's
ACP adapter receives the stand-in too. If the token is missing or supplied through `-e`, co:op
stops the launch and names the server. Tools supplied by a remote session's controller use the
same route restricted to the server's exact path.

### Legacy SSE servers

Legacy SSE servers need broader access because they choose a message endpoint during each
connection. co:op allows `GET` on the stream's path and `POST` on any path at the same host.
Other hosts and methods are blocked, and the broker still sends only one secret header.

The credential still stays outside the box, but a legacy SSE connection lets the box reach
more paths on that host than streamable HTTP does. co:op names these servers at launch and
explains what to ask their vendors for. Use a server's streamable HTTP URL when it offers one.

Some legacy servers return a full endpoint URL instead of a path. The client then posts directly
to that URL, bypassing co:op. Filtered networking blocks the request. Open networking sends only
the stand-in, so authentication fails without exposing the real token. To use this server,
switch to its streamable HTTP URL.

### Offline runs

An offline box cannot reach remote MCP servers, so co:op removes them from its MCP configuration.
The launch reports: "MCP servers that need internet are left out: …". Local command servers
remain available, and remote-server token variables stay outside the box. An offline remote
session's editor adapter also receives only the local servers.

### Open runs

With open networking, the box connects directly to the internet. co:op therefore uses a separate
helper container to keep supported MCP credentials outside the box.

If the MCP configuration includes a remote server with a secret, co:op starts a helper from
the gateway image (`coop-net broker`). It joins the box's network, and co:op adds a hosts entry
so the box can find it. The helper runs without privileges (`--cap-drop ALL`, uid 65532,
read-only root, 256 MB). It has the label `coop=broker` and the box's supervisor labels.

The helper exits when the `coop` process that started it closes its input, including when that
process crashes. If a helper is left behind, co:op removes it during box cleanup.

Inside the box, each brokered MCP server points to `http://coop-broker:<port>`. The port is
15580 plus the server's index. The box sends a stand-in token; the helper replaces it with the
real credential and connects only to that server's host. Your token variable stays outside
the box.

The box talks to the helper over plain HTTP on the container network. If the project has no
co:op-managed network, the helper uses Docker's default bridge, which is shared with other
containers that have no assigned network. On that bridge, the stand-in token protects access
to the credential. It is a random 256-bit value created for the run and accepted only by its
own listener for its server's route. co:op controls the helper's hosts entry and rejects any
`--add-host` argument that would replace it.

Remote open sessions use the helper for controller-provided tools too: the session's child
passes its adapter list to co:op. The helper image is built on first use, just like the
images for a first filtered run.

The MCP helper is separate from the provider broker used in open and filtered runs. It allows
exact paths for streamable HTTP and the same-host routes described above for legacy SSE.
The following servers still run with their secrets inside the box. co:op names each one
at launch:

- a server whose secret is in two places or mixes text with two references;
- a URL that isn't plain `https` on port 443;
- every server, when the box has no network to reach a helper on (Apple's `container` runtime,
  `--network none` or `--network container:…` in your runtime arguments).

## Project services

A `service:` rule requests access to one project service and needs your approval. Before a
filtered run starts, co:op recreates that service and its dependencies on an internal network
owned by the project. They can talk to each other, but have no direct internet connection.
With Docker Engine 26 or later, Docker's DNS server on this network resolves only service
names. The filtered box sends its own DNS requests only to co:op's guard.

The services use standard HTTPS proxy variables to reach co:op's guard. It forwards requests
only for approved TLS names, after checking the name in the actual ClientHello. Removing the
proxy variables disconnects the services from the internet; it does not bypass the guard.

Allowed and blocked external requests appear in `coop net watch`, `inspect`, `blocked` and the JSON
views, with the name of the Compose service they came from. Direct traffic between services stays on
the internal network and isn't reported as external traffic.

In filtered mode, you must name the service you need; `box.network: true` alone is rejected.
A filtered run that needs project services waits for every other box in the project to stop
before starting those services. Each loop run and editor session gets its own Compose project,
network and volumes. A run started directly uses the project's development stack.

Service approval covers both the name and the Compose definition you reviewed. `coop approve`
saves a digest of that service's configuration, and each launch compares it with the current
file. Changing `db:` to use a different image or network setup requires another approval.
Changing an unrelated service does not.

If the definition changed, the launch stops with
`the Compose service "db" changed since it was approved` and tells you how to review it.
`coop net` lists the change under `⚠ New runs need your approval` as
`The Compose service "db" changed after its network access was approved.`

## TLS ports and localhost

A `tls` rule can allow ports other than 443, such as `[443]`, `[853]` or `[443, 8443]`.
The gateway redirects only the listed ports. It reads the original destination port from the
kernel's `SO_ORIGINAL_DST` record, so the client cannot choose a different port after the redirect.

A request to an unlisted port is blocked and recorded with its port number. Connecting directly
to the guard's listener is also blocked and counted, because that connection has no original
destination port. Port 53 is reserved for the gateway, and a `serve` port cannot use one of
the captured ports.

You can run a test server on `127.0.0.1:3000` and use `curl` to reach it inside the box.
This traffic stays inside the box's network namespace and never reaches your host or the gateway.
Only traffic leaving the box passes through the gateway. The exception is Docker's DNS address,
`127.0.0.11`, which is blocked so the box uses only co:op's DNS guard.

## Your project's image

You can use your own project image in filtered mode if its `.agent/Dockerfile` starts with
`ARG COOP_BASE_IMAGE` / `FROM ${COOP_BASE_IMAGE}`. Build it on the host with
`coop build --egress filtered`. co:op uses its locked client image as the base and tags the
result `coop-<repo>-filtered:<hash>`. The hash is the first 16 hex characters of the sha256
of the client image reference. Plain `coop build` selects this image too when the project's
effective network mode is filtered.

A filtered launch uses only a project image you built explicitly. If the image is missing,
the clients changed, or the sanitized build inputs changed, co:op asks you to review and rebuild.
Build inputs include copied scripts and file modes. An image from the old automatic-build cache
does not count as approved; build it explicitly once for filtered use.

Use `.agent/Dockerfile.dockerignore` to exclude files that this image does not build from, or name
the ignore file after your configured Dockerfile. Its rules take precedence over root
`.dockerignore`. Excluded source changes do not require another build; the running box still sees
that source through its repository mount. For a toolchain-only image, exclude everything with
`**` and add `!path/to/input` for each file it copies. The Dockerfile and active ignore file always
remain part of the build fingerprint. Changes to them or to an included input require a rebuild.
Docker exceptions cannot restore secrets, Git metadata or generated output co:op omits.

Before starting the box, co:op checks the built image itself. The Dockerfile's `FROM` line
alone cannot establish what the image contains. Two checks must pass:

1. The locked image's layers must be the first layers of the built image, so the box is that exact
   image plus your own.
2. Every pinned client entry point must be byte for byte the file in the locked image. That covers
   each launcher, what it execs, and the native binaries it needs. They're read out of a container
   that is created and never started, so nothing from the image runs to describe itself.

You can add tools to the image, but cannot replace, wrap or delete a pinned client. co:op names
the affected client when it rejects the image. It also rejects images that do not use the
locked image as their base. The host setup checks cover co:op's own image; your project image
must pass both checks above on every launch. `COOP_IMAGE` is rejected because an arbitrary image
has not passed those checks.

Reading client entry points from an image takes a few seconds, so co:op caches the results beside
your approvals, outside every agent mount. Host setup records the locked image's files; a launch
records the project image's files. Each record is tied to the image's content-addressed ID.
A rebuilt image gets a new ID, so co:op reads its files again.

Missing, damaged or mismatched records are ignored. The comparison between the locked and
project images still runs on every launch.

The base image uses the non-root box user. To install packages, put `USER root` before the
installation commands and `USER node` after them. Review the Dockerfile and its copied files
before building: the build executes project instructions with ordinary Docker networking.
`--egress` selects the image to build; it does not filter traffic during the build.

co:op stages the build without shadowed secrets or Git metadata, checks the resulting image,
and saves approval for those exact inputs outside the agent's mounts. If saving approval fails,
the command fails even if Docker finished the build.

Filtered launches never build the project image automatically. An editor connection with open
networking may build a missing image. Ordinary runs warn when inputs have changed and require
`coop build --egress open` to rebuild. Offline launches do not build project images automatically.
co:op may still prepare its own embedded base, gateway and client images; those builds do not
execute instructions from your repository.

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

If co:op cannot enforce a rule, it stops the launch before saving approval or creating a
container. It never starts a run with a requested restriction silently removed.

### Ranges no rule can reach

Some destinations can never be granted, inside a `cidr:` grant or anywhere else:

- the host's own interface addresses;
- every subnet the container runtime allocates and every gateway it holds in one, so sibling
  containers, other sessions' boxes and the daemon's own address are out of reach;
- the loopback, link-local and metadata ranges.

co:op reads this inventory from the daemon at launch and updates it while the box runs. When
another filtered box creates a network, or a VPN brings an interface up, co:op adds the new subnets,
gateways and host addresses to the running box's protected set in one atomic kernel update.
Those addresses are blocked as soon as the update takes effect. A network change stops the box
only if the update is refused or the protected set would exceed 256 ranges. An address remains
blocked after it disappears from the host.

An approved CIDR cannot override these protected ranges. Packets to protected addresses are
still dropped and counted. The only exception is an approved `service:` rule: it allows the
exact container address you approved, without allowing the rest of that subnet.

### Serve ports and mounts

A published `serve` port accepts connections only from the host. The rule matches the bridge
gateway address that Docker uses for traffic from the host. Other containers on the same
bridge cannot connect through it.

A Docker socket or kernel control directory could let the agent bypass the gateway, so filtered
runs reject mounts of `/var/run`, `/run`, `/proc`, `/sys`, `/dev`, `/` and the Docker endpoint's
socket directory. The error names the blocked mount. For example, one `curl` request to a Docker
socket could start another container outside the filtered network.

## Observed versus counted

`coop net inspect [<run>]` shows network activity for a run. If you omit the run, it shows this
project's newest one. The report starts with totals for allowed traffic, then lists every
destination the workload reached. Each row shows the hostname, port and transport, followed by
each resolved IP address with its connection count and bytes sent and received.

The rest of the report lists problems: blocked attempts, alerts, missing evidence, parts of the
network setup that did not run normally, and unfinished cleanup. These sections are omitted for
a clean run. `--json` includes every field.

At the end of an interactive filtered run, co:op finalizes the network record and prints the
same report on stderr. It appears under `Networking stats:`, with each destination's
totals on its own row. The report follows the agent's output without a tool prefix. First,
co:op says why the box stopped: `The Coop box has stopped — main process exited with status 0`,
or `— interrupted by Ctrl-C` when a signal reached `coop`. It does not infer an interruption
from the exit status alone.

The sections shown before the agent starts also have no prefix: `Protecting secrets`,
`Configuring network access` with each provider's endpoints, `Applied N approved network rules`
and the configured MCP servers, then `✓ Everything else blocked` and `Starting <agent>`.

- For TLS traffic, the gateway records each connection's requested name, resolved address and
  transferred bytes. It also saves an event when it blocks a name. `coop net blocked <host>`
  finds this project's newest run that blocked that host, groups its blocked attempts by cause
  and counts repeats. Use `--run <run>` to choose a run. With `--run`, you can supply an exact
  event ID instead of a host.
- For raw traffic, the kernel counts packets and bytes for each approved address rule. These
  appear under `Raw traffic` in `coop net inspect` and as `address_grants` in `--json`. Individual
  hosts within a CIDR are not recorded or shown.
- For blocked raw packets, the filter counts how many it drops without recording their
  destinations. The report therefore shows a count
  (`N raw packets were blocked with no remote address recorded`). There is no destination
  record for `coop net blocked` to open.
- An unmeasured metric appears as UNKNOWN, never zero. A total is also UNKNOWN if any part of
  it was not measured.
- Only traffic co:op can identify as coming from the workload appears as `Allowed`. co:op's
  own resolver connection and a closing kernel socket with no owner appear only in `--json`.
  Other connections that co:op cannot attribute appear under `Other observed endpoints` with
  the reason. It does not count them as allowed traffic.

## Runs, checks and exports

Use any unique prefix of a run's ID to select it. The eight characters shown by `coop net runs`
work with every run command. If a prefix matches more than one run, co:op refuses it and shows
the longer prefixes you can use.

`coop net runs` shows this project's 25 newest runs. When there are more, it shows a count and
the `--all` option. Use `--all-projects` to see runs from every project, labeled by name and path.
Coop keeps a finished run's record for 90 days after it ends. If cleanup is still pending, it
keeps the record until cleanup finishes.

Each run also keeps a copy of the rules it started with; the `--run` option below reads that
copy. Coop removes the saved copy of a launch's rules only when no retained run record refers
to it and no launch has used it for 90 days. It keeps the copy a worker session was created
with for the whole session.

Coop keeps rule copies made before this version because it cannot tell which sessions still
use them. The exception is a copy made by `coop net setup`: Coop marks these copies as setup
checks, so it knows no session uses them. It removes a setup copy after removing that check's
run record. An editor session that starts no box for 90 days may need to be started again.

`coop net check <url-or-host>` tells you whether the rules would allow a normal new run in this
project to reach a destination. It gives one answer and one reason, based on the approved project
rules and each agent's provider access. Use `--run <run>` to check the rules a recorded run
started with instead. Neither check sends network traffic.

To check an IP address, also specify `--protocol tcp|udp --port <n>` (or `--icmp`) and a run.
An address alone does not specify a transport, and the answer must account for that run's
protected ranges.

`coop net export <run>` exports the finalized run record as JSON. It leaves out destination
names and addresses by default, because even a blocked name can contain a secret. Use
`--include-addresses` to include them in the export on your machine. The `digest` is a checksum
of the exported content. It is not a signature and does not make a partial record complete.

## Asking for access

Add the rule to `.agent/project.yaml` under `box.egress_rules`, then ask a human to run
`coop approve` on the host. Editing the repository file requests access; it does not grant it.
Coop stores approval outside the repository, so editing or deleting the file cannot widen
access. Nothing inside the box can approve its own request. New runs use the approved rules;
a box that is already running keeps the rules it started with.

A running editor session (`coop acp`) can pick up a new approval without reconnecting. Once
no prompt is running, co:op starts a box with the new rules, then stops the old box. Any
processes left in the old box stop too, and the editor shows a line explaining this. A box
started for another reason, such as a crash or provider switch, also gets the new rules.

If co:op cannot apply the rules, or the new box stops before the switch, the session keeps its
previous rules and explains why. After `coop net forget`, a running session keeps its rules
until you reconnect the editor.

### Reviewing a request

Approval covers the file's exact network mode, rules and the reviewed definition of every
`service:` it names. `coop approve` has no flags. To change access, edit the file and review
it again. Before saving approval, `coop approve` checks that the runtime can enforce the rules,
just as it does at launch. It refuses unsupported rules, such as an IPv6 destination, during
review so they cannot be approved and then fail at every launch.

The review compares the request with the access already approved. Unchanged rules have no
marker; additions use `+` and removals use `-`. The `NETWORK RULES` heading explains these
markers. Coop explains any network mode change in plain words. A request for `open` gets a
red warning because it would allow unrestricted traffic. To confirm, answer
`Approve this access for new runs and running editor sessions? [y/N]`.

If the file is already exactly what was approved, `coop approve` prints
`No approval needed: this project's requested access has not changed.` and grants nothing new.
Provider endpoints are separate from this comparison. co:op allows the endpoints each agent
needs, so they are not shown as access requested by the project.

### Pending requests

If the project's request differs from your saved approval, co:op treats it as pending:

- `coop init` ends with `⚠ New runs need your approval`, the reason, and
  `Review changes: coop approve`.
- Running `coop net` without a subcommand shows the same comparison under
  `⚠ New runs need your approval`.
- Every launch stops before starting a box or its main process. It explains why and prints
  `Review it: coop approve`. This applies to `coop run`, a named agent, `coop acp`, `coop loop`,
  an interactive fork, fork ACP, and a fork review or merge gate.

These messages do not appear when the project has no pending request.

### Access for one run

For one run, use `--allow-domain <name>` to allow an exact name over TLS on port 443, or pass
`--egress-rules <file>` with a full rule document. A rules file inside a folder mounted into
the agent's box only requests access. Its location prevents the agent from using it to
approve its own rules.

### The approval follows the directory

Approval belongs to the project directory itself. If you move an approved checkout aside and
put another checkout at the same path, co:op refuses the next launch until a human reviews it
again. `coop net` reports that access is blocked and names the command to review it.

Coop identifies the directory with a hidden file, called a marker, with `0600` permissions.
It is hard-linked to a file in co:op's private state. The checkout and `$XDG_STATE_HOME`
(or `~/.local/state`) must therefore be on the same filesystem, and that filesystem must
support hard links. The marker contains identifiers, not credentials.

Running `git clean -x` removes ignored files, including this marker. Run `coop approve` again
to review and restore access. Copying the marker's contents does not restore its hard link.

If you move a checkout with its marker intact, `coop approve` can recover it. It verifies the
marker against co:op's private hard link, retires the old marker and private link, then asks
you to approve the checkout's new canonical path. A copied marker cannot inherit the
original checkout's access or retire its approval.

If a marker is malformed, inspect it with `ls -l -- ./.coop-network-approval`. Only after
confirming it is stale, remove the name inside that checkout with
`rm -- ./.coop-network-approval`, then run `coop approve` again. co:op prints these same
recovery commands and precautions when it refuses the launch.

The box can see the marker, so co:op also checks the paths in runtime mount arguments.
You may mount the exact project directory. You may not mount a
path inside it that an existing agent could swap before Docker opens the mount, or a writable
parent directory that would let the agent replace the project.

Mounts must never expose co:op's private state, even read-only. This includes the paths it
will use for network state, forks, execution records and launch locks on a first run, before
those paths exist. co:op also inspects existing named volumes. When it uses the marker to
verify approval, it refuses `--volumes-from` and custom volume-driver options. To mount a cache,
keep it outside the checkout and name its path explicitly so co:op can inspect it.

### Forgetting an approval

`coop net forget` removes a project's approval. It shows the saved access, asks you to confirm
in the terminal, then removes only that approval record. Run it in the project, or pass
`--project <path>` if the checkout is already gone. Run records, their receipts and this host's
setup results stay. The next filtered run needs approval again.

If the approved path was a symlink, the approval belongs to the directory it pointed to.
Once both the symlink and directory are gone, co:op cannot find the approval from that path.
`coop net forget` explains this and leaves other projects' approvals alone.

## When a run is interrupted

The `coop` process that starts a filtered run manages its gateway containers, volumes and
network record. If the process dies because of `SIGKILL`, a crash or a reboot, the run stays
`cleanup pending` until co:op recovers it. If the process died after cleanup but before saving
the final record, the status is `final record missing`. co:op attempts recovery automatically:

- `coop net inspect` makes one recovery attempt for that run, with a time limit, before
  reporting its cleanup status.
- Starting a loop, fork or build checks for runs waiting for cleanup and attempts recovery.
- Every filtered launch does the same before starting its own box.

If recovery succeeds, you need to do nothing. If it cannot finish, co:op reports what prevented
it, such as Docker being stopped, a different daemon at the saved endpoint, or a resource it
could not remove. `coop net inspect` shows `⚠ Cleanup incomplete — <the blocker>` and what to
do next. co:op retries automatically.

To recover runs now:

    coop net recover           # settle every pending run now
    coop net recover <run>     # settle one run now

When `coop net recover` cannot finish a run, it prints
`⚠ Cleanup incomplete for network run <run>`, followed by the causes and what to do next. An
unreadable run record does not stop recovery of the others. co:op attempts recovery for every
run it can read, names any records it could not read, and exits 1.

Recovery removes only that run's resources, using their exact names and the run's labels.
It never matches a name prefix or runs a general prune. It saves a final receipt marked
`partial`, with workload `supervisor_lost`. If the process supervising the run is still alive,
co:op leaves recovery to that process.
