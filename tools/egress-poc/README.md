# Transparent HTTPS egress POC

An opt-in experiment, not a supported Coop networking mode. It does not change
`coop` launch behavior, mount a repository or credential store, publish host ports,
or change the host firewall. Do not put real credentials in this image.

## Run

Requires Python 3 and a local Linux Docker engine (including a Linux VM on macOS)
that permits namespace-local nftables. The build downloads packages. The default
experiment uses a synthetic TLS fixture; `--live` additionally contacts the public
Anthropic, OpenAI and Emisar endpoints without credentials.

From the repository root:

```sh
docker build --progress=plain -t coop-egress-poc:experiment tools/egress-poc
poc_evidence_root="$(mktemp -d)"
python3 tools/egress_poc.py --live --output "$poc_evidence_root/run"
```

The command exits nonzero on a failed assertion or cleanup failure and prints the
path to `report.json`. That report contains individual checks, timing summaries,
the Docker version, exact built image ID, DNS snapshot and cleanup results. Its
output directory must not already exist. Defaults: 1,000 sequential requests,
100 requests at concurrency 10, five startup samples and five 16 MiB transfers
per path. Image download/build, public DNS preparation and fixture startup are
not included in the warm gateway startup measurement. Percentiles from five
samples are descriptive, not tail-latency guarantees.

Baseline requests use a hosts-file entry; proxied requests also pay for the local
DNS lookup. They run sequentially, not interleaved, so host load can influence the
difference. The report retains concurrent timings and fixture accept-overflow
counters to distinguish a fixture bottleneck from gateway latency. Probes fail
fast: absent checks in a failed report are untested, not implicitly passed.

Host-only policy regressions are part of `make check` via `make tools-test`:

```sh
python3 -m unittest discover -s tools -p 'test_egress_poc.py'
```

## Boundary

Each experiment creates one dedicated bridge and its own fixture, gateway and
client containers. Clients share the gateway's network namespace, not its mounts
or PID namespace. Trusted init installs namespace-local nftables, then drops its
UID and all capabilities before starting Envoy and dnsmasq. The client starts
only after the gateway listeners are ready. Both have read-only roots, tmpfs
scratch and no-new-privileges; clients run as UID 1000, services as UID 65532.
Capability dropping applies to the running service processes. The trusted host's
Docker socket holder can still create a root/NET_ADMIN exec or diagnostic container.

TCP 443 is transparently redirected to Envoy. Exact visible SNI selects a static
upstream; an arbitrary original destination does not select the upstream. The
client still validates the real server's certificate. DNS over TCP/UDP 53 is
redirected before Docker's rules to an upstream-free static resolver. Unknown
names are not forwarded anywhere. Other traffic is rejected for both IP families,
including Docker's hidden high-port resolver. Even the trusted service UID may
connect only to the fixed upstream IP/port pairs. No Envoy admin listener or
hot-restart abstract socket is enabled.

The synthetic fixture is the only deliberately private upstream. Its IP and port
are obtained from a container created by this run, never accepted from user input.
Public upstream answers must all be public unicast IPv4 before any are used; they
are frozen for this short experiment. Only the public fixture certificate is
mounted into clients, never its private key or gateway policy.

Cleanup checks recorded resource IDs and ownership labels and removes clients
before their gateways, then the fixture and bridge. Runtime resources are removed;
the reusable built image and evidence directory remain. A forcibly killed host
harness or unreachable Docker daemon can leave resources: use the experiment
label/recorded IDs to inspect them, never a global prune. Do not automatically
restart a gateway underneath an existing client.

The failure probe pauses the gateway with its interface still up, proves direct
egress remains denied, and tests recovery after unpausing. A separate kill probe
records link/route state, proves the client remains alive and compares retained
firewall rules using a trusted inspector. Docker may remove the interface on
gateway death; that timing is not attributed solely to the firewall.

Read-only discovery after an interrupted harness:

```sh
docker ps -a --filter label=coop.egress-poc
docker network ls --filter label=coop.egress-poc
```

Inspect each candidate's full `coop.egress-poc` label and ID before cleanup;
other experiments may be active. A killed harness may not have written its report.

## What this does not establish

- SNI is not encrypted HTTP Host/path authorization. ECH, shared-CDN fronting,
  HTTPS tunnels and compromised approved origins remain outside this claim.
- No DNS refresh/rebinding qualification or long-session reliability. No arbitrary
  TCP, cleartext HTTP, QUIC/HTTP3, SSH, IPv6 upstreams or WebSocket-specific test.
- Arbitrary localhost ports are blocked too: local MCP servers, development
  servers and debuggers are not usable through this narrow policy. This POC does
  not demonstrate a general development-container networking policy.
- Provider HTTP 401 proves verified TLS to the API, not authenticated inference.
  Emisar HTTP reachability does not prove an authenticated MCP tool invocation.
- No native Claude/Codex invocation inside the restricted namespace, provider
  failover, token refresh, or credential broker is tested here.
- The kernel, Docker daemon, host, gateway binaries and image supply chain are
  trusted. This is not a hostile-kernel escape test or an image vulnerability audit.
  The base is digest-pinned; apt package versions are not a reproducible lockfile.
- Short repeated runs and injected gateway death are not daemon-restart, OOM,
  long-duration or multi-runtime qualification. No statement of universal safety
  follows from a passing report.

## Design sources

- [Docker network-namespace sharing](https://docs.docker.com/engine/network/#container-networks)
- [Docker installs DNS rules inside namespaces](https://docs.docker.com/engine/network/firewall-iptables/)
- [Envoy TLS inspector](https://www.envoyproxy.io/docs/envoy/v1.39.1/configuration/listeners/listener_filters/tls_inspector)
- [dnsmasq static records and no-upstream options](https://thekelleys.org.uk/dnsmasq/docs/dnsmasq-man.html)
- [nftables socket UID and hooks](https://netfilter.org/projects/nftables/manpage.html)

Fable 5.1's design consultation specifically identified the hidden Docker DNS
listener and Envoy hot-restart socket hazards; both have explicit runtime probes.
