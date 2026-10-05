# Transparent HTTPS egress POC

This experiment is retired. co:op's supported filtered networking, `--egress filtered`, shipped
separately: see [docs/networking.md](../../docs/networking.md).

This is an opt-in experiment. It isn't a supported co:op networking mode. It doesn't change
`coop` launch behavior, mount a repository or a credential store, publish host ports or
change the host firewall. Don't put real credentials in this image.

## Run

You need Python 3 and a local Linux Docker engine that permits namespace-local nftables. A
Linux VM on macOS counts. The build downloads packages. By default the experiment uses a
synthetic TLS fixture. With `--live`, it also contacts the public Anthropic, OpenAI and
Emisar endpoints, without credentials.

From the repository root:

```sh
docker build --progress=plain -t coop-egress-poc:experiment tools/egress-poc
poc_evidence_root="$(mktemp -d)"
python3 tools/egress_poc.py --live --output "$poc_evidence_root/run"
```

The command prints the path to `report.json`. It exits nonzero if an assertion fails or the
cleanup fails. The report holds the individual checks, timing summaries, the Docker version,
the exact built image ID, a DNS snapshot and the cleanup results. Its output directory must
not exist yet.

The defaults are 1,000 sequential requests, 100 requests at concurrency 10, five startup
samples and five 16 MiB transfers per path. The warm gateway startup measurement leaves out
the image download and build, public DNS preparation and fixture startup. Percentiles from
five samples are descriptive. They aren't tail-latency guarantees.

Baseline requests use a hosts-file entry. Proxied requests also pay for the local DNS
lookup. The two run one after the other, not interleaved, so host load can affect the
difference. The report keeps the concurrent timings and the fixture's accept-overflow
counters, so you can tell a fixture bottleneck from gateway latency. Probes fail fast. A
check that's absent from a failed report wasn't tested. Don't count it as passed.

`make check` runs the host-only policy regression tests, through `make tools-test`:

```sh
python3 -m unittest discover -s tools -p 'test_egress_poc.py'
```

## Boundary

Each experiment creates one dedicated bridge and its own fixture, gateway and client
containers. Clients share the gateway's network namespace. They don't share its mounts or
its PID namespace.

A trusted init installs namespace-local nftables. It then drops its UID and all
capabilities before it starts Envoy and dnsmasq. The client starts only after the gateway
listeners are ready. Both have read-only roots, tmpfs scratch and no-new-privileges. Clients
run as UID 1000, and services run as UID 65532.

Dropping capabilities applies to the running service processes. Whoever holds the trusted
host's Docker socket can still create a root/NET_ADMIN exec or diagnostic container.

TCP 443 is transparently redirected to Envoy. The exact visible SNI selects a static
upstream. An arbitrary original destination doesn't select the upstream. The client still
validates the real server's certificate.

DNS over TCP/UDP 53 is redirected, ahead of Docker's rules, to a static resolver with no
upstream. Unknown names aren't forwarded anywhere. Other traffic is rejected for both IP
families, including Docker's hidden high-port resolver. Even the trusted service UID may
connect only to the fixed upstream IP/port pairs. Envoy runs with no admin listener and no
hot-restart abstract socket.

The synthetic fixture is the only deliberately private upstream. Its IP and port come from a
container this run created. They're never accepted from user input. All public upstream
answers must be public unicast IPv4 before any of them is used. They stay frozen for this
short experiment. Only the fixture's public certificate is mounted into clients. Its private
key and the gateway policy never are.

Cleanup checks the recorded resource IDs and ownership labels. It removes clients before
their gateways, then the fixture and the bridge. The runtime resources are removed. The
reusable built image and the evidence directory stay.

If the host harness is forcibly killed or the Docker daemon is unreachable, resources can be
left behind. Use the experiment label or the recorded IDs to inspect them. Never use a
global prune. Don't automatically restart a gateway underneath an existing client.

The failure probe pauses the gateway with its interface still up. It proves direct egress
stays denied, then tests recovery after unpausing. A separate kill probe records the link
and route state, proves the client stays alive and compares the retained firewall rules
using a trusted inspector. Docker may remove the interface when the gateway dies. That
timing isn't attributed to the firewall alone.

To look for leftovers after an interrupted harness, run these read-only commands:

```sh
docker ps -a --filter label=coop.egress-poc
docker network ls --filter label=coop.egress-poc
```

Before cleanup, inspect each candidate's full `coop.egress-poc` label and ID, because other
experiments may be active. A killed harness may not have written its report.

## What this does not establish

- SNI isn't encrypted HTTP Host/path authorization. ECH, shared-CDN fronting, HTTPS tunnels
  and compromised approved origins remain outside this claim.
- It doesn't qualify DNS refresh or rebinding, or long-session reliability. There's no
  arbitrary TCP, cleartext HTTP, QUIC/HTTP3, SSH, IPv6 upstream or WebSocket-specific test.
- Arbitrary localhost ports are blocked too. Local MCP servers, development servers and
  debuggers don't work through this narrow policy. This POC doesn't demonstrate a general
  development-container networking policy.
- A provider's HTTP 401 proves verified TLS to the API. It doesn't prove authenticated
  inference. Reaching Emisar over HTTP doesn't prove an authenticated MCP tool invocation.
- Nothing here tests a native Claude/Codex invocation inside the restricted namespace,
  provider failover, token refresh or a credential broker.
- The kernel, Docker daemon, host, gateway binaries and image supply chain are trusted. This
  isn't a hostile-kernel escape test or an image vulnerability audit. The base is
  digest-pinned, but the apt package versions aren't a reproducible lockfile.
- Short repeated runs and an injected gateway death aren't daemon-restart, OOM,
  long-duration or multi-runtime qualification. A passing report makes no claim of
  universal safety.

## Design sources

- [Docker network-namespace sharing](https://docs.docker.com/engine/network/#container-networks)
- [Docker installs DNS rules inside namespaces](https://docs.docker.com/engine/network/firewall-iptables/)
- [Envoy TLS inspector](https://www.envoyproxy.io/docs/envoy/v1.39.1/configuration/listeners/listener_filters/tls_inspector)
- [dnsmasq static records and no-upstream options](https://thekelleys.org.uk/dnsmasq/docs/dnsmasq-man.html)
- [nftables socket UID and hooks](https://netfilter.org/projects/nftables/manpage.html)

Fable 5.1's design consultation identified two hazards: the hidden Docker DNS listener and
the Envoy hot-restart socket. Both have explicit runtime probes.
