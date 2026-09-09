#!/usr/bin/env python3
"""Opt-in Docker experiment. Not a production Coop provider or security guarantee."""

import argparse
import ipaddress
import json
import math
from pathlib import Path
import re
import socket
import statistics
import subprocess
import time
import uuid


PUBLIC_HOSTS = ("api.anthropic.com", "api.openai.com", "emisar.dev")


def hostname(value):
    if len(value) > 253 or value != value.lower() or "." not in value:
        raise ValueError("expected an exact lowercase DNS hostname")
    for label in value.split("."):
        if not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", label):
            raise ValueError("wildcards, URLs and malformed names are not allowed")
    try:
        ipaddress.ip_address(value)
    except ValueError:
        return value
    raise ValueError("IP literals are not hostnames")


def public_ipv4(value):
    address = ipaddress.ip_address(value)
    if address.version != 4 or not address.is_global or address.is_multicast:
        raise ValueError(f"not a public unicast IPv4 address: {value}")
    return str(address)


def snapshot(names):
    result = {}
    for name in names:
        hostname(name)
        addresses = sorted({row[4][0] for row in socket.getaddrinfo(
            name, 443, family=socket.AF_INET, type=socket.SOCK_STREAM)})
        if not addresses:
            raise ValueError(f"no IPv4 addresses for {name}")
        # Reject the whole answer, not just the bad entries in a mixed response.
        result[name] = [(public_ipv4(address), 443) for address in addresses]
    return result


def socket_address(address, port):
    return {"socket_address": {"address": address, "port_value": port}}


def configuration(policy):
    """Input comes only from snapshot() or the harness-owned synthetic fixture."""
    chains, clusters, upstream_rules, dns = [], [], [], []
    for index, (name, endpoints) in enumerate(sorted(policy.items())):
        hostname(name)
        cluster = f"allowed_{index}"
        chains.append({
            "filter_chain_match": {"server_names": [name], "transport_protocol": "tls"},
            "filters": [{"name": "envoy.filters.network.tcp_proxy", "typed_config": {
                "@type": "type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy",
                "stat_prefix": cluster, "cluster": cluster, "idle_timeout": "300s",
            }}],
        })
        clusters.append({
            "name": cluster, "type": "STATIC", "connect_timeout": "3s",
            "load_assignment": {"cluster_name": cluster, "endpoints": [{"lb_endpoints": [
                {"endpoint": {"address": socket_address(address, port)}}
                for address, port in endpoints
            ]}]},
        })
        for address, port in endpoints:
            if ipaddress.ip_address(address).version != 4 or not 1 <= port <= 65535:
                raise ValueError("invalid upstream")
            upstream_rules.append(f"meta skuid 65532 ip daddr {address} tcp dport {port} accept")
        dns.append(f"host-record={name},{endpoints[0][0]}")
    if not chains:
        raise ValueError("empty experiment policy")
    envoy = {"static_resources": {"listeners": [{
        "name": "https", "address": socket_address("127.0.0.1", 15001),
        "listener_filters_timeout": "3s", "continue_on_listener_filters_timeout": False,
        "listener_filters": [{"name": "envoy.filters.listener.tls_inspector", "typed_config": {
            "@type": "type.googleapis.com/envoy.extensions.filters.listener.tls_inspector.v3.TlsInspector",
        }}], "filter_chains": chains,
    }], "clusters": clusters}}
    firewall = """table inet coop_poc {
 chain capture {
  type nat hook output priority -110; policy accept;
  meta skuid != 65532 tcp dport 443 redirect to :15001
  meta skuid != 65532 udp dport 53 redirect to :1053
  meta skuid != 65532 tcp dport 53 redirect to :1053
 }
 chain output {
  type filter hook output priority 0; policy drop;
  ct state established,related accept
  # OUTPUT interface metadata can retain eth0 after DNAT; match the rewritten tuple.
  ip daddr 127.0.0.1 tcp dport { 15001, 1053 } accept
  ip daddr 127.0.0.1 udp dport 1053 accept
  UPSTREAM_RULES
  reject with icmpx type admin-prohibited
 }
 chain input {
  type filter hook input priority 0; policy drop;
  ct state established,related accept
  iifname "lo" accept
 }
}
""".replace("UPSTREAM_RULES", "\n  ".join(sorted(set(upstream_rules))))
    resolver = "\n".join([
        "port=1053", "listen-address=127.0.0.1", "bind-interfaces",
        "no-resolv", "no-poll", "no-hosts", "local=/#/", "cache-size=0",
        "pid-file=/tmp/dnsmasq.pid", *dns, "",
    ])
    return {"envoy.json": json.dumps(envoy, indent=2), "firewall.nft": firewall,
            "dnsmasq.conf": resolver}


def summary(samples):
    ordered = sorted(samples)
    return {"n": len(samples), "median_ms": round(statistics.median(samples), 3),
            "p95_ms": round(ordered[math.ceil(.95 * len(samples)) - 1], 3),
            "max_ms": round(max(samples), 3)}


class Experiment:
    def __init__(self, image, output):
        self.image = image
        self.output = output
        self.identity = uuid.uuid4().hex[:12]
        self.resources = []
        self.results = {"experiment": self.identity, "checks": [], "limits": [
            "HTTPS/TCP visible SNI, not encrypted HTTP origin enforcement; no MITM",
            "Static DNS snapshot; no refresh, ECH rejection, QUIC or cleartext HTTP",
            "No real credentials; provider responses prove reachability, not model/MCP success",
            "Only the recorded Docker runtime is exercised; not production support",
        ]}

    def docker(self, *args, check=True, timeout=45):
        result = subprocess.run(["docker", *args], capture_output=True, text=True, timeout=timeout)
        if check and result.returncode:
            raise RuntimeError(f"docker {args[0]} failed: {result.stderr.strip()}")
        return result

    def network(self):
        result = self.docker("network", "create", "--label", f"coop.egress-poc={self.identity}",
                             f"coop-egress-poc-{self.identity}")
        identifier = result.stdout.strip()
        self.resources.append(("network", identifier))
        return identifier

    def container(self, role, network, command, extra=()):
        identifier = self.docker(
            "create", "--name", f"coop-egress-poc-{self.identity}-{role}",
            "--label", f"coop.egress-poc={self.identity}", "--network", network,
            "--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m,mode=1777",
            "--cap-drop", "ALL", "--security-opt", "no-new-privileges=true",
            "--pids-limit", "128", "--memory", "384m", *extra,
            self.image, command,
        ).stdout.strip()
        self.resources.append(("container", identifier))
        self.docker("start", identifier)
        return identifier

    def execute(self, container, *command, check=True, timeout=30):
        return self.docker("exec", container, *command, check=check, timeout=timeout)

    def ready(self, container):
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if self.execute(container, "test", "-f", "/tmp/ready", check=False).returncode == 0:
                return
            state = self.docker("inspect", "-f", "{{.State.Running}}", container).stdout.strip()
            if state != "true":
                raise RuntimeError(self.docker("logs", container).stderr)
            time.sleep(.05)
        raise RuntimeError("container readiness exceeded 15 seconds")

    def check(self, name, passed, detail=""):
        self.results["checks"].append({"name": name, "passed": bool(passed), "detail": detail})
        if not passed:
            raise AssertionError(f"{name}: {detail}")

    def curl(self, agent, url, *options):
        return self.execute(agent, "curl", "--silent", "--show-error", "--noproxy", "*",
                            "--connect-timeout", "2", "--max-time", "5", *options,
                            url, check=False)

    def pair(self, network, policy_dir, ca_path, suffix):
        started = time.monotonic()
        keeper = self.container(f"keeper-{suffix}", network, "keeper", [
            "--cap-add", "NET_ADMIN", "--cap-add", "SETUID", "--cap-add", "SETGID",
            "--cap-add", "SETPCAP", "--mount", f"type=bind,src={policy_dir},dst=/policy,readonly",
        ])
        self.ready(keeper)
        ready_ms = (time.monotonic() - started) * 1000
        agent = self.container(f"agent-{suffix}", f"container:{keeper}", "idle", [
            "--user", "1000:1000", "--mount", f"type=bind,src={ca_path},dst=/tmp/fixture-ca.pem,readonly",
        ])
        return keeper, agent, ready_ms, (time.monotonic() - started) * 1000

    def remove(self, kind, identifier):
        # Every deletion is bounded to a recorded resource with the exact experiment label.
        if (kind, identifier) not in self.resources or not re.fullmatch(r"[0-9a-f]{64}", identifier):
            raise RuntimeError("refusing cleanup of an unowned resource")
        label = self.docker(kind, "inspect", "-f",
                            '{{index .Labels "coop.egress-poc"}}' if kind == "network" else
                            '{{index .Config.Labels "coop.egress-poc"}}', identifier).stdout.strip()
        if label != self.identity:
            raise RuntimeError("resource ownership label changed")
        args = ("network", "rm", identifier) if kind == "network" else ("rm", "-f", identifier)
        self.docker(*args)
        self.resources.remove((kind, identifier))

    def cleanup(self):
        errors = []
        for kind, identifier in reversed(self.resources[:]):
            try:
                self.remove(kind, identifier)
            except (RuntimeError, subprocess.TimeoutExpired) as error:
                errors.append(str(error))
        self.results["cleanup"] = {"remaining": self.resources, "errors": errors}
        return not errors


def run(experiment, requests, live, startup_runs):
    e = experiment
    e.results["docker"] = json.loads(e.docker("version", "--format", "{{json .}}").stdout)
    e.results["image"] = json.loads(e.docker("image", "inspect", e.image).stdout)[0]["Id"]
    e.image = e.results["image"]
    network = e.network()
    fixture = e.container("fixture", network, "fixture", ["--user", "1000:1000"])
    e.ready(fixture)
    fixture_ip = json.loads(e.docker("inspect", fixture).stdout)[0]["NetworkSettings"]["Networks"]
    fixture_ip = next(iter(fixture_ip.values()))["IPAddress"]
    policy = snapshot(PUBLIC_HOSTS) if live else {}
    public_policy = json.loads(json.dumps(policy))
    policy["allowed.test"] = [(fixture_ip, 8443)]
    e.results["policy"] = {"public_snapshot": public_policy,
                           "synthetic_fixture_only": {"allowed.test": [fixture_ip, 8443]}}
    policy_dir = e.output / "policy"
    policy_dir.mkdir()
    for filename, contents in configuration(policy).items():
        (policy_dir / filename).write_text(contents)
    ca = e.docker("exec", fixture, "openssl", "x509", "-in", "/tmp/cert.pem").stdout
    ca_path = e.output / "fixture-ca.pem"
    ca_path.write_text(ca)
    keeper, agent, ready_ms, pair_ms = e.pair(network, policy_dir, ca_path, "0")
    cert = ("--cacert", "/tmp/fixture-ca.pem")
    environment = e.execute(agent, "env").stdout.splitlines()
    proxy_names = {"http_proxy", "https_proxy", "all_proxy", "no_proxy"}
    e.check("no_proxy_environment", not any(line.split("=", 1)[0].lower() in proxy_names for line in environment))
    result = e.execute(agent, "curl", "--silent", "--show-error", "--max-time", "5",
                       *cert, "https://allowed.test")
    e.check("ordinary_curl", result.stdout == "ok", result.stderr)
    result = e.curl(agent, "https://allowed.test", *cert)
    e.check("curl_explicitly_ignoring_proxies", result.returncode == 0 and result.stdout == "ok", result.stderr)
    result = e.curl(agent, "https://allowed.test", *cert, "--resolve", "allowed.test:443:203.0.113.7")
    e.check("approved_sni_routes_to_fixed_upstream_not_original_ip",
            result.returncode == 0 and result.stdout == "ok", result.stderr)
    for name, url, options in [
        ("unknown_dns_name", "https://denied.test", ()),
        ("denied_sni_same_upstream", "https://denied.test", ("--resolve", f"denied.test:443:{fixture_ip}", "-k")),
        ("no_sni_ip_literal", f"https://{fixture_ip}", ("-k",)),
        ("cleartext_http", "http://allowed.test", ()),
        ("direct_fixture_port", f"https://{fixture_ip}:8443", ("-k",)),
        ("metadata", "http://169.254.169.254/latest/meta-data/", ()),
        ("ipv6_literal", "https://[2606:4700:4700::1111]", ("-k",)),
        ("proxy_admin", "http://127.0.0.1:9901/server_info", ()),
    ]:
        result = e.curl(agent, url, *options)
        e.check(name, result.returncode != 0, result.stderr.strip())
    for transport in ("+tcp", "+notcp"):
        for resolver in ("127.0.0.11", "8.8.8.8"):
            result = e.execute(agent, "dig", f"@{resolver}", "allowed.test", "+short", "+tries=1", "+time=1", transport)
            e.check(f"controlled_dns_{resolver}_{transport}", result.stdout.strip() == fixture_ip, result.stdout)
            result = e.execute(agent, "dig", f"@{resolver}", "denied.test", "+tries=1", "+time=1", transport)
            e.check(f"denied_dns_{resolver}_{transport}", "status: NXDOMAIN" in result.stdout, result.stdout)
    for qtype in ("HTTPS", "SVCB", "AAAA"):
        result = e.execute(agent, "dig", "@127.0.0.11", "allowed.test", qtype, "+short", "+tries=1", "+time=1")
        e.check(f"no_{qtype}_records", not result.stdout.strip(), result.stdout)
    # libnetwork's real resolver port is observable; port-53 interception alone is insufficient.
    listeners = e.execute(agent, "ss", "-H", "-lnut").stdout
    e.results["listeners"] = listeners
    dns_ports = re.findall(r"127\.0\.0\.11:(\d+)", listeners)
    e.check("docker_hidden_dns_listener_discovered", bool(dns_ports), listeners)
    for port in sorted(set(dns_ports)):
        for transport in ("+tcp", "+notcp"):
            result = e.execute(agent, "dig", "@127.0.0.11", "-p", port, "example.com",
                               "+tries=1", "+time=1", transport, check=False)
            e.check(f"docker_hidden_dns_port_{port}_{transport}", result.returncode != 0, result.stdout)
    unix = e.execute(agent, "ss", "-H", "-xl").stdout
    e.check("no_abstract_control_socket", "@" not in unix, unix)
    for container, role in ((keeper, "keeper"), (agent, "agent")):
        status = e.execute(container, "sed", "-n", "/^Uid:/p;/^Cap/p;/^NoNewPrivs/p", "/proc/1/status").stdout
        e.results[f"{role}_privileges"] = status
        e.check(f"{role}_capabilities_dropped", all(int(value, 16) == 0 for value in
                re.findall(r"Cap\w+:\s*([0-9a-f]+)", status)), status)
        e.check(f"{role}_no_new_privileges", "NoNewPrivs:\t1" in status, status)
    result = e.execute(agent, "nft", "list", "ruleset", check=False)
    e.check("agent_cannot_change_firewall", result.returncode != 0, result.stderr)
    result = e.execute(agent, "setpriv", "--reuid=65532", "id", check=False)
    e.check("agent_cannot_assume_proxy_uid", result.returncode != 0, result.stderr)
    node = e.execute(agent, "node", "-e", "const fs=require('fs'); require('https').get('https://allowed.test',"
                     "{ca:fs.readFileSync('/tmp/fixture-ca.pem')},r=>{let b='';r.on('data',x=>b+=x);"
                     "r.on('end',()=>{if(b!=='ok')process.exit(2);console.log(b)})}).on('error',e=>{console.error(e);process.exit(1)})")
    e.check("node_https_without_proxy_configuration", node.stdout.strip() == "ok", node.stderr)
    streaming = json.loads(e.execute(agent, "python3", "/poc/runtime.py", "stream").stdout)
    e.results["sse_stream"] = streaming
    e.check("sse_delivered_incrementally", streaming["total_ms"] - streaming["first_ms"] > 150)
    samples = json.loads(e.execute(agent, "python3", "/poc/runtime.py", "benchmark", str(requests),
                                   "1", "https://allowed.test", timeout=max(60, requests)).stdout)
    e.results["sequential_fresh_tls"] = summary(samples)
    e.check("python_verified_https_sequential", len(samples) == requests)
    counters_before = e.execute(fixture, "nstat", "-asz", "TcpExtListenOverflows", "TcpExtListenDrops").stdout
    samples = json.loads(e.execute(agent, "python3", "/poc/runtime.py", "benchmark", "100", "10",
                                   "https://allowed.test", timeout=90).stdout)
    e.results["fixture_accept_counters_before"] = counters_before
    e.results["fixture_accept_counters_after"] = e.execute(
        fixture, "nstat", "-asz", "TcpExtListenOverflows", "TcpExtListenDrops").stdout
    e.results["concurrent_fresh_tls"] = summary(samples)
    e.check("python_verified_https_concurrent", len(samples) == 100)
    for suffix, target in (("proxied", agent), ("baseline", None)):
        if target is None:
            target = e.container("baseline", network, "idle", [
                "--user", "1000:1000", "--add-host", f"allowed.test:{fixture_ip}",
                "--mount", f"type=bind,src={ca_path},dst=/tmp/fixture-ca.pem,readonly",
            ])
            baseline_samples = json.loads(e.execute(target, "python3", "/poc/runtime.py", "benchmark",
                                                    str(requests), "1", "https://allowed.test:8443",
                                                    timeout=max(60, requests)).stdout)
            e.results["baseline_fresh_tls"] = summary(baseline_samples)
            e.check("baseline_verified_https_sequential", len(baseline_samples) == requests)
            baseline_concurrent = json.loads(e.execute(target, "python3", "/poc/runtime.py", "benchmark",
                                                       "100", "10", "https://allowed.test:8443", timeout=90).stdout)
            e.results["baseline_concurrent_tls"] = summary(baseline_concurrent)
        options = cert if suffix == "proxied" else (*cert, "--connect-to", f"allowed.test:443:{fixture_ip}:8443")
        values = []
        for _ in range(5):
            result = e.curl(target, "https://allowed.test/large", *options, "-o", "/dev/null", "-w", "%{time_total}")
            e.check(f"{suffix}_16MiB_transfer_{len(values)}", result.returncode == 0, result.stderr)
            values.append(float(result.stdout) * 1000)
        e.results[f"{suffix}_16MiB"] = summary(values)
    if live:
        for domain in PUBLIC_HOSTS:
            path, expected = ("/api/mcp/rpc", "405") if domain == "emisar.dev" else ("/v1/models", "401")
            result = e.curl(agent, f"https://{domain}{path}", "-o", "/dev/null", "-w", "%{http_code}")
            e.check(f"public_tls_reachability_{domain}", result.returncode == 0 and result.stdout == expected,
                    f"HTTP {result.stdout}; {result.stderr}")
    ready_samples, pair_samples = [ready_ms], [pair_ms]
    for index in range(1, startup_runs):
        another, child, ready_ms, pair_ms = e.pair(network, policy_dir, ca_path, str(index))
        ready_samples.append(ready_ms)
        pair_samples.append(pair_ms)
        result = e.curl(child, "https://allowed.test", *cert)
        e.check(f"repeated_pair_{index}", result.returncode == 0 and result.stdout == "ok", result.stderr)
        e.remove("container", child)
        e.remove("container", another)
    e.results["keeper_startup"] = summary(ready_samples)
    e.results["pair_startup"] = summary(pair_samples)
    original_rules = e.execute(keeper, "nft", "list", "table", "inet", "coop_poc").stdout
    e.docker("pause", keeper)
    try:
        links = e.execute(agent, "ip", "-o", "link", "show", "dev", "eth0").stdout
        e.results["paused_agent_links"] = links
        e.results["paused_agent_routes"] = e.execute(agent, "ip", "route").stdout
        e.check("paused_gateway_keeps_interface_up", "LOWER_UP" in links)
        e.execute(fixture, "true")
        result = e.curl(agent, f"https://{fixture_ip}:8443", "-k")
        e.check("paused_gateway_firewall_denies_direct_egress", result.returncode == 7 and
                result.stderr.startswith("curl:"), result.stderr)
        result = e.curl(agent, "https://allowed.test", *cert, "--resolve", f"allowed.test:443:{fixture_ip}")
        e.check("paused_gateway_has_no_proxy_fallback", result.returncode == 28 and
                result.stderr.startswith("curl:"), result.stderr)
    finally:
        e.docker("unpause", keeper)
    result = e.curl(agent, "https://allowed.test", *cert)
    e.check("unpaused_gateway_recovers", result.returncode == 0 and result.stdout == "ok", result.stderr)
    started = time.monotonic()
    e.docker("kill", keeper)
    e.execute(agent, "true")
    e.check("agent_survives_keeper_crash", e.docker("inspect", "-f", "{{.State.Running}}", agent).stdout.strip() == "true")
    e.results["post_crash_agent_links"] = e.execute(agent, "ip", "-o", "link").stdout
    e.results["post_crash_agent_routes"] = e.execute(agent, "ip", "route").stdout
    result = e.curl(agent, "https://allowed.test", *cert)
    e.check("gateway_death_removes_egress", result.returncode in (6, 7, 28) and result.stderr.startswith("curl:"), result.stderr)
    result = e.curl(agent, f"https://{fixture_ip}:8443", "-k")
    e.check("keeper_crash_no_direct_fallback", result.returncode in (7, 28) and result.stderr.startswith("curl:"), result.stderr)
    e.results["docker_kill_and_denial_ms"] = round((time.monotonic() - started) * 1000, 3)
    inspector = e.container("inspector", f"container:{agent}", "idle", ["--cap-add", "NET_ADMIN"])
    retained_rules = e.execute(inspector, "nft", "list", "table", "inet", "coop_poc").stdout
    e.check("surviving_namespace_rules_unchanged", retained_rules == original_rules)
    e.results["observed_rules"] = retained_rules


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="coop-egress-poc:experiment")
    parser.add_argument("--output", required=True, type=Path, help="new evidence directory (must not exist)")
    parser.add_argument("--requests", type=int, default=1000)
    parser.add_argument("--startup-runs", type=int, default=5)
    parser.add_argument("--live", action="store_true", help="also probe public provider/Emisar TLS; no credentials")
    args = parser.parse_args()
    if not 1 <= args.requests <= 10000 or not 1 <= args.startup_runs <= 100:
        parser.error("requests must be 1..10000 and startup-runs 1..100")
    args.output = args.output.resolve()
    try:
        args.output.mkdir(parents=True, exist_ok=False)
    except OSError as error:
        parser.error(f"cannot create fresh evidence directory {args.output}: {error.strerror}")
    experiment = Experiment(args.image, args.output)
    success = False
    try:
        run(experiment, args.requests, args.live, args.startup_runs)
        success = True
    except (Exception, KeyboardInterrupt) as error:
        experiment.results["error"] = f"{type(error).__name__}: {error}"
    finally:
        success = experiment.cleanup() and success
        experiment.results["passed"] = success
        (args.output / "report.json").write_text(json.dumps(experiment.results, indent=2) + "\n")
    print(json.dumps({"passed": success, "report": str(args.output / "report.json"),
                      "error": experiment.results.get("error")}))
    return 0 if success else 1


if __name__ == "__main__":
    raise SystemExit(main())
