"""Container-only services for the opt-in egress experiment; no host credentials."""

import http.server
import json
import os
from pathlib import Path
import signal
import socket
import ssl
import subprocess
import sys
import time


def keeper():
    subprocess.run(["nft", "-f", "/policy/firewall.nft"], check=True)
    os.execvp("setpriv", [
        "setpriv", "--reuid=65532", "--regid=65532", "--clear-groups",
        "--bounding-set=-all", "--inh-caps=-all", "--ambient-caps=-all",
        "--no-new-privs", "python3", __file__, "services",
    ])


def services():
    processes = [
        subprocess.Popen(["envoy", "-c", "/policy/envoy.json", "--disable-hot-restart",
                          "--concurrency", "1", "--log-level", "warning"]),
        subprocess.Popen(["dnsmasq", "--keep-in-foreground", "--conf-file=/policy/dnsmasq.conf"]),
    ]
    try:
        deadline = time.monotonic() + 10
        while True:
            if any(p.poll() is not None for p in processes):
                raise RuntimeError("gateway service exited")
            try:
                for port in (15001, 1053):
                    with socket.create_connection(("127.0.0.1", port), timeout=.1):
                        pass
                break
            except OSError:
                if time.monotonic() >= deadline:
                    raise RuntimeError("gateway readiness timed out")
                time.sleep(.02)
        Path("/tmp/ready").touch()
        while all(p.poll() is None for p in processes):
            time.sleep(.05)
        raise RuntimeError("gateway service exited; no restart or direct fallback")
    finally:
        for p in processes:
            if p.poll() is None:
                p.terminate()
        for p in processes:
            try:
                p.wait(timeout=2)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait()


class Fixture(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        if self.path == "/stream":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", "320")
            self.end_headers()
            for _ in range(32):
                self.wfile.write(b"data: ok\n\n")
                self.wfile.flush()
                time.sleep(.01)
            return
        size = 16 * 1024 * 1024 if self.path == "/large" else 2
        self.send_response(200)
        self.send_header("Content-Length", str(size))
        self.end_headers()
        if size == 2:
            self.wfile.write(b"ok")
        else:
            for _ in range(size // 65536):
                self.wfile.write(b"x" * 65536)

    def log_message(self, *_args):
        pass


class FixtureServer(http.server.ThreadingHTTPServer):
    # The default backlog of five caused SYN retries in the ten-client benchmark.
    request_queue_size = 128


def fixture():
    subprocess.run([
        "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
        "-keyout", "/tmp/key.pem", "-out", "/tmp/cert.pem", "-days", "1",
        "-subj", "/CN=allowed.test", "-addext", "subjectAltName=DNS:allowed.test",
    ], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain("/tmp/cert.pem", "/tmp/key.pem")
    server = FixtureServer(("0.0.0.0", 8443), Fixture)
    server.socket = context.wrap_socket(server.socket, server_side=True)
    Path("/tmp/ready").touch()
    server.serve_forever()


def benchmark():
    """Each request uses normal DNS and a fresh verified TLS connection."""
    import concurrent.futures
    import urllib.request
    context = ssl.create_default_context(cafile="/tmp/fixture-ca.pem")
    count, concurrency = map(int, sys.argv[2:4])
    url = sys.argv[4]

    def request(_):
        started = time.monotonic()
        with urllib.request.urlopen(url, context=context, timeout=5) as response:
            data = response.read()
            if data != b"ok":
                raise RuntimeError("incorrect fixture response")
        return (time.monotonic() - started) * 1000

    with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
        samples = list(pool.map(request, range(count)))
    print(json.dumps(samples))


def stream():
    import urllib.request
    context = ssl.create_default_context(cafile="/tmp/fixture-ca.pem")
    started = time.monotonic()
    with urllib.request.urlopen("https://allowed.test/stream", context=context, timeout=5) as response:
        first = response.read(10)
        first_ms = (time.monotonic() - started) * 1000
        rest = response.read()
    if first + rest != b"data: ok\n\n" * 32:
        raise RuntimeError("truncated or incorrect stream")
    print(json.dumps({"first_ms": first_ms, "total_ms": (time.monotonic() - started) * 1000}))


if __name__ == "__main__":
    commands = {"keeper": keeper, "services": services, "fixture": fixture,
                "benchmark": benchmark, "stream": stream, "idle": lambda: signal.pause()}
    commands[sys.argv[1]]()
