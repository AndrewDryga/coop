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
	"slices"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
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

	// A sibling container is not a destination a rule may name: the runtime's own
	// subnets are protected, so the only reachable container is an APPROVED
	// sidecar, whose one address is permitted before that drop. Raw UDP is
	// therefore proved through the sidecar, and ICMP against a public address.
	t.Run("raw-transports", func(t *testing.T) {
		rules := []egress.Rule{
			{To: egress.Destination{Domain: "*.example.com"}, Protocol: "tls", Ports: []int{443}},
			{To: egress.Destination{Service: "echo"}, Protocol: "udp", Ports: []int{9099}},
			{To: egress.Destination{IP: fixture.String()}, Protocol: "udp", Ports: []int{9099}},
			{To: egress.Destination{IP: "1.1.1.1"}, Protocol: "icmp", Types: []string{"echo-request"}},
			{To: egress.Destination{IP: "1.1.1.1"}, Protocol: "tcp", Ports: []int{853}},
		}
		script := transportProbe + fmt.Sprintf(`
expect ALLOWED  "tls *.example.com covers www"      "$(tls www.example.com)"
expect REFUSED  "tls apex is not covered"           "$(tls example.com)"
expect REFUSED  "tls lookalike is not covered"      "$(tls notexample.com)"
expect ALLOWED  "raw tcp 1.1.1.1:853"               "$(probe tcp 1.1.1.1 853)"
expect REFUSED  "raw tcp 1.1.1.1:8853"              "$(probe tcp 1.1.1.1 8853)"
expect REFUSED  "raw tcp 8.8.8.8:853"               "$(probe tcp 8.8.8.8 853)"
expect ALLOWED  "raw udp to the approved sidecar"   "$(probe udp echo 9099)"
expect REFUSED  "raw udp to the sidecar on 9098"    "$(probe udp echo 9098)"
expect REFUSED  "raw udp to a granted sibling %[1]s" "$(probe udp %[1]s 9099)"
expect ALLOWED  "icmp echo 1.1.1.1"                 "$(probe icmp 1.1.1.1)"
expect REFUSED  "icmp echo 8.8.8.8"                 "$(probe icmp 8.8.8.8)"
exit $failed
`, fixture)
		runTransportCase(t, store, candidate, clients, transportCase{rules: rules, script: script,
			compose: transportEchoCompose(candidate.ClientImage)})
	})

	// TLS is not only 443. The upstream port is the kernel's redirect record, so
	// a name granted on 853 reaches DNS-over-TLS, the SAME name on 443 is refused
	// even though the name is approved, and a direct dial at the guard's own
	// listener — which declares no port at all — is refused and counted.
	// cloudflare-dns.com is deliberately NOT the fixture here: it is the
	// gateway's own pinned maintenance resolver name, and this case must not be
	// able to pass on the guard's traffic. A container on the bridge cannot
	// stand in for a public endpoint either: a lease must be a public address.
	t.Run("tls-ports", func(t *testing.T) {
		rules := []egress.Rule{
			{To: egress.Destination{Domain: "dns.google"}, Protocol: "tls", Ports: []int{853, 8443}},
			{To: egress.Destination{Domain: "example.com"}, Protocol: "tls", Ports: []int{443}},
		}
		script := transportProbe + `
expect ALLOWED  "tls dns.google:853 with visible SNI"  "$(handshake dns.google 853)"
expect REFUSED  "the same name on 443"                 "$(handshake dns.google 443)"
expect ALLOWED  "example.com on its granted 443"       "$(tls example.com)"
expect REFUSED  "example.com on the captured 853"      "$(handshake example.com 853)"
expect REFUSED  "a direct dial at the guard listener"  "$(handshake dns.google 15443 127.0.0.1)"
exit $failed
`
		record := runTransportCase(t, store, candidate, clients, transportCase{rules: rules, script: script})
		snapshot := record.Receipt.Snapshot
		if !slices.ContainsFunc(snapshot.Connections, func(c networkview.Connection) bool {
			return c.Name == "dns.google" && c.NameSource == "sni" && c.Transport == "tls" && strings.HasSuffix(c.Peer, ":853") &&
				c.SentBytes != nil && *c.SentBytes > 0 && c.ReceivedBytes != nil && *c.ReceivedBytes > 0
		}) {
			t.Fatalf("no measured TLS connection on the granted non-443 port: %+v", snapshot.Connections)
		}
		// Every refusal is counted with the port the kernel recorded, including
		// the direct dial, whose port is the guard's own listener.
		for _, want := range []struct {
			name, reason string
			port         int
		}{{"dns.google", "unapproved_name", 443}, {"example.com", "unapproved_name", 853}, {"", "tls_direct_dial_refused", 15443}} {
			if !slices.ContainsFunc(snapshot.Denials, func(d networkview.Denial) bool {
				return d.Source == "guard" && d.Kind == "tls_denied" && d.Basis == "observed" && d.Name == want.name &&
					d.Reason == want.reason && d.Port != nil && *d.Port == want.port
			}) {
				t.Fatalf("missing counted %s refusal for %q on port %d: %+v", want.reason, want.name, want.port, snapshot.Denials)
			}
		}
	})

	t.Run("protected-beats-granted-cidr", func(t *testing.T) {
		wide, err := hostPrefixAround(fixture)
		if err != nil {
			t.Fatal(err)
		}
		host, err := protectedRuntimeAddress(t, docker, wide)
		if err != nil {
			t.Skip("no protected address inside the bridge network on this runtime:", err)
		}
		rules := []egress.Rule{
			{To: egress.Destination{CIDR: wide.String()}, Protocol: "udp", Ports: []int{9099}},
			{To: egress.Destination{CIDR: wide.String()}, Protocol: "tcp", Ports: []int{9099}},
		}
		script := transportProbe + fmt.Sprintf(`
expect REFUSED  "udp to the sibling %[1]s in granted %[2]s" "$(probe udp %[1]s 9099)"
expect REFUSED  "tcp to the protected address %[3]s"  "$(probe tcp %[3]s 9099)"
expect REFUSED  "udp to the protected address %[3]s"  "$(probe udp %[3]s 9099)"
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
timeout 45 python3 -m http.server %d --bind 0.0.0.0 --directory /tmp/s || test $? -eq 124
`, port)
		reached := make(chan string, 1)
		sibling := make(chan siblingProbe, 1)
		runTransportCase(t, store, candidate, clients, transportCase{rules: rules, script: script, serve: port, whileRunning: func(repo string) {
			// The host proof runs FIRST: the box's server is bounded, and the
			// sibling probe below is allowed to spend that bound waiting.
			defer func() {
				// A published port is HOST ingress. A sibling container on the same
				// bridge is not the host, and must not reach it.
				sibling <- siblingReachesServePort(docker, port)
			}()
			host := project.HostPort(repo, port)
			deadline := time.Now().Add(30 * time.Second)
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
				// The runtime's forwarder accepts on the host side before the
				// box's own server is listening, so an empty answer is "not yet",
				// not "refused". Only a body settles this.
				if len(body) != 0 {
					reached <- string(body)
					return
				}
				time.Sleep(250 * time.Millisecond)
			}
			reached <- ""
		}})
		if body := <-reached; !strings.Contains(body, "served-from-the-box") {
			t.Fatalf("the published serve port did not reach the box: %q", body)
		}
		probe := <-sibling
		if probe.err != nil {
			t.Fatalf("the sibling probe could not run, so it proved nothing: %v", probe.err)
		}
		if probe.reached {
			t.Fatal("a sibling container on the bridge reached the box's published serve port")
		}
		t.Logf("sibling container at %s was refused by the box's ingress rule: %s", probe.from, probe.detail)
	})

	// The agent's OWN loopback is not a host surface: a test server on
	// 127.0.0.1 is ordinary work, not an attempt on a protected address.
	t.Run("agent-localhost", func(t *testing.T) {
		rules := []egress.Rule{{To: egress.Destination{Domain: "example.com"}, Protocol: "tls", Ports: []int{443}}}
		script := transportProbe + `
mkdir -p /tmp/s && printf 'served-to-itself' > /tmp/s/index.html
(cd /tmp/s && timeout 20 python3 -m http.server 3000 --bind 127.0.0.1 >/dev/null 2>&1 &)
for i in 1 2 3 4 5 6 7 8 9 10; do curl -q -sS --max-time 1 -o /dev/null http://127.0.0.1:3000/index.html && break; sleep 1; done
expect ALLOWED  "the box reaches its own 127.0.0.1:3000"  "$(http 127.0.0.1:3000/index.html)"
expect ALLOWED  "the box still reaches an allowed name"   "$(tls example.com)"
exit $failed
`
		record := runTransportCase(t, store, candidate, clients, transportCase{rules: rules, script: script})
		if counters := record.Receipt.Snapshot.Counters; counters == nil || counters.ProtectedPackets == nil || *counters.ProtectedPackets != 0 {
			t.Fatalf("the box's own loopback was counted as an attempt on a protected address: %+v", record.Receipt.Snapshot.Counters)
		}
		for _, alert := range record.Receipt.Snapshot.Alerts {
			if alert.Category == "protected_destination" {
				t.Fatalf("a localhost server raised a protected-destination alert: %+v", alert)
			}
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
# One TLS handshake with visible SNI on ANY port: $1 is the name, $2 the port,
# and $3 an optional address to dial instead of the name (a direct dial).
handshake() { timeout 15 openssl s_client -connect "${3:-$1}:$2" -servername "$1" -verify_return_error -brief </dev/null >/dev/null 2>&1 && echo ALLOWED || echo REFUSED; }
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

// transportEchoCompose is the approved-sidecar half of the raw transport
// proof: the same UDP echo the neighbour runs, declared as a Compose service so
// a `service:` grant can name it.
func transportEchoCompose(image string) string {
	return fmt.Sprintf(`services:
  echo:
    image: %s
    entrypoint: ["socat", "-T60", "UDP-RECVFROM:9099,fork", "EXEC:/bin/cat"]
    user: "0:0"
`, image)
}

// siblingProbe is what another container on the bridge could do to the box's
// published port. err means the probe itself could not run — which proves
// nothing and must fail the case rather than read as a refusal.
type siblingProbe struct {
	reached bool
	from    string
	detail  string
	err     error
}

// siblingReachesServePort tries the box's published container port from the
// neighbour container this suite already owns. The host reaches that port
// through the bridge gateway; a sibling is not the gateway, so its packets meet
// the protected drop instead.
func siblingReachesServePort(docker *runtime.Docker, port int) siblingProbe {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	controllers, err := runtime.Runtime{Name: "docker"}.ContainersByLabel(ctx, "coop.network.role", "controller")
	if err != nil {
		return siblingProbe{err: fmt.Errorf("controller listing: %w", err)}
	}
	if len(controllers) != 1 {
		return siblingProbe{err: fmt.Errorf("expected one live controller, got %d", len(controllers))}
	}
	members, err := docker.NetworkMembers(ctx, "bridge")
	if err != nil {
		return siblingProbe{err: fmt.Errorf("bridge members: %w", err)}
	}
	// A listing gives short ids; the network's own inventory is keyed by full
	// ones, so the two are related by prefix.
	address := netip.Addr{}
	for id, member := range members {
		if strings.HasPrefix(id, controllers[0].ID) {
			address = member
		}
	}
	if !address.IsValid() {
		return siblingProbe{err: errors.New("the controller has no bridge address")}
	}
	fixture := runtime.DockerRef{Name: "coop-net-e2e-echo", Labels: map[string]string{"coop.network.test": "transports"}}
	value, present, err := docker.InspectContainer(ctx, fixture)
	if err != nil || !present || !value.State.Running {
		return siblingProbe{err: errors.Join(errors.New("the neighbour container is not available to probe from"), err)}
	}
	fixture.ID = value.ID
	// A working curl in the same container proves the probe itself is sound: the
	// sidecar's own loopback answers nothing, so 7 (connection refused) is the
	// shape of "curl ran". Anything else here is a harness failure.
	if _, err := docker.ExecRead(ctx, fixture, 4096, "curl", "-q", "--proxy", "", "-sS", "--max-time", "5",
		"-o", "/dev/null", "http://127.0.0.1:1/"); err == nil {
		return siblingProbe{err: errors.New("the neighbour's control probe unexpectedly connected")}
	}
	data, err := docker.ExecRead(ctx, fixture, 4096, "curl", "-q", "--proxy", "", "-sS", "--max-time", "5",
		"-o", "/dev/null", "-w", "%{http_code}", fmt.Sprintf("http://%s:%d/index.html", address, port))
	return siblingProbe{reached: err == nil, from: address.String(), detail: strings.TrimSpace(string(data)) + fmt.Sprintf(" (%v)", err)}
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

// protectedRuntimeAddress finds one address inside within that a launch would
// protect — a host interface, or the runtime's own gateway in that subnet. It
// uses the SAME inventory the launch takes, so the case proves the envelope the
// box actually enforces rather than a second guess at it.
func protectedRuntimeAddress(t *testing.T, docker *runtime.Docker, within netip.Prefix) (netip.Addr, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	networks, err := docker.Networks(ctx)
	if err != nil {
		return netip.Addr{}, err
	}
	protected, _, err := filteredProtectedAddresses(networks, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	for _, prefix := range protected {
		if prefix.Addr().Is4() && prefix.Bits() == prefix.Addr().BitLen() && within.Contains(prefix.Addr()) {
			return prefix.Addr(), nil
		}
	}
	return netip.Addr{}, errors.New("no protected address inside " + within.String())
}
