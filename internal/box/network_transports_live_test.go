//go:build networkruntimee2e

package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// This is the transport half of the release matrix: every combination the
// grammar accepts is either proved enforced here or refused by name. It runs
// against the real daemon and publishes nothing.
func TestRestrictedNetworkTransports(t *testing.T) {
	root := os.Getenv("COOP_NETWORK_TRIAL_STATE")
	if root == "" {
		t.Skip("requires COOP_NETWORK_TRIAL_STATE and a local Docker daemon")
	}
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	build, cancelBuild := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancelBuild()
	docker, err := runtime.BindDocker(build, runtime.Runtime{Name: "docker"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	candidate, err := BuildNetworkCandidate(build, docker, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	clients := qualifiedClients(t, candidate)
	// One neighbour on the default bridge answers UDP and ICMP, so the raw
	// transports are proved against a destination this test fully controls
	// rather than an internet service that may rate-limit or disappear.
	fixture, removeFixture := startTransportFixture(t, docker, candidate.ClientImage)
	// Registered as a defer, not t.Cleanup: cleanups run after this function's
	// deferred docker.Close(), which would leave the neighbour running.
	defer removeFixture()

	t.Run("raw-transports", func(t *testing.T) {
		rules := []egress.Rule{
			{To: egress.Destination{Domain: "*.example.com"}, Protocol: "tls", Ports: []int{443}},
			{To: egress.Destination{IP: fixture.String()}, Protocol: "udp", Ports: []int{9099}},
			{To: egress.Destination{IP: fixture.String()}, Protocol: "icmp", Types: []string{"echo-request"}},
			{To: egress.Destination{IP: "1.1.1.1"}, Protocol: "tcp", Ports: []int{853}},
		}
		script := transportProbe + fmt.Sprintf(`
expect ALLOWED  "tls *.example.com covers www"      "$(tls www.example.com)"
expect REFUSED  "tls apex is not covered"           "$(tls example.com)"
expect REFUSED  "tls lookalike is not covered"      "$(tls notexample.com)"
expect ALLOWED  "raw tcp 1.1.1.1:853"               "$(probe tcp 1.1.1.1 853)"
expect REFUSED  "raw tcp 1.1.1.1:8853"              "$(probe tcp 1.1.1.1 8853)"
expect REFUSED  "raw tcp 8.8.8.8:853"               "$(probe tcp 8.8.8.8 853)"
expect ALLOWED  "raw udp %[1]s:9099"                "$(probe udp %[1]s 9099)"
expect REFUSED  "raw udp %[1]s:9098"                "$(probe udp %[1]s 9098)"
expect ALLOWED  "icmp echo %[1]s"                   "$(probe icmp %[1]s)"
expect REFUSED  "icmp echo 1.1.1.1"                 "$(probe icmp 1.1.1.1)"
exit $failed
`, fixture)
		runTransportCase(t, store, candidate, clients, transportCase{rules: rules, script: script})
	})

	t.Run("protected-beats-granted-cidr", func(t *testing.T) {
		wide, err := hostPrefixAround(fixture)
		if err != nil {
			t.Fatal(err)
		}
		host, err := protectedHostAddress(wide)
		if err != nil {
			t.Skip("no protected host address inside the bridge network on this runtime:", err)
		}
		rules := []egress.Rule{
			{To: egress.Destination{CIDR: wide.String()}, Protocol: "udp", Ports: []int{9099}},
			{To: egress.Destination{CIDR: wide.String()}, Protocol: "tcp", Ports: []int{9099}},
		}
		script := transportProbe + fmt.Sprintf(`
expect ALLOWED  "udp inside the granted %[2]s"        "$(probe udp %[1]s 9099)"
expect REFUSED  "tcp to the protected host %[3]s"     "$(probe tcp %[3]s 9099)"
expect REFUSED  "udp to the protected host %[3]s"     "$(probe udp %[3]s 9099)"
expect REFUSED  "tcp to metadata 169.254.169.254"     "$(probe tcp 169.254.169.254 80)"
exit $failed
`, fixture, wide, host)
		record := runTransportCase(t, store, candidate, clients, transportCase{rules: rules, script: script})
		if record.Receipt.Snapshot.Counters == nil || record.Receipt.Snapshot.Counters.ProtectedPackets == nil ||
			*record.Receipt.Snapshot.Counters.ProtectedPackets == 0 {
			t.Fatal("a granted CIDR that reached a protected address was not counted as protected")
		}
		if len(record.Receipt.Snapshot.AddressGrants) != 2 {
			t.Fatalf("expected one observation per address grant, got %d", len(record.Receipt.Snapshot.AddressGrants))
		}
	})

	t.Run("serve-port", func(t *testing.T) {
		port := 8000
		rules := []egress.Rule{{To: egress.Destination{Domain: "example.com"}, Protocol: "tls", Ports: []int{443}}}
		// The server is bounded by `timeout`, whose 124 exit is the intended
		// end of the case, not a failure of the box.
		script := fmt.Sprintf(`set -eu
mkdir -p /tmp/s && printf 'served-from-the-box' > /tmp/s/index.html
timeout 25 python3 -m http.server %d --bind 0.0.0.0 --directory /tmp/s || test $? -eq 124
`, port)
		reached := make(chan string, 1)
		runTransportCase(t, store, candidate, clients, transportCase{rules: rules, script: script, serve: port, whileRunning: func(repo string) {
			host := project.HostPort(repo, port)
			deadline := time.Now().Add(25 * time.Second)
			for time.Now().Before(deadline) {
				conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", host), time.Second)
				if err != nil {
					time.Sleep(250 * time.Millisecond)
					continue
				}
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				fmt.Fprintf(conn, "GET /index.html HTTP/1.0\r\nHost: localhost\r\n\r\n")
				body, _ := io.ReadAll(conn)
				_ = conn.Close()
				reached <- string(body)
				return
			}
			reached <- ""
		}})
		if body := <-reached; !strings.Contains(body, "served-from-the-box") {
			t.Fatalf("the published serve port did not reach the box: %q", body)
		}
	})

	t.Run("compose-service", func(t *testing.T) {
		compose := fmt.Sprintf(`services:
  web:
    image: %[1]s
    entrypoint: ["sh", "-c", "mkdir -p /tmp/d && echo the-web-sidecar > /tmp/d/index.html && exec python3 -m http.server 80 --bind 0.0.0.0 --directory /tmp/d"]
  other:
    image: %[1]s
    entrypoint: ["sh", "-c", "mkdir -p /tmp/d && echo the-other-sidecar > /tmp/d/index.html && exec python3 -m http.server 80 --bind 0.0.0.0 --directory /tmp/d"]
`, candidate.ClientImage)
		rules := []egress.Rule{{To: egress.Destination{Service: "web"}, Protocol: "tcp", Ports: []int{80}}}
		script := transportProbe + `
expect ALLOWED  "http://web/ is the approved sidecar"  "$(http web)"
expect REFUSED  "http://other/ is a neighbour"         "$(http other)"
exit $failed
`
		runTransportCase(t, store, candidate, clients, transportCase{rules: rules, script: script, compose: compose})
	})
}

func qualifiedClients(t *testing.T, candidate networkstate.CandidateSpec) []networkstate.QualifiedClient {
	t.Helper()
	closure, err := agents.LockedClientClosure(agents.ClientPlatform{OS: candidate.Runtime.OS, Architecture: candidate.Runtime.Architecture, Libc: candidate.Libc})
	if err != nil {
		t.Fatal(err)
	}
	var clients []networkstate.QualifiedClient
	for _, client := range closure.Clients {
		clients = append(clients, networkstate.QualifiedClient{Provider: client.Provider, Client: client.Client, Version: client.Version})
	}
	return clients
}

// transportProbe is the in-box harness: one exit code per case, so a failure
// names the transport rather than dumping a wall of curl output.
const transportProbe = `set -u
failed=0
expect() {
  if [ "$3" = "$1" ]; then printf 'ok   %-42s %s\n' "$2" "$3"
  else printf 'FAIL %-42s want %s got %s\n' "$2" "$1" "$3"; failed=1; fi
}
tls() { curl -q --proxy '' --noproxy '*' -sS --max-time 12 -o /dev/null "https://$1" 2>/dev/null && echo ALLOWED || echo REFUSED; }
http() { curl -q --proxy '' --noproxy '*' -sS --max-time 8 -o /dev/null "http://$1/" 2>/dev/null && echo ALLOWED || echo REFUSED; }
probe() { python3 -c '
import socket, struct, sys
kind, host = sys.argv[1], sys.argv[2]
port = int(sys.argv[3]) if len(sys.argv) > 3 else 0
try:
    if kind == "tcp":
        s = socket.socket(); s.settimeout(5); s.connect((host, port))
    elif kind == "udp":
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(5)
        s.sendto(b"coop", (host, port))
        assert s.recvfrom(64)[0] == b"coop"
    else:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM, socket.IPPROTO_ICMP); s.settimeout(5)
        s.sendto(struct.pack("!BBHHH", 8, 0, 0, 0, 1) + b"coop-echo", (host, 0))
        s.recvfrom(128)
    print("ALLOWED")
except Exception:
    print("REFUSED")
' "$@"; }
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
`

type transportCase struct {
	rules        []egress.Rule
	script       string
	serve        int    // the published container port, 0 for none
	compose      string // the .agent/compose.yml document, "" for none
	whileRunning func(repo string)
}

// runTransportCase drives one real filtered run through the private smoke seam
// and returns its sealed record. A compose document makes the run a services
// case; whileRunning runs on the host while the box is alive.
func runTransportCase(t *testing.T, store *networkstate.Store, candidate networkstate.CandidateSpec,
	clients []networkstate.QualifiedClient, c transportCase) networkstate.Execution {
	t.Helper()
	smoke, err := store.BeginQualification(candidate, clients)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if c.serve != 0 {
		policy := fmt.Sprintf("serve:\n  ports: [%d]\n", c.serve)
		if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte(policy), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if c.compose != "" {
		if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"), []byte(c.compose), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "filtered", Memory: "256m", Pids: "128", CPUs: "1", NoNewPrivileges: true}
	mode := egress.Filtered
	policy, err := store.Admit(repo, networkstate.Admission{InvocationMode: &mode,
		Operator: []egress.Input{{Origin: egress.Origin{Kind: "operator"}, Rules: c.rules}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var registered networkstate.Execution
	permit := &networkSmokeLaunch{authority: smoke, registered: func(r networkstate.Execution) { registered = r }}
	var output bytes.Buffer
	spec := RunSpec{Repo: repo, Workdir: "/workspace", Batch: true, Quiet: true, Ctx: ctx, Stdout: &output, Stderr: &output,
		CapturedEgress: &CapturedEgress{Store: store, Project: repo, Fingerprint: policy.Fingerprint}, Cmd: []string{"sh", "-c", c.script}}
	if c.serve != 0 {
		spec.Serve = true
	}
	if c.compose != "" {
		defer func() {
			if err := DownServicesFile(runtime.Runtime{Name: "docker"}, repo, ComposeFileAt(repo, ".agent/compose.yml"), true, io.Discard, io.Discard, repo); err != nil {
				t.Errorf("compose fixture cleanup failed: %v", err)
			}
		}()
	}
	if c.whileRunning != nil {
		spec.OnRuntimeLaunch = func() { go c.whileRunning(repo) }
	}
	code, runErr := runWithNetworkSmoke(cfg, runtime.Runtime{Name: "docker"}, spec, defaultCompositionArtifactOps(), permit)
	t.Logf("case output:\n%s", output.String())
	if runErr != nil || code != 0 {
		t.Fatal("transport fixture failed", code, runErr)
	}
	record, err := store.Execution(registered.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Receipt == nil || record.Receipt.Finality != "final" {
		t.Fatal("transport case sealed no final receipt")
	}
	for _, resource := range record.Resources {
		if resource.State != "gone" {
			t.Fatalf("resource cleanup pending: %s", resource.Role)
		}
	}
	return record
}

// startTransportFixture runs one exactly-owned UDP echo neighbour on the
// default bridge and returns its address.
func startTransportFixture(t *testing.T, docker *runtime.Docker, image string) (netip.Addr, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ref := runtime.DockerRef{Name: "coop-net-e2e-echo", Labels: map[string]string{"coop.network.test": "transports"}}
	// A leftover from an interrupted run owns the name; remove it first so the
	// case starts from a fresh, exactly-owned intention.
	_ = docker.RemoveContainer(ctx, ref)
	id, err := docker.CreateContainer(ctx, runtime.DockerCreate{Ref: ref, Image: image,
		Options: []string{"--network", "bridge", "--entrypoint", "socat", "--user", "0:0"},
		Command: []string{"-T60", "UDP-RECVFROM:9099,fork", "EXEC:/bin/cat"}})
	if err != nil {
		t.Fatal(err)
	}
	ref.ID = id
	if err := docker.StartContainer(ctx, ref); err != nil {
		t.Fatal(err)
	}
	remove := func() {
		stop, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		if err := docker.RemoveContainer(stop, ref); err != nil {
			t.Errorf("transport fixture cleanup failed: %v", err)
		}
	}
	members, err := docker.NetworkMembers(ctx, "bridge")
	if err != nil {
		remove()
		t.Fatal(err)
	}
	address, ok := members[id]
	if !ok {
		remove()
		t.Fatal("the transport fixture has no IPv4 address on the bridge network")
	}
	return address, remove
}

// hostPrefixAround returns the bridge network's own prefix, so the CIDR case
// grants a range that provably contains a protected host address.
func hostPrefixAround(address netip.Addr) (netip.Prefix, error) {
	prefix, err := address.Prefix(24)
	if err != nil {
		return netip.Prefix{}, err
	}
	return prefix, nil
}

func protectedHostAddress(within netip.Prefix) (netip.Addr, error) {
	protected, err := filteredHostAddresses()
	if err != nil {
		return netip.Addr{}, err
	}
	for _, prefix := range protected {
		if prefix.Addr().Is4() && within.Contains(prefix.Addr()) {
			return prefix.Addr(), nil
		}
	}
	return netip.Addr{}, errors.New("no host address inside " + within.String())
}
