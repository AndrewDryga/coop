"""Host-only policy tests; real Docker experiments are explicitly opt-in."""

import contextlib
import io
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import egress_poc as poc


class PolicyTests(unittest.TestCase):
    def test_fixture_accept_queue_exceeds_benchmark_concurrency(self):
        source = Path(__file__).parent / "egress-poc" / "runtime.py"
        spec = importlib.util.spec_from_file_location("egress_fixture", source)
        runtime = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(runtime)
        self.assertGreaterEqual(runtime.FixtureServer.request_queue_size, 10)

    def test_exact_names_only(self):
        self.assertEqual(poc.hostname("api.openai.com"), "api.openai.com")
        for name in ("*.example.com", "example.com.", "API.EXAMPLE.COM", "localhost",
                     "http://example.com", "a..com", "-a.com", "a-.com", "127.0.0.1",
                     "a\n.com", "a/com", "a" * 64 + ".com"):
            with self.subTest(name=name), self.assertRaises(ValueError):
                poc.hostname(name)

    def test_public_unicast_ipv4_only(self):
        self.assertEqual(poc.public_ipv4("1.1.1.1"), "1.1.1.1")
        for value in ("127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1",
                      "192.168.0.1", "224.0.0.1", "0.0.0.0", "::1", "8.8.8.8/32"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                poc.public_ipv4(value)

    def test_mixed_dns_answer_rejected(self):
        rows = [(None, None, None, None, (ip, 443)) for ip in ("1.1.1.1", "10.0.0.1")]
        with mock.patch.object(poc.socket, "getaddrinfo", return_value=rows):
            with self.assertRaises(ValueError):
                poc.snapshot(["allowed.example"])

    def test_no_dynamic_upstream_or_control_listener(self):
        config = poc.configuration({"allowed.example": [("1.1.1.1", 443)]})
        envoy = json.loads(config["envoy.json"])
        self.assertNotIn("admin", envoy)
        listener = envoy["static_resources"]["listeners"][0]
        self.assertNotIn("default_filter_chain", listener)
        self.assertEqual(listener["filter_chains"][0]["filter_chain_match"],
                         {"server_names": ["allowed.example"], "transport_protocol": "tls"})
        self.assertEqual(envoy["static_resources"]["clusters"][0]["type"], "STATIC")
        self.assertIn("no-resolv\n", config["dnsmasq.conf"])
        self.assertIn("local=/#/\n", config["dnsmasq.conf"])
        self.assertNotIn("server=", config["dnsmasq.conf"])

    def test_namespace_firewall_is_deny_by_default_and_preempts_docker(self):
        rules = poc.configuration({"allowed.example": [("1.1.1.1", 443)]})["firewall.nft"]
        self.assertIn("table inet coop_poc", rules)
        self.assertIn("priority -110", rules)
        self.assertEqual(rules.count("policy drop"), 2)
        self.assertNotIn("flush ruleset", rules)
        self.assertNotIn('oifname "lo" accept', rules)
        self.assertNotIn("oifname", rules)
        self.assertIn("ip daddr 127.0.0.1 tcp dport { 15001, 1053 } accept", rules)
        self.assertNotIn("meta skuid 65532 accept", rules)
        self.assertIn("meta skuid 65532 ip daddr 1.1.1.1 tcp dport 443 accept", rules)
        for rule in ("tcp dport 443 redirect to :15001", "udp dport 53 redirect to :1053",
                     "tcp dport 53 redirect to :1053", "reject with icmpx type admin-prohibited"):
            self.assertIn(rule, rules)

    def test_invalid_upstreams_rejected(self):
        for endpoint in (("::1", 443), ("1.1.1.1", 0), ("1.1.1.1", 70000)):
            with self.subTest(endpoint=endpoint), self.assertRaises(ValueError):
                poc.configuration({"allowed.example": [endpoint]})

    def test_empty_policy_rejected(self):
        with self.assertRaises(ValueError):
            poc.configuration({})

    def test_cleanup_refuses_unknown_identifier(self):
        e = poc.Experiment("image", None)
        with mock.patch.object(e, "docker") as docker:
            with self.assertRaises(RuntimeError):
                e.remove("container", "a" * 64)
            docker.assert_not_called()

    def test_cleanup_refuses_changed_ownership(self):
        e = poc.Experiment("image", None)
        identifier = "a" * 64
        e.resources.append(("container", identifier))
        with mock.patch.object(e, "docker", return_value=mock.Mock(stdout="another-run\n")) as docker:
            with self.assertRaisesRegex(RuntimeError, "ownership label"):
                e.remove("container", identifier)
            self.assertEqual(docker.call_count, 1)
            self.assertEqual(docker.call_args.args[:2], ("container", "inspect"))
        self.assertEqual(e.resources, [("container", identifier)])

    def test_fixture_ca_is_readonly_mount_not_docker_cp(self):
        e = poc.Experiment("image", None)
        with mock.patch.object(e, "container", side_effect=["keeper", "agent"]) as create:
            with mock.patch.object(e, "ready"):
                e.pair("network", "/policy", "/fixture-ca.pem", "0")
        self.assertIn("type=bind,src=/fixture-ca.pem,dst=/tmp/fixture-ca.pem,readonly",
                      create.call_args.args[3])

    def test_existing_evidence_directory_is_usage_error_not_traceback(self):
        with tempfile.TemporaryDirectory() as directory:
            stderr = io.StringIO()
            with mock.patch("sys.argv", ["egress_poc.py", "--output", directory]):
                with contextlib.redirect_stderr(stderr), self.assertRaises(SystemExit) as raised:
                    poc.main()
            self.assertEqual(raised.exception.code, 2)
            self.assertIn("fresh evidence directory", stderr.getvalue())
            self.assertNotIn("Traceback", stderr.getvalue())
            self.assertEqual(list(Path(directory).iterdir()), [])


if __name__ == "__main__":
    unittest.main()
