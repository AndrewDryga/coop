# Restricted networking: what is supported

Boxes run behind a per-run gateway by default. The box can reach its selected providers and the
destinations you approved. Other connections are blocked.

Filtering works on the network itself, so changing proxy settings can't bypass the rules. The same
rules apply to every tool in the box: the provider CLI, `curl`, an SDK or a subprocess. The
[credential broker](#provider-credentials-stay-outside-the-box) handles API-key and subscription
accounts. It works behind the packet filter and doesn't replace it.

`coop net` shows what a new run in this project can reach, and why. `coop net runs` lists recorded
runs, and `coop net --help` lists the commands.

## Allow one website

Add a rule for the site under `box.egress_rules` in the project's `.agent/project.yaml`:

    box:
      egress: filtered
      egress_rules:
        - to: {domain: "api.github.com"}
          protocol: tls
          ports: [443]

Then run `coop approve` on the host and answer `y`:

    NETWORK RULES (+ new)
      + api.github.com:443 · TLS
    Approve this access for new runs and running editor sessions? [y/N] y
    ✓ Approved for new runs and running editor sessions

New runs can reach that exact name over HTTPS, but not its subdomains or other ports. Everything
else stays blocked:

    $ coop run -- curl -sS -o /dev/null -w '%{http_code}\n' https://api.github.com/
    200
    $ coop run -- curl -sS -o /dev/null -w '%{http_code}\n' https://example.com/
    curl: (6) Could not resolve host: example.com
    000

`coop net check <url>` answers the same question without starting a box. To allow a site for a
single run, see [access for one run](#access-for-one-run). [Asking for access](#asking-for-access)
explains how approval works.

## Network modes

Every box uses filtered networking unless you or the project choose another mode, even outside a
co:op project. An agent reaches its provider and the destinations you approved. A
plain `coop run` or `coop shell` reaches only the destinations you approved. `coop init` writes
`box.egress: filtered` into new projects and leaves an existing project file unchanged.

There are two other modes: `open` allows all network traffic, and `offline` turns networking off.
The command line, `COOP_EGRESS` and the project file use the same three words. Where you set the
mode decides what it covers:

- For a single run, use `--egress open` or `--egress offline`. The project's network rules don't
  apply to that run, approved or not, and its `Configuring network access` section says
  `This project's network rules do not apply to this run.`
- For your runs, set `COOP_EGRESS`. `COOP_EGRESS=offline` always keeps a run offline unless you
  pass `--egress`. For `filtered` and `open`, a project's saved approval outranks `COOP_EGRESS`,
  and the run shows a warning.
- For one project, set `box.egress`. A project's request for `open` needs your approval on the
  host.

When the mode comes from the default, the run's `Configuring network access` section starts with
`Filtered by default. Use --egress open for unrestricted access.`

Filtered networking needs Docker. On another runtime, such as Apple's `container`, co:op refuses a
run with no mode set. Run it with `--egress open` or `--egress offline`, set `COOP_EGRESS`, or
switch to Docker with `COOP_RUNTIME=docker`. co:op never switches to open networking on its own.

## Setting up a host

Before your first filtered session, co:op runs five checks to make sure Docker can enforce the
network rules. It shows you the results, then starts your session if all five pass. If Docker
isn't running or a check fails, co:op explains the problem and won't start the session.

Later runs skip these checks while the saved results are still valid. You still need to
[approve](#asking-for-access) what each project can access on the network.

The checks use co:op's pinned gateway and locked client images with the selected Docker daemon, and
co:op builds those images if needed.

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

    ✓ All 5 checks passed. This host is ready for filtered runs

If a check fails, co:op shows the error and the checks that passed before it. The output ends with
`✗ this host is not ready for filtered runs` and `No setup was saved`.

### Fleet workers and Docker-in-Docker

Install a Docker CLI and a matching Buildx plugin that work together through `docker build`.
Buildx alone isn't enough: Debian bookworm's Docker CLI 20.10 doesn't pass that command to the
plugin. Docker CLI 27.5.1 with Buildx 0.20.1 is a supported pairing for a Docker-in-Docker (DinD)
sidecar running Docker 27.5.1.

Filtered runs need a Linux Docker daemon running as root on amd64 or arm64, accessed through a
local Unix socket. Rootless Docker and daemons with user-namespace remapping aren't supported.
With DinD, mount the worker's state and temporary directories at the same absolute paths in the
daemon container. Never mount the Docker socket into the agent's container.

Run `coop net setup` as the worker's OS user, using the same Docker endpoint and network state
directory as the worker. Session API requests don't build images or run these setup checks. If
setup fails, the worker can't accept filtered jobs. Don't retry them with open networking. Before
accepting a job, co:op checks that the saved results still match the daemon, runtime requirements
and exact images in use.

## Supported today

| Rule | Meaning | Enforced by |
| --- | --- | --- |
| `to: {domain: "api.example.com"}` · `protocol: tls` · `ports: [443]` | that exact name over TLS on port 443 | the gateway checks the visible SNI and routes to a validated IPv4 address |
| `to: {domain: "*.example.com"}` · `protocol: tls` · `ports: [443]` | one label under that name (`www.example.com`), but never the bare domain and never `a.b.example.com` | the same hostname check applies to DNS requests and the SNI |
| `to: {domain: "dns.google"}` · `protocol: tls` · `ports: [853, 8443]` | that name over TLS on those ports, and on no other | the gateway redirects only those ports, then checks the original port that the kernel reports |
| `to: {ip: "10.42.8.12"}` / `to: {cidr: "10.42.9.0/24"}` · `protocol: tcp\|udp` · `ports: [...]` | raw TCP or UDP to those IPv4 addresses on those ports | an nftables rule allows the traffic and keeps its own packet and byte counts |
| `to: {ip: ...}` / `to: {cidr: ...}` · `protocol: icmp` · `types: [echo-request]` | IPv4 ping to those addresses | an nftables rule allows echo requests, and conntrack allows the replies |
| `to: {provider: claude}` | the core endpoints that co:op maintains for that provider | co:op turns the provider definitions built into its release into explicit TLS rules |
| `to: {service: "db"}` · `protocol: tcp` · `ports: [5432]` | one Compose sidecar that belongs to this project | co:op reads the service container's exact IP address from the runtime at launch |
| `serve: {ports: [3000]}` in `.agent/project.yaml` | the host browser reaches the box's dev server | Docker publishes the port on the gateway, and the box shares the gateway's network namespace |

In a filtered run, `COOP_RUN_ARGS` and extra runtime arguments accept bind mounts, `-e KEY=VALUE`
and `--label KEY=VALUE` only. co:op refuses any other flag by name. Its own tracking labels take
precedence over your labels with the same key.

## Provider access

Each agent gets access to the provider endpoints its client needs:

| Agent | Hosts a filtered run reaches |
| --- | --- |
| `coop claude` | `api.anthropic.com`, `platform.claude.com` (the OAuth refresh) and `mcp-proxy.anthropic.com` (the claude.ai connectors a login has on by default) |
| `coop codex` | `chatgpt.com` and `auth.openai.com`. An API-key account uses `api.openai.com` |
| `coop gemini` | the Google model and Code Assist routes that co:op's Gemini adapter declares for the selected credential type |
| `coop grok` | `cli-chat-proxy.grok.com`, `code.grok.com` and `auth.x.ai` (the OAuth refresh) |

Provider rules don't include release feeds, package registries, update checks or telemetry
(`raw.githubusercontent.com`, `registry.npmjs.org`, `api.github.com` and the Datadog intakes).
co:op disables this traffic through each client's settings inside the box, and your host profiles
stay unchanged. Updates are also disabled in co:op's images, so clients can't update themselves
even when their home folders aren't mounted.

| Client | In the box | Update switch in co:op's image |
| --- | --- | --- |
| Claude | runs with `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` and `DISABLE_UPDATES=1` | `DISABLE_UPDATES` |
| Codex | reads a generated `config.toml` that sets `check_for_update_on_startup = false`, `analytics.enabled = false` and the `otel` exporters to `none`, on top of your own settings | `/etc/codex/managed_config.toml`, which also turns off Codex's background server and its updater |
| Gemini | gets `general.enableAutoUpdate`, `general.enableAutoUpdateNotification` and `privacy.usageStatisticsEnabled` off, plus `GEMINI_TELEMETRY_ENABLED=false` | `/etc/gemini-cli/settings.json` |
| Grok | runs with `GROK_TELEMETRY_ENABLED=false` and `GROK_DISABLE_AUTOUPDATER=1` | `GROK_DISABLE_AUTOUPDATER` |

A session that only answers a prompt should produce no blocked requests from these background
features. If the agent or your project requests one of these hosts directly, co:op records the
blocked request as usual and never hides it from the report. You can inspect it and approve access.

## Provider credentials stay outside the box

API keys and OAuth or subscription accounts both use a provider broker for each Docker run, whether
networking is open or filtered. The broker accepts requests only for the selected providers and
accounts, and the provider folders mounted into the box never contain reusable credentials.

The host can refresh credentials during a long run without restarting the client. Removing or
replacing an account stops the old run from using it.

Clients keep using their original provider URLs. co:op's adapter for each provider lists the origins
and routes its client may use. A private loopback listener handles TLS for those origins. It checks
each request's path, method and query, then adds the selected account's credentials before
forwarding the request. Requests to undeclared routes on those origins are blocked.

Only the run itself receives the broker's certificate authority, and co:op never installs it on
the host or in a global trust store. The broker still verifies the provider's TLS certificate.
Other origins follow the run's open or filtered network rules.

A separate container with no Linux capabilities holds the short-lived credential snapshot. The box
joins that container's network namespace after co:op inspects the container, but can't read the
private snapshot or the CA signing key. When the run stops, co:op removes that container, so nothing can
reach the broker. If the host stops renewing or publishing snapshots, they expire and the broker refuses access. Filtered
runs keep their approved network rules, and co:op still records which traffic came from the box.
Open runs still allow ordinary outbound traffic.

The lead and its teammates share one account per provider inside a box. Run teammates in separate
boxes if they shouldn't trust one another. A fallback list can include both API-key and
subscription accounts.

The supported API variables are `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` and `GEMINI_API_KEY`. co:op
refuses alternate token variables, Vertex credentials and `XAI_API_KEY` instead of exposing them.
Gemini's own plain-file OAuth credentials are supported, but an encrypted-only cache with no
credential that co:op can import is not. co:op also refuses custom provider base URLs.

Offline runs receive no provider keys and don't start a broker. Online provider runs need Docker
with an ordinary bridge or a project services network. Host networking, arbitrary joins to another
container's network, and Apple's `container` runtime don't support this setup.

## A client's own downloads

A client may need to download public files as well as call its model API. For example, Codex
fetches its curated plugin store from chatgpt.com and github.com whenever an API-key run starts.

These downloads use the broker's `download` route, which sends no credentials and rejects any that
the box supplies. It allows only the exact requests that the provider's adapter lists, including
their query strings. For Codex, these are:

| Host | Request lines |
| --- | --- |
| chatgpt.com | `GET /backend-api/plugins/featured?platform=codex` |
| github.com | `GET /openai/plugins.git/info/refs?service=git-upload-pack` and `POST /openai/plugins.git/git-upload-pack` |

The github.com lines fetch one public repository and never push. The same discovery path with
`service=git-receive-pack` isn't in the list.

The broker never turns the public Git download route into general GitHub access. Other GitHub
requests still require ordinary network approval in filtered mode.

## MCP servers

### Filtered runs

Bearer tokens for MCP servers stay outside filtered boxes. When a shared server uses
`bearer_token_env_var`, co:op adds a route for it to the run's broker. The MCP configuration inside
the box points to that route's loopback listener and uses `COOP_MCP_TOKEN_<n>` instead of your
token. This stand-in works only for that server and run, and the broker adds the real token when it
forwards the request.

The route allows only the server's exact URL path and the methods MCP uses: POST, GET and DELETE.
It waits for long tool calls to finish. The agent can't bypass the broker by connecting directly to
that server's host.

Your token variable never enters a filtered box, even if that box doesn't load MCP. A session's ACP
adapter receives the stand-in too. If the token is missing or supplied through `-e`, co:op stops
the launch and names the server. Tools supplied by a remote session's controller use the same
route, restricted to the server's exact path.

### Legacy SSE servers

Legacy SSE servers need broader access because they choose a message endpoint during each
connection. co:op allows `GET` on the stream's path and `POST` on any path at the same host. Other
hosts and methods are blocked, and the broker still sends only one secret header.

The credential still stays outside the box, but a legacy SSE connection lets the box reach more
paths on that host than streamable HTTP does. co:op names these servers at launch and explains what
to ask their vendors for. Use a server's streamable HTTP URL when it offers one.

Some legacy SSE servers return a full endpoint URL instead of a path, and the client then posts
directly to that URL, bypassing co:op. Filtered networking blocks the request. Open networking
sends only the stand-in, so authentication fails without exposing the real token. To use this
server, switch to its streamable HTTP URL.

### Offline runs

An offline box can't reach remote MCP servers, so co:op removes them from its MCP configuration.
The launch reports: "MCP servers that need internet are left out: …". Local command servers remain
available, and remote-server token variables stay outside the box. An offline remote session's
editor adapter also receives only the local servers.

### Open runs

With open networking, the box connects directly to the internet, so co:op uses a separate helper
container to keep supported MCP credentials outside the box.

If the MCP configuration includes a remote server with a secret, co:op starts a helper from the
gateway image (`coop-net broker`). It joins the box's network, and co:op adds a hosts entry so the
box can find it. The helper runs without privileges (`--cap-drop ALL`, uid 65532, read-only root,
256 MB). It has the label `coop=broker` and the box's supervisor labels.

The helper exits with the `coop` process that started it, even after a crash, and box cleanup
removes any helper left behind.

Inside the box, each brokered MCP server points to `http://coop-broker:<port>`, where the port is
15580 plus the server's index. The box sends a stand-in token, and the helper replaces it with the
real credential and connects only to that server's host. Your token variable stays outside the box.

The box talks to the helper over plain HTTP on the container network. If the project has no
co:op-managed network, the helper uses Docker's default bridge, which is shared with other
containers that have no assigned network. On that bridge, the stand-in token protects access to the
credential. It's a random 256-bit value created for the run and accepted only by its own listener
for its server's route. co:op controls the helper's hosts entry and rejects any `--add-host`
argument that would replace it.

Remote sessions with open networking also use the helper for tools the controller provides. co:op
builds the helper image on first use.

The MCP helper is separate from the provider broker used in open and filtered runs. It allows exact
paths for streamable HTTP and the same-host routes described above for legacy SSE.

The following servers still run with their secrets inside the box. co:op names each one at launch:

- a server whose secret is in two places, or whose secret header is anything other than literal
  text followed by one variable reference
- a server whose URL isn't plain `https` on port 443
- every server, when the box has no network to reach a helper on (Apple's `container` runtime,
  `--network none` or `--network container:…` in your runtime arguments)

## Project services

A `service:` rule requests access to one project service and needs your approval. Before a filtered
run starts, co:op recreates that service and its dependencies on an internal network owned by the
project. They can talk to each other, but have no direct internet connection. With Docker Engine 26
or later, Docker's DNS server on this network resolves only service names. The filtered box sends
its own DNS requests only to the gateway.

The services reach the gateway through the standard HTTPS proxy variables. The gateway forwards
requests only for approved TLS names, after checking the name in the actual ClientHello. Removing
the proxy variables disconnects the services from the internet. It doesn't bypass the gateway.

Allowed and blocked external requests appear in `coop net watch`, `inspect`, `blocked` and the JSON
views, with the name of the Compose service they came from. Direct traffic between services stays on
the internal network and isn't reported as external traffic.

In filtered mode, you must name the service you need. co:op rejects `box.network: true` alone.
`coop init --services` adds a rule for each service it sets up, so one `coop approve` lets the box
reach them.

For any other Compose file, `coop approve` lists the services and their ports and asks once whether
to add and approve their rules:

- If you answer yes, it writes the rules under `box.egress_rules`, keeping the file's comments and
  order. It also saves each service's definition, so a later edit asks again.
- If you answer no, nothing changes.
- A service that declares no TCP port gets no guessed port. You need to finish its rule yourself.
- With no terminal, the command asks nothing and writes nothing. It shows the rules and
  `box.network: false`, and exits non-zero.

A filtered run that needs project services waits for every other box in the project to stop before
starting those services. Each loop run and editor session gets its own Compose project, network and
volumes. A run started directly uses the project's development stack.

Service approval covers both the name and the Compose definition you reviewed, and each launch
compares that definition with the current file. Changing `db:` to use a different image or network
setup requires another approval. Changing an unrelated service doesn't.

If the definition changed, the launch stops and tells you how to review it. `coop net` lists the
change under `⚠ New runs need your approval` as
`The Compose service "db" changed after its network access was approved.`

## TLS ports and localhost

A `tls` rule isn't limited to port 443. It lists its ports, such as `[443]`, `[853]` or
`[443, 8443]`, and the gateway redirects only those. It reads the original destination port from
the kernel's `SO_ORIGINAL_DST` record, so the client can't choose a different port after the
redirect.

A request to an unlisted port is blocked and recorded with its port number. Connecting directly to
the gateway's TLS listener is also blocked and counted, because that connection has no original
destination port.

You can run a test server on `127.0.0.1:3000` and use `curl` to reach it inside the box. This
traffic stays inside the box's network namespace and never reaches your host or the gateway. The
exception is Docker's DNS address, `127.0.0.11`, which is blocked so the box uses only the gateway
for DNS.

## Your project's image

You can use your own project image in filtered mode if its `.agent/Dockerfile` starts with
`ARG COOP_BASE_IMAGE` / `FROM ${COOP_BASE_IMAGE}`. Build it on the host with
`coop build --egress filtered`, or with plain `coop build` when the project's effective network
mode is filtered. co:op uses its locked client image as the base and tags the result
`coop-<repo>-<path hash>-filtered:<hash>`.

A filtered launch uses only a project image built from files you approved by running `coop build`.

- If the image is missing or the clients changed, co:op rebuilds those same files by itself. It
  works on a private snapshot and compares that snapshot with your approval first.
- If the build inputs changed, co:op asks you to review and rebuild, and `coop update` leaves that
  to you too. Build inputs include copied scripts and file modes, but not the secrets and Git
  metadata that co:op leaves out of the build.

To keep unrelated edits from requiring a rebuild, list the files this image doesn't build from in
`.agent/Dockerfile.dockerignore`, or in an ignore file named after your configured Dockerfile. Its
rules take precedence over the root `.dockerignore`.

- The running box still sees excluded files through its repository mount.
- For a toolchain-only image, exclude everything with `**` and add `!path/to/input` for each file
  it copies.
- The Dockerfile and the active ignore file always count as build inputs.
- A Docker exception can't bring back secrets, Git metadata, generated output or
  `.agent/project.yaml`. co:op leaves all of these out.

`.agent/project.yaml` is never a build input, with or without an ignore file. Adding a network rule
needs `coop approve` and no rebuild, and `COPY . .` can't put that file in an image. co:op refuses a
Dockerfile that copies the file by name, so copy another file instead. Pointing `box.dockerfile` at
another Dockerfile still needs a review and a build.

Before starting the box, co:op checks the built image itself, because the Dockerfile's `FROM` line
alone can't show what the image contains. Two checks must pass:

1. The locked image's layers must be the first layers of the built image, so the box is that exact
   image plus your own layers.
2. Every pinned client entry point must match the file in the locked image byte for byte. That
   covers each launcher, what it execs, and the native binaries it needs. co:op reads them out of a
   container that it creates and never starts, so nothing from the image runs to describe itself.

You can add tools to the image, but you can't replace, wrap or delete a pinned client. The host
setup checks cover co:op's own image, and your project image must pass both checks above on every
launch. co:op rejects `COOP_IMAGE` because an arbitrary image hasn't passed those checks.

co:op caches what it reads from an image outside every agent mount, tied to the image's ID.

The base image uses the non-root box user. To install packages, put `USER root` before the
installation commands and `USER node` after them. Review the Dockerfile and its copied files before
building, because the build runs project instructions with ordinary Docker networking. `--egress`
selects the image to build. It doesn't filter traffic during the build.

co:op checks the resulting image and saves approval for its exact inputs outside the agent's
mounts. If saving approval fails, the command fails even if Docker finished the build.

An agent's edit can't reach a build's ordinary networking, because co:op rebuilds only files you
approved. Such a rebuild fetches again whatever the
approved Dockerfile fetches. An editor connection with open networking may build a missing image.
An open-network run warns when the build inputs have changed, but keeps using the old image until
you run `coop build --egress open`.
Offline launches don't build project images automatically. co:op may still prepare its own embedded
base, gateway and client images, but those builds don't run instructions from your repository.

## Refused, with the reason you will see

| Requested | Message |
| --- | --- |
| IPv6 address, CIDR, or `icmpv6` | `IPv6 destinations are not supported yet — a filtered box runs on IPv4 only` |
| `tls` on port 53 | `TLS on port 53 is not allowed here — the gateway takes 53 for DNS; use 443 or another port` |
| raw `tcp`/`udp` on port 443 or 53 | `raw tcp to <dest> on port 443 is not allowed — the gateway takes 443 for TLS and DNS; use another port` |
| raw `tcp` on a port a `tls` rule also names | `raw tcp to <dest> on port 8443 clashes with a TLS rule on the same port — the gateway takes that port for TLS; use another port` |
| a `serve` port on a port the gateway takes for TLS or DNS | `port 8443 is one the gateway uses for TLS and DNS — serve on another port in filtered mode` |
| ICMP beyond echo-request | `ICMP N to <dest> is not supported yet — only echo-request is` |
| `cidr: 0.0.0.0/0` | `a /0 rule allows everything, which is not filtering — use --egress open if that is what you want` |
| a host, loopback, link-local or metadata range | `<range> is a protected range (your host, loopback, link-local or cloud metadata) — no rule can allow it` |
| a project image not built on the client image | `this project's .agent/Dockerfile did not build on coop's client image — start it with ARG COOP_BASE_IMAGE and FROM ${COOP_BASE_IMAGE} …` |
| a project image that changes a pinned client | `this project's .agent/Dockerfile changes claude's cli client at /opt/coop/bin/claude — a filtered box runs only the clients that coop net setup checked on this host …` |
| `COOP_IMAGE` | `a filtered box runs coop's own image — unset COOP_IMAGE to start one` |
| a runtime other than Docker, including a run with no mode set | `restricted networking needs docker; <runtime> cannot run the network gateway — run this with --egress open or --egress offline, set COOP_EGRESS, or set COOP_RUNTIME=docker` |
| `box.network: true` with no `service:` rule | `<Agent> cannot start because filtered boxes reach project services only through approved network rules`, then a complete rule to paste for each Compose service. In a terminal, `coop approve` offers to add them |
| `-v /var/run:/x` (or any mount of `/run`, `/proc`, `/sys`, `/dev`, `/`, or the Docker socket's directory) | `a filtered box cannot mount …: it is or holds …, which reaches Docker or the kernel` |
| a project file with an unapproved rule, or a changed rule or mode | `<Agent> cannot start because this project asks for network access that has not been approved` · `Review it: coop approve` |
| `--allow-domain` or `--egress-rules` on a run that is open or offline | `<Agent> cannot start because --allow-domain only adds to filtered access, and this run is offline` · `Use --egress filtered, or leave out --allow-domain.` |
| a rules file that is a bare list, or nests its rules under `box:` | `--egress-rules takes a file that lists its rules under one top-level egress_rules key, like this:`, then a rules file to copy |
| `open`, in a project with no approval yet | `<Agent> cannot start because this project asks for unrestricted internet access, which has not been approved` · `Review it: coop approve` |
| an approved Compose service whose definition changed | `<Agent> cannot start because the Compose service "db" changed since it was approved` · `Review it: coop approve` |
| an approved project directory replaced by another at the same path | `<Agent> cannot start because the project directory at <path> was replaced since it was approved` · `Review it: coop approve` |

If co:op can't enforce a rule, it stops the launch before saving approval or creating a container.
It never starts a run with a requested restriction silently removed.

### Ranges no rule can reach

Some destinations can never be allowed, by a `cidr:` rule or any other rule:

- the host's own interface addresses
- every subnet the container runtime allocates and the gateway address it holds in each one, so
  sibling containers, other sessions' boxes and the daemon's own address are out of reach
- the loopback, link-local and metadata ranges

co:op reads this list from the daemon at launch and updates it while the box runs. When another
filtered box creates a network, or a VPN brings an interface up, co:op adds the new subnets,
gateways and host addresses to the running box's protected set in one atomic kernel update. Those
addresses are blocked as soon as the update takes effect. A network change stops the box only if
the update is refused or the protected set would exceed 256 ranges. An address remains blocked
after it disappears from the host.

An approved CIDR can't override these protected ranges, and packets to protected addresses are
still dropped and counted. The only exception is an approved `service:` rule: it allows the exact
container address you approved, without allowing the rest of that subnet.

### Serve ports and mounts

A published `serve` port accepts connections only from the host. Other containers on the same bridge
can't connect through it.

A Docker socket or kernel control directory could let the agent bypass the gateway. For example,
one `curl` request to a Docker socket could start another container outside the filtered network.
So filtered runs reject mounts of `/var/run`, `/run`, `/proc`, `/sys`, `/dev`, `/` and the Docker
endpoint's socket directory, and the error names the blocked mount.

## Observed versus counted

`coop net inspect [<run>]` shows network activity for a run, or this project's newest one if you
omit the run. The report starts with totals for allowed traffic, then lists every destination the
box or its Compose services reached.

The rest of the report lists problems: blocked attempts, alerts, missing evidence, parts of the
network setup that didn't run normally, and unfinished cleanup. A clean run omits these sections.
`--json` includes every field.

At the end of an interactive filtered run, co:op saves the final network record and prints the same
report on stderr under `Networking stats:`. First, it says why the box stopped, as in
`The Coop box has stopped: main process exited with status 0.` When Ctrl-C reached `coop`, the
reason is `interrupted by Ctrl-C`.

What the report shows depends on what co:op could record:

- For TLS traffic, the gateway records each connection's requested name, resolved address and
  transferred bytes. It also saves an event when it blocks a name. `coop net blocked <host>` finds
  this project's newest run that blocked that host, groups its blocked attempts by cause and counts
  repeats. Use `--run <run>` to choose a run, and with it you can supply an exact event ID instead
  of a host.
- A blocked DNS lookup doesn't record which port the program wanted, so `coop net blocked` shows a
  rule for TLS on port 443, labeled as the likely fix.
- A direct connection to an address the run looked up, such as `http://` to a name approved only
  for TLS, is listed under that name with the address beside it, so `coop net blocked <host>`
  finds it. An address that two looked-up names share is listed as an address.
- For raw traffic, the kernel counts packets and bytes for each approved address rule. These
  appear under `Raw traffic` in `coop net inspect`, each row named by its rule, such as
  `10.42.8.0/24:5432 · TCP`, and as `address_grants` in `--json`. Individual hosts within a CIDR
  aren't recorded or shown.
- For other blocked packets, the filter counts how many it drops but doesn't record their
  destinations, so the report shows only a count
  (`N packets were blocked, and Coop did not record where they were going`). `coop net blocked`
  has no destination record to open, so check the program's own error for the address it wanted.
  When the report also lists a direct connection, whose packets are part of that count, it reads
  `N packets were dropped in all, direct connections included`.
- An unmeasured metric appears as UNKNOWN, never zero. A total is also UNKNOWN if any part of it
  wasn't measured.
- Only traffic co:op can identify as coming from the box or its Compose services appears as
  `Allowed`. co:op's own resolver connection and a closing kernel socket with no owner appear only
  in `--json`. Other connections that co:op can't tie to either one appear under
  `Other observed endpoints` with the reason, and don't count as allowed traffic.

## Runs, checks and exports

Use any unique prefix of a run's ID to select it. The eight characters shown by `coop net runs` work
with every run command. If a prefix matches more than one run, co:op refuses it and shows the longer
prefixes you can use.

`coop net runs` shows this project's 25 newest runs. When there are more, it shows a count and the
`--all` option. Use `--all-projects` to see runs from every project, labeled by name and path. co:op
keeps a finished run's record for 90 days after it ends. If cleanup is still pending, it keeps the
record until cleanup finishes.

Each run also keeps a copy of the rules it started with, which the `--run` option below reads. co:op
removes the saved copy of a launch's rules only when no retained run record refers to it and no
launch has used it for 90 days. It keeps the copy a worker session was created with for the whole
session. An editor session that starts no box for 90 days may need to be started again. A copy made
by `coop net setup` is removed along with that check's run record.

`coop net check <url-or-host>` tells you whether the rules would allow a normal new run in this
project to reach a destination. It gives one answer and one reason, based on the approved project
rules and each agent's provider access. Use `--run <run>` to check the rules a recorded run started
with instead. Neither check sends network traffic. The exit status is the answer:

- 0: the rules allow the connection
- 3: the rules block it, or new runs need approval first
- 1: the check could not finish
- 2: invalid arguments

To check an IP address, also specify `--protocol tcp|udp --port <n>` (or `--icmp`) and a run,
because the answer depends on the transport and that run's protected ranges.

`coop net export <run>` exports the run's final record as JSON. It leaves out destination names and
addresses by default, because even a blocked name can contain a secret. Use `--include-addresses` to
include them in the export on your machine. The `digest` is a checksum of the exported content. It
isn't a signature and doesn't make a partial record complete.

## Asking for access

Add the rule to `.agent/project.yaml` under `box.egress_rules`, then ask a human to run
`coop approve` on the host. Editing the repository file requests access but doesn't grant it. co:op
stores approval outside the repository, so editing or deleting the file can't widen access, and
nothing inside the box can approve its own request. New runs use the approved rules, and a box
that's already running keeps the rules it started with.

In a filtered editor session (`coop acp`), each new thread starts with a note naming what the
agent can reach and how to allow more.

A running editor session can pick up a new approval without reconnecting. Once no prompt is
running, co:op starts a box with the new rules, then stops the old box and any processes left in
it, and the editor shows a line explaining this. A box started for another reason, such as a crash
or provider switch, also gets the new rules.

If co:op can't apply the rules, or the new box stops before the switch, the session keeps its
previous rules and explains why. After `coop net forget`, a running session keeps its rules until
you reconnect the editor.

### Reviewing a request

Approval covers the file's exact network mode, rules and the reviewed definition of every `service:`
it names. To change network access, edit the file and review it again. Before saving approval,
`coop approve` checks that the runtime can enforce the rules, just as it does at launch. It refuses
unsupported rules, such as an IPv6 destination, during review so they can't be approved and then
fail at every launch.

Plain `coop approve` doesn't allow any file exposure. With mount protection, a launch may refuse
hidden files that stay readable under another letter case. To review that permission as well, use
`coop approve --case-exposure`. A run that uses eBPF enforcement protects those files without this
approval. See [hiding secrets](box.md#hiding-secrets).

The review compares the request with the access already approved. Unchanged rules have no marker,
additions use `+` and removals use `-`. co:op explains any network mode change in plain words, and a
request for `open` gets a red warning because it would allow unrestricted traffic. To confirm,
answer `Approve this access for new runs and running editor sessions? [y/N]`.

If the file is already exactly what was approved, `coop approve` prints
`No approval needed: this project's requested access has not changed.` and grants nothing new. If a
filtered box still can't start because the project's services have no network rules, it says that
instead and offers to add them. See [project services](#project-services).

Provider endpoints aren't shown as access the project requested, because co:op allows the endpoints
each agent needs.

### Pending requests

If the project's request differs from your saved approval, co:op treats it as pending:

- `coop init` ends with `⚠ New runs need your approval`, the reason, and
  `Review changes: coop approve`.
- Running `coop net` without a subcommand shows the same comparison under
  `⚠ New runs need your approval`.
- Every launch stops before starting a box or its main process. It explains why and prints
  `Review it: coop approve`. This applies to `coop run`, a named agent, `coop loop`, an interactive
  fork, fork ACP, and a fork review or merge gate.
- `coop acp` connects your editor anyway and runs no agent until you approve. See
  [editors](integrations.md#when-the-project-box-is-not-ready). A withdrawn approval, a replaced
  project folder or an offline setting still stops it.

### Access for one run

For one run, use `--allow-domain <host>` to allow an exact name over TLS on port 443, or pass
`--egress-rules <path>` to add the rules in a file. Both add to the project's approved rules for
that run only. A rules file lists its rules under one top-level `egress_rules` key, the way
`.agent/project.yaml` lists them under `box.egress_rules`:

    egress_rules:
      - to: {domain: "docs.example.com"}
        protocol: tls
        ports: [443]

Keep the file outside the project. A rules file inside a folder mounted into the agent's box only
requests access, so the agent can't use such a file to approve its own rules.

Both flags only add to filtered access, and co:op refuses them by name on an open or offline run.

### The approval follows the directory

Approval belongs to the project directory itself. If you move an approved checkout aside and put
another checkout at the same path, co:op refuses the next launch until a human reviews it again.
`coop net` reports that access is blocked and names the command to review it.

co:op identifies the directory by a hidden marker file with `0600` permissions. The marker is
hard-linked to a file in co:op's private state, so the checkout and `$XDG_STATE_HOME` (or
`~/.local/state`) must be on the same filesystem, and that filesystem must support hard links. The
marker contains identifiers, not credentials.

Running `git clean -x` removes ignored files, including this marker. Run `coop approve` again to
review and restore access. Copying the marker's contents doesn't restore its hard link.

If you move a checkout with its marker intact, `coop approve` can recover it. It verifies the marker
against co:op's private hard link, removes the old marker and private link, then asks you to approve
the checkout's new path. A copied marker can't inherit the original checkout's access or cancel its
approval.

If a marker is malformed, inspect it with `ls -l -- ./.coop-network-approval`. Only after confirming
it's stale, remove it inside that checkout with `rm -- ./.coop-network-approval`, then run
`coop approve` again.

The box can see the marker, so co:op also checks the paths in runtime mount arguments:

- You may mount the exact project directory.
- You may not mount a path inside it that an existing agent could swap before Docker opens the
  mount, or a writable parent directory that would let the agent replace the project.
- No mount may expose co:op's private state, even read-only. That includes the paths it will
  use for network state, forks, execution records and launch locks on a first run, before those
  paths exist.
- co:op inspects existing named volumes. When it uses the marker to verify approval, it refuses
  `--volumes-from` and custom volume-driver options. To mount a cache, keep it outside the
  checkout and name its path explicitly so co:op can inspect it.

### Forgetting an approval

`coop net forget` removes a project's approval. It shows the saved access, asks you to confirm in
the terminal, then removes only that approval record. Run it in the project, or pass
`--project <path>` if the checkout is already gone. Run records, their receipts and this host's
setup results stay. Until you approve again, only offline runs of that project start.

If the approved path was a symlink, the approval belongs to the directory it pointed to. Once both
the symlink and directory are gone, co:op can't find the approval from that path. `coop net forget`
explains this and leaves other projects' approvals alone.

## When a run is interrupted

The `coop` process that starts a filtered run manages its gateway containers, volumes and network
record. If it dies because of `SIGKILL`, a crash or a reboot, the run stays `cleanup pending` until
co:op recovers it. If it died after cleanup but before saving the final record, the status is
`final record missing`. co:op attempts recovery automatically:

- `coop net inspect` makes one recovery attempt for that run, with a time limit, before
  reporting its cleanup status.
- Starting a loop, fork or build checks for runs waiting for cleanup and attempts recovery.
- Every filtered launch does the same before starting its own box.

If recovery succeeds, you don't need to do anything. If it can't finish, co:op reports what
prevented it, such as Docker being stopped, a different daemon at the saved endpoint, a resource it
couldn't remove, or a resource Docker may still be creating. If Docker never answered a request to
create a resource, co:op counts that resource as gone only if it's still missing ten minutes later.
`coop net inspect` shows `⚠ Cleanup incomplete — <the blocker>` and what to do next, and co:op
retries automatically.

To recover runs now:

    coop net recover           # settle every pending run now
    coop net recover <run>     # settle one run now

Without a run, `coop net recover` reports only the runs it cleaned up or couldn't clean up. A run
whose own `coop` process is still running, in this project or another, is left for that process to
clean up and isn't listed. Name the run to see why it was left alone.

When `coop net recover` can't finish a run, it prints `⚠ Cleanup incomplete for network run <run>`,
followed by the causes and what to do next. An unreadable run record doesn't stop recovery of the
others: co:op names any records it couldn't read and exits 1.

Recovery removes only that run's resources, using their exact names and the run's labels. It never
matches a name prefix or runs a general prune. It saves a final receipt marked `partial`, with the
workload outcome `supervisor_lost`.
