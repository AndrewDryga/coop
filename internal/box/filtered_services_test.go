package box

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// A rule this runtime cannot enforce must fail the launch by name, before any
// approval is read or written. Silently dropping the constraint — or accepting
// it and letting the gateway ignore it — is the one outcome the design forbids.
func TestUnsupportedRequestsAreRefusedBeforeAdmission(t *testing.T) {
	cases := map[string]struct {
		rule egress.Rule
		want string
	}{
		"tls on the DNS port": {egress.Rule{To: egress.Destination{Domain: "api.example.com"}, Protocol: "tls", Ports: []int{53}},
			"TLS on port 53 is not allowed here"},
		"ipv6 destination": {egress.Rule{To: egress.Destination{IP: "2606:4700:4700::1111"}, Protocol: "tcp", Ports: []int{5432}},
			"IPv6 destinations are not supported yet"},
		"icmpv6": {egress.Rule{To: egress.Destination{CIDR: "2001:db8::/32"}, Protocol: "icmpv6", Types: []string{"echo-request"}},
			"IPv6 destinations are not supported yet"},
		"captured tcp port": {egress.Rule{To: egress.Destination{IP: "10.0.0.1"}, Protocol: "tcp", Ports: []int{443}},
			"the gateway takes 443 for TLS and DNS"},
		"captured udp port": {egress.Rule{To: egress.Destination{IP: "10.0.0.1"}, Protocol: "udp", Ports: []int{53}},
			"the gateway takes 53 for TLS and DNS"},
		"protected range": {egress.Rule{To: egress.Destination{CIDR: "169.254.0.0/16"}, Protocol: "tcp", Ports: []int{80}},
			"is a protected range"},
		"icmp beyond echo": {egress.Rule{To: egress.Destination{IP: "10.0.0.1"}, Protocol: "icmp", Types: []string{"3"}},
			"only echo-request is"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			project := networkstate.Admission{Requests: []egress.Rule{c.rule}}
			operator := networkstate.Admission{Operator: []egress.Input{{Origin: egress.Origin{Kind: "operator"}, Rules: []egress.Rule{c.rule}}}}
			for label, input := range map[string]networkstate.Admission{"project request": project, "operator grant": operator} {
				err := checkSupportedRequests(input)
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("%s: got %v, want a refusal naming %q", label, err, c.want)
				}
				if !strings.Contains(err.Error(), NetworkRuleText(mustNormalize(t, c.rule))) {
					t.Errorf("%s: the refusal does not name the rule: %v", label, err)
				}
			}
		})
	}
	supported := networkstate.Admission{Requests: []egress.Rule{
		{To: egress.Destination{Domain: "*.example.com"}, Protocol: "tls", Ports: []int{443}},
		{To: egress.Destination{Domain: "dot.example.com"}, Protocol: "tls", Ports: []int{853, 8443}},
		{To: egress.Destination{CIDR: "10.42.9.0/24"}, Protocol: "udp", Ports: []int{123}},
		{To: egress.Destination{Service: "db"}, Protocol: "tcp", Ports: []int{5432}},
	}}
	if err := checkSupportedRequests(supported); err != nil {
		t.Fatal("a supported request set was refused:", err)
	}
	// Two rules that are each fine can clash, whichever sources they come from: the launch checks
	// what the box would get, and coop approve checks the project's own rules the same way.
	tls := egress.Rule{To: egress.Destination{Domain: "example.com"}, Protocol: "tls", Ports: []int{8443}}
	raw := egress.Rule{To: egress.Destination{IP: "1.1.1.1"}, Protocol: "tcp", Ports: []int{8443}}
	for label, input := range map[string]networkstate.Admission{
		"project":             {Requests: []egress.Rule{tls, raw}},
		"project beside flag": {Requests: []egress.Rule{raw}, Operator: []egress.Input{{Origin: egress.Origin{Kind: "operator"}, Rules: []egress.Rule{tls}}}},
	} {
		err := checkSupportedRequests(input)
		if err == nil || err.Error() != "raw tcp to 1.1.1.1/32 on port 8443 clashes with a TLS rule on the same port — the gateway takes that port for TLS; use another port" {
			t.Errorf("%s: a clash = %v", label, err)
		}
	}
}

func mustNormalize(t *testing.T, rule egress.Rule) egress.Rule {
	t.Helper()
	normalized, err := egress.NormalizeRules([]egress.Rule{rule})
	if err != nil || len(normalized) != 1 {
		t.Fatal(err)
	}
	return normalized[0]
}

// The gateway captures DNS 53, TLS 443 and every other port this policy grants
// TLS on, so a project that serves on one of them is told before anything is
// created.
func TestFilteredServePortsCannotCollideWithTheCapture(t *testing.T) {
	if err := checkFilteredServePorts([]int{3000, 8080}, []int{443}); err != nil {
		t.Fatal(err)
	}
	for _, port := range []int{443, 53, 8443} {
		err := checkFilteredServePorts([]int{3000, port}, []int{443, 8443})
		if err == nil || !strings.Contains(err.Error(), "the gateway uses for TLS and DNS") {
			t.Errorf("serve port %d was accepted: %v", port, err)
		}
	}
}

// A serve port publishes on the gateway container, which owns the namespace,
// and the agent still learns its URL.
func TestFilteredPublishTargetsTheGatewayAndKeepsTheURL(t *testing.T) {
	spec := RunSpec{Repo: t.TempDir(), Serve: true, servePorts: []int{3000}}
	options, plan, env := filteredPublish(&config.Config{}, spec, func(int) bool { return true })
	if len(options) != 2 || options[0] != "-p" || !strings.HasSuffix(options[1], ":3000") || !strings.HasPrefix(options[1], "127.0.0.1:") {
		t.Fatalf("publish options bind the wrong interface or port: %v", options)
	}
	if published := publishedServePorts(plan); len(published) != 1 || published[0] != 3000 {
		t.Fatalf("the published port list is wrong: %v", published)
	}
	if len(env) != 2 || !strings.HasPrefix(env[1], "COOP_SERVE_URL_3000=http://localhost:") {
		t.Fatalf("the box was not told its own URL: %v", env)
	}
	// A taken host port skips publication without failing the launch, and the
	// discovery URL still names the workspace's assigned port.
	options, plan, env = filteredPublish(&config.Config{}, spec, func(int) bool { return false })
	if len(options) != 0 || len(publishedServePorts(plan)) != 0 || len(env) != 2 {
		t.Fatalf("a taken host port changed more than the publication: %v %+v %v", options, plan, env)
	}
	if options, plan, env := filteredPublish(&config.Config{}, RunSpec{Repo: t.TempDir()}, func(int) bool { return true }); options != nil || plan != nil || env != nil {
		t.Fatal("a run without serve ports published something")
	}
	if options, plan, env := filteredPublish(&config.Config{}, RunSpec{Repo: t.TempDir(), servePorts: []int{3000}}, func(int) bool {
		t.Fatal("a run that did not request Serve must not inspect host ports")
		return true
	}); options != nil || plan != nil || env != nil {
		t.Fatal("a run without Serve published a configured project port")
	}
}

// A service grant needs this project's Compose file; without one the launch
// says which services it could not resolve rather than dropping the grant.
func TestServiceGrantsWithoutAComposeFileRefuseTheLaunch(t *testing.T) {
	policy := servicePolicy(t, "web", "cache")
	grants := serviceGrants(policy)
	if len(grants) != 2 {
		t.Fatalf("expected both service grants, got %d", len(grants))
	}
	_, _, _, _, err := resolveServiceBindings(context.Background(), nil, runtime.Runtime{Name: "docker"}, RunSpec{Repo: t.TempDir()}, "", nil, grants, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "cache") || !strings.Contains(err.Error(), "web") {
		t.Fatalf("missing Compose file did not name the unresolved services: %v", err)
	}
}

func servicePolicy(t *testing.T, names ...string) egress.Snapshot {
	t.Helper()
	var rules []egress.Rule
	for _, name := range names {
		rules = append(rules, egress.Rule{To: egress.Destination{Service: name}, Protocol: "tcp", Ports: []int{80}})
	}
	policy, err := egress.Compile("test", egress.Filtered,
		[]egress.Input{{Rules: rules, Origin: egress.Origin{Kind: "operator"}}}, nil, false, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

// A `service:` grant is bound to a name, and the repository decides what that
// name runs. The approval carries the digest of the definition a human saw, so
// rewriting the service — into a proxy image with ordinary egress, say — is
// refused at the next launch instead of quietly inheriting the tunnel.
func TestApprovedServiceDefinitionChangeRefusesTheLaunch(t *testing.T) {
	repo := t.TempDir()
	compose := filepath.Join(repo, "compose.yml")
	write := func(image string) {
		body := "services:\n  db:\n    image: " + image + "\n    expose: [5432]\n  web:\n    image: nginx:1\n"
		if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("postgres:16")
	approved, err := composeServiceDigests(compose, repo, false, []string{"db"})
	if err != nil {
		t.Fatal(err)
	}
	approval := &networkstate.Approval{Services: approved}
	if err := checkApprovedServices(approval, compose, repo, false); err != nil {
		t.Fatal("the reviewed definition was refused:", err)
	}
	// An unrelated service may change freely: it is not what was granted.
	if err := os.WriteFile(compose, []byte("services:\n  db:\n    image: postgres:16\n    expose: [5432]\n  web:\n    image: nginx:2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkApprovedServices(approval, compose, repo, false); err != nil {
		t.Fatal("an unrelated service edit tripped the approved service:", err)
	}
	write("socat-proxy:latest")
	err = checkApprovedServices(approval, compose, repo, false)
	if err == nil || !strings.Contains(err.Error(), `Compose service "db" changed since it was approved`) {
		t.Fatalf("a rewritten service kept its grant: %v", err)
	}
	// The same refusal is what a launch gets, before anything is started or even asked of Docker:
	// with no Docker on PATH at all, the launch still names the changed service.
	t.Setenv("PATH", t.TempDir())
	_, _, _, _, launchErr := resolveServiceBindings(context.Background(), nil, runtime.Runtime{Name: "docker"}, RunSpec{Repo: repo}, compose, approval, serviceGrants(servicePolicy(t, "db")), nil, nil)
	if launchErr == nil || !strings.Contains(launchErr.Error(), "changed since it was approved") {
		t.Fatalf("the launch ran a rewritten service: %v", launchErr)
	}
	// A service the file no longer declares at all is named, not skipped.
	if err := os.WriteFile(compose, []byte("services:\n  web:\n    image: nginx:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkApprovedServices(approval, compose, repo, false); err == nil || !strings.Contains(err.Error(), `no service "db"`) {
		t.Fatalf("a removed service was not reported: %v", err)
	}
}

func TestFilteredStartupScopesComposeToGrantedServices(t *testing.T) {
	repo := t.TempDir()
	compose := filepath.Join(repo, "compose.yml")
	body := `services:
  db:
    image: app:1
    depends_on: [cache]
  cache:
    image: redis:8
    depends_on:
      queue:
        condition: service_started
  queue:
    image: queue:1
  admin:
    image: admin:1
`
	if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	digests, err := composeServiceDigests(compose, repo, false, []string{"db"})
	if err != nil {
		t.Fatal(err)
	}
	if len(digests) != 3 || digests["db"] == "" || digests["cache"] == "" || digests["queue"] == "" {
		t.Fatalf("approved service digests = %v, want db and its transitive dependencies", digests)
	}
	id := strings.Repeat("a", 64)
	docker := &filteredDaemonFixture{
		networkMembers:  map[string]netip.Addr{id: netip.MustParseAddr("172.31.0.2")},
		composeServices: map[string]string{"db": id},
		extraNetworks: []runtime.DockerNetwork{{Name: ComposeProject(repo) + "_filtered", Internal: true,
			Subnets: []netip.Prefix{netip.MustParsePrefix("172.31.0.0/16")}, Gateways: []netip.Addr{netip.MustParseAddr("172.31.0.1")}}},
	}
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	_, _, clients, prepared, err := resolveServiceBindings(t.Context(), docker, recorderRuntime(t, recorder), RunSpec{Repo: repo}, compose,
		&networkstate.Approval{Services: digests}, serviceGrants(servicePolicy(t, "db")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(prepared.cleanup)
	gotClients := map[string]netip.Addr{}
	for _, client := range clients {
		gotClients[client.Name] = client.Address
	}
	for _, name := range []string{"db", "cache", "queue"} {
		if !gotClients[name].Is4() {
			t.Fatalf("service proxy clients = %v, want named dependency closure", clients)
		}
	}
	if err := prepared.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	var up string
	for _, call := range strings.Split(string(data), "\n") {
		if strings.Contains(call, " up ") {
			up = call
		}
	}
	if !strings.HasSuffix(up, " up -d --wait db") {
		t.Fatalf("filtered startup was not scoped to the directly granted service: %q", up)
	}
	changed := strings.Replace(body, "redis:8", "redis:9", 1)
	if err := os.WriteFile(compose, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkApprovedServices(&networkstate.Approval{Services: digests}, compose, repo, false); err == nil || !strings.Contains(err.Error(), `service "cache" changed`) {
		t.Fatalf("changed dependency kept the parent service approval: %v", err)
	}
}

func TestFilteredFinalServiceStartRechecksBindsAndNewSecrets(t *testing.T) {
	for _, changed := range []string{"escaped bind", "new secret"} {
		t.Run(changed, func(t *testing.T) {
			repo := t.TempDir()
			shared := filepath.Join(repo, "shared")
			if err := os.Mkdir(shared, 0o700); err != nil {
				t.Fatal(err)
			}
			compose := filepath.Join(repo, "compose.yml")
			data := []byte("services:\n  db:\n    image: postgres:18\n    volumes: [\"./shared:/data:ro\"]\n")
			if err := os.WriteFile(compose, data, 0o644); err != nil {
				t.Fatal(err)
			}
			recorder := filepath.Join(t.TempDir(), "runtime.log")
			prepared := &preparedFilteredServices{runtime: recorderRuntime(t, recorder), repo: repo, file: compose, data: data,
				selected: []string{"db"}}
			if changed == "escaped bind" {
				if err := os.Remove(shared); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/", shared); err != nil {
					t.Fatal(err)
				}
				if err := prepared.start(t.Context()); err == nil || !strings.Contains(err.Error(), "outside the repo") {
					t.Fatalf("escaped bind was launched: %v", err)
				}
				if calls, _ := os.ReadFile(recorder); strings.Contains(string(calls), " up ") {
					t.Fatalf("Compose started after bind escaped: %s", calls)
				}
				return
			}
			if err := os.WriteFile(filepath.Join(shared, ".env"), []byte("TOKEN=private\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := prepared.start(t.Context()); err != nil {
				t.Fatal(err)
			}
			calls, err := os.ReadFile(recorder)
			if err != nil || !strings.Contains(string(calls), "coop-compose-override-shadow.yml") {
				t.Fatalf("new secret was not shadowed at final launch: %v\n%s", err, calls)
			}
		})
	}
}

func TestFilteredServiceOverrideIsInternalAndKeepsPeerTrafficDirect(t *testing.T) {
	services := []string{"app", "db"}
	addresses := map[string]netip.Addr{
		"app": netip.MustParseAddr("192.168.40.16"),
		"db":  netip.MustParseAddr("192.168.40.17"),
	}
	data, err := filteredServiceOverride("coop-example_filtered", services, netip.MustParsePrefix("192.168.40.0/24"), addresses, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"networks: !override",
		"internal: true",
		"HTTPS_PROXY: \"http://coop-gateway:15444\"",
		"https_proxy: \"http://coop-gateway:15444\"",
		"NO_PROXY: \"localhost,127.0.0.1,app,db\"",
		"ipv4_address: 192.168.40.16",
		"subnet: 192.168.40.0/24",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("filtered override missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "admin:") {
		t.Fatal("filtered override changed an unrelated service")
	}
}

func TestFilteredLaunchStartsServicesAfterTheGateway(t *testing.T) {
	f, docker := filteredFixture(t)
	ready := filepath.Join(t.TempDir(), "gateway-ready")
	docker.onStart = func(role string) {
		if role == "guard" {
			if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	runtimePath := filepath.Join(t.TempDir(), "compose")
	script := "#!/bin/sh\n" +
		"case \"$*\" in *'up -d --wait db'*) test -f " + strconv.Quote(ready) + " || exit 41 ;; esac\n"
	if err := os.WriteFile(runtimePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	address := netip.MustParseAddr("172.31.0.16")
	compose := filepath.Join(f.record.Project, "compose.yml")
	data := []byte("services:\n  db:\n    image: postgres:18\n")
	if err := os.WriteFile(compose, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f.servicesNet = ComposeProject(f.record.Project) + "_filtered"
	f.preparedServices = &preparedFilteredServices{runtime: runtime.Runtime{Name: runtimePath}, repo: f.record.Project, file: compose, data: data,
		selected: []string{"db"}, addresses: map[string]netip.Addr{"db": address}, cleanup: func() {}}
	docker.networkMembers = map[string]netip.Addr{strings.Repeat("a", 64): address}
	docker.composeServices = map[string]string{"db": strings.Repeat("a", 64)}
	code, err := f.launch(t.Context(), RunSpec{Repo: f.record.Project}, nil, nil, io.Discard, io.Discard)
	if err != nil || code != 7 {
		t.Fatalf("filtered launch = (%d, %v)", code, err)
	}
	if len(docker.connected) != 1 || !strings.HasSuffix(docker.connected[0], "/"+filteredServiceProxyAlias) {
		t.Fatalf("controller network attachment = %v", docker.connected)
	}
	// The alias is the prepared services' only route to approved TLS, so it exists before the
	// controller runs — even with the guard's creation overlapping the controller's start.
	if attach, start := slices.Index(docker.log, "connect:controller"), slices.Index(docker.log, "start:controller"); attach < 0 || attach > start {
		t.Fatalf("the services network was attached after the controller started: %v", docker.log)
	}
}

func TestApprovedServiceBindsExternalVolumeIdentity(t *testing.T) {
	repo := t.TempDir()
	compose := filepath.Join(repo, "compose.yml")
	body := func(volume string) string {
		return "services:\n  db:\n    image: postgres:18\n    volumes: [customer:/data:ro]\nvolumes:\n  customer:\n    external: true\n    name: " + volume + "\n"
	}
	if err := os.WriteFile(compose, []byte(body("customer-a")), 0o644); err != nil {
		t.Fatal(err)
	}
	approved, err := composeServiceDigests(compose, repo, false, []string{"db"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(compose, []byte(body("customer-b")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkApprovedServices(&networkstate.Approval{Services: approved}, compose, repo, false); err == nil || !strings.Contains(err.Error(), `service "db" changed`) {
		t.Fatalf("external volume retarget kept the service approval: %v", err)
	}
	writable := strings.Replace(body("customer-a"), ":ro]", ":rw]", 1)
	if err := os.WriteFile(compose, []byte(writable), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkApprovedServices(&networkstate.Approval{Services: approved}, compose, repo, false); err == nil {
		t.Fatal("external volume read/write increase kept the service approval")
	}
}

func TestFilteredServiceStartUsesPrivateOwnershipBesideAnotherBox(t *testing.T) {
	repo := t.TempDir()
	compose := filepath.Join(repo, "compose.yml")
	if err := os.WriteFile(compose, []byte("services:\n  db:\n    image: postgres:18\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	digests, err := composeServiceDigests(compose, repo, false, []string{"db"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop, Workspace: repo})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forkspace.EndExecution(repo, other) })

	recorder := filepath.Join(t.TempDir(), "runtime.log")
	owner := "run-two"
	docker := &filteredDaemonFixture{extraNetworks: []runtime.DockerNetwork{{
		Name: ComposeProjectFor(repo, owner) + "_filtered", Internal: true,
		Subnets: []netip.Prefix{netip.MustParsePrefix("172.31.0.0/16")}, Gateways: []netip.Addr{netip.MustParseAddr("172.31.0.1")},
	}}}
	_, _, _, prepared, err := resolveServiceBindings(t.Context(), docker, recorderRuntime(t, recorder), RunSpec{Repo: repo, RunID: owner}, compose,
		&networkstate.Approval{Services: digests}, serviceGrants(servicePolicy(t, "db")), nil, nil)
	if err != nil {
		t.Fatalf("private filtered service preparation beside another box: %v", err)
	}
	t.Cleanup(prepared.cleanup)
	data, readErr := os.ReadFile(recorder)
	if readErr != nil || !strings.Contains(string(data), "compose -p "+ComposeProjectFor(repo, owner)) {
		t.Fatalf("filtered launch did not use its private Compose project: %v\n%s", readErr, data)
	}
}

// A later box of an editor session uses the services its session already runs: it starts nothing, so it
// needs no exclusive lock while the boxes already running hold the shared side, and it cannot recreate
// a service under them. Anything that does not match goes the whole way and stays visible there.
func TestSessionBoxJoinsTheServicesItsSessionRuns(t *testing.T) {
	repo := t.TempDir()
	compose := filepath.Join(repo, "compose.yml")
	body := []byte("services:\n  db:\n    image: app:1\n    depends_on: [cache]\n  cache:\n    image: redis:8\n")
	if err := os.WriteFile(compose, body, 0o644); err != nil {
		t.Fatal(err)
	}
	digests, err := composeServiceDigests(compose, repo, false, []string{"db"})
	if err != nil {
		t.Fatal(err)
	}
	const owner = "supervisor-one"
	session := RunSpec{Repo: repo, ActivitySource: owner, SupervisorID: owner}
	running := serviceDefinition(body, []string{"cache", "db"})
	fixture := func() *filteredDaemonFixture {
		return &filteredDaemonFixture{serviceDefinition: running,
			composeServices: map[string]string{"db": strings.Repeat("a", 64), "cache": strings.Repeat("b", 64)},
			extraNetworks: []runtime.DockerNetwork{{Name: ComposeProjectFor(repo, owner) + "_filtered", Internal: true,
				Subnets: []netip.Prefix{netip.MustParsePrefix("172.31.0.0/16")}, Gateways: []netip.Addr{netip.MustParseAddr("172.31.0.1")}}}}
	}
	resolve := func(t *testing.T, ctx context.Context, docker filteredDocker, spec RunSpec) (*preparedFilteredServices, string, error) {
		t.Helper()
		recorder := filepath.Join(t.TempDir(), "runtime.log")
		_, bindings, clients, prepared, err := resolveServiceBindings(ctx, docker, overrideRecorderRuntime(t, recorder), spec, compose,
			&networkstate.Approval{Services: digests}, serviceGrants(servicePolicy(t, "db")), nil, nil)
		if err == nil && (len(bindings) != 1 || len(clients) != 2) {
			t.Fatalf("bindings=%v clients=%v, want db bound and both services allowed to the proxy", bindings, clients)
		}
		data, _ := os.ReadFile(recorder)
		return prepared, string(data), err
	}

	t.Run("joins beside a running box and starts nothing", func(t *testing.T) {
		unlock, err := forkspace.LockServiceLaunch(t.Context(), repo, false)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		prepared, calls, err := resolve(t, ctx, fixture(), session)
		if err != nil {
			t.Fatalf("a later box waited on, or failed at, what the running box holds: %v", err)
		}
		if !prepared.joined || strings.Contains(calls, " up ") {
			t.Fatalf("joined=%v, and Compose was called:\n%s", prepared.joined, calls)
		}
	})

	t.Run("a session asks the same before it starts a box", func(t *testing.T) {
		recordHolder(t, repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionACP, SourceID: "another session", Role: forkspace.ExecutionRoleActive})
		unlock, err := forkspace.LockServiceLaunch(t.Context(), repo, false)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		ready := func(docker filteredDocker) error {
			return editorFilteredLaunchBusy(t.Context(), docker, overrideRecorderRuntime(t, filepath.Join(t.TempDir(), "runtime.log")), RunSpec{Repo: repo, SupervisorID: owner}, compose,
				&networkstate.Approval{Services: digests}, serviceGrants(servicePolicy(t, "db")))
		}
		if err := ready(fixture()); err != nil {
			t.Fatalf("a box that would join was put on hold: %v", err)
		}
		other := fixture()
		other.serviceDefinition = "another"
		var busy *ServiceLaunchBusy
		if err := ready(other); !errors.As(err, &busy) {
			t.Fatalf("a box that cannot join was told %v", err)
		}
	})

	for _, tc := range []struct {
		name   string
		spec   RunSpec
		change func(*filteredDaemonFixture)
	}{
		{name: "services started from other bytes", spec: session, change: func(d *filteredDaemonFixture) { d.serviceDefinition = "another" }},
		{name: "a service stopped", spec: session, change: func(d *filteredDaemonFixture) { delete(d.composeServices, "cache") }},
		{name: "no supervisor", spec: RunSpec{Repo: repo, ActivitySource: owner}},
		{name: "another session's project", spec: RunSpec{Repo: repo, ActivitySource: "supervisor-two", SupervisorID: "supervisor-two"},
			change: func(d *filteredDaemonFixture) { d.extraNetworks = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docker := fixture()
			if tc.change != nil {
				tc.change(docker)
			}
			prepared, calls, err := resolve(t, t.Context(), docker, tc.spec)
			if prepared != nil && prepared.joined {
				t.Fatal("joined services it does not match")
			}
			if strings.Count(calls, " up --no-start ") == 0 && err == nil {
				t.Fatalf("neither a launch nor a refusal:\n%s", calls)
			}
			if prepared != nil {
				prepared.cleanup()
			}
		})
	}
}

// A joined box goes straight to the shared side of the lock, so the box already running cannot hold it
// up, and it never runs Compose.
func TestFilteredLaunchOfAJoinedBoxTakesNoExclusiveLock(t *testing.T) {
	f, docker := filteredFixture(t)
	address := netip.MustParseAddr("172.31.0.16")
	f.servicesNet = ComposeProject(f.record.Project) + "_filtered"
	f.preparedServices = &preparedFilteredServices{joined: true, addresses: map[string]netip.Addr{"db": address}}
	docker.networkMembers = map[string]netip.Addr{strings.Repeat("a", 64): address}
	docker.composeServices = map[string]string{"db": strings.Repeat("a", 64)}
	unlock, err := forkspace.LockServiceLaunch(t.Context(), f.record.Project, false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if code, err := f.launch(ctx, RunSpec{Repo: f.record.Project}, nil, nil, io.Discard, io.Discard); err != nil || code != 7 {
		t.Fatalf("filtered launch = (%d, %v)", code, err)
	}
}

// A loop's filtered services carry the run's labels in the seed and the final override alike, so a
// killed filtered run is swept like an open one.
func TestFilteredRunServicesCarryTheRunLabels(t *testing.T) {
	repo := t.TempDir()
	compose := filepath.Join(repo, "compose.yml")
	if err := os.WriteFile(compose, []byte("services:\n  db:\n    image: app:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	digests, err := composeServiceDigests(compose, repo, false, []string{"db"})
	if err != nil {
		t.Fatal(err)
	}
	spec := RunSpec{Repo: repo, RunID: "run-one"}
	host := serviceSupervisor(spec)
	if host == "" {
		t.Skip("this host has no stable process identity, so no service can be labeled")
	}
	docker := &filteredDaemonFixture{extraNetworks: []runtime.DockerNetwork{{
		Name: ComposeProjectFor(repo, spec.RunID) + "_filtered", Internal: true,
		Subnets: []netip.Prefix{netip.MustParsePrefix("172.31.0.0/16")}, Gateways: []netip.Addr{netip.MustParseAddr("172.31.0.1")},
	}}}
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	_, _, _, prepared, err := resolveServiceBindings(t.Context(), docker, overrideRecorderRuntime(t, recorder), spec, compose,
		&networkstate.Approval{Services: digests}, serviceGrants(servicePolicy(t, "db")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(prepared.cleanup)
	data, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	creates := 0
	for _, call := range strings.Split(string(data), "CALL ")[1:] {
		if !strings.Contains(call, " up --no-start ") {
			continue
		}
		creates++
		if !strings.Contains(call, `"coop": "service"`) || !strings.Contains(call, `"coop.host": "`+host+`"`) {
			t.Errorf("a filtered create lacks the run's labels:\n%s", call)
		}
	}
	if creates != 2 {
		t.Fatalf("Compose creates = %d, want the seed and the final:\n%s", creates, data)
	}
}

// A filtered launch never removes service containers: a removed container's anonymous volumes are
// orphaned and its replacement starts empty. Once the filtered network exists the seed declares the
// subnet the final override pins, so Compose recreates only the containers (keeping their anonymous
// volumes) instead of the network, whose disconnect fails for a service `coop up` moved off it.
func TestFilteredLaunchSeedsOntoTheExistingSubnet(t *testing.T) {
	for _, tc := range []struct {
		name       string
		firstSeen  bool // the network does not exist until the seed creates it
		seedSubnet bool
	}{
		{name: "network exists", seedSubnet: true},
		{name: "first launch", firstSeen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			compose := filepath.Join(repo, "compose.yml")
			body := "services:\n  db:\n    image: app:1\n    depends_on: [cache]\n  cache:\n    image: redis:8\n    volumes: [\"/data\"]\n"
			if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			digests, err := composeServiceDigests(compose, repo, false, []string{"db"})
			if err != nil {
				t.Fatal(err)
			}
			id := strings.Repeat("b", 64)
			fixture := &filteredDaemonFixture{
				networkMembers:  map[string]netip.Addr{id: netip.MustParseAddr("172.31.0.2")},
				composeServices: map[string]string{"db": id},
				extraNetworks: []runtime.DockerNetwork{{Name: ComposeProject(repo) + "_filtered", Internal: true,
					Subnets: []netip.Prefix{netip.MustParsePrefix("172.31.0.0/16")}, Gateways: []netip.Addr{netip.MustParseAddr("172.31.0.1")}}},
			}
			var docker filteredDocker = fixture
			if tc.firstSeen {
				docker = &networkAfterSeed{filteredDaemonFixture: fixture}
			}
			recorder := filepath.Join(t.TempDir(), "runtime.log")
			_, _, _, prepared, err := resolveServiceBindings(t.Context(), docker, overrideRecorderRuntime(t, recorder), RunSpec{Repo: repo}, compose,
				&networkstate.Approval{Services: digests}, serviceGrants(servicePolicy(t, "db")), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(prepared.cleanup)
			data, err := os.ReadFile(recorder)
			if err != nil {
				t.Fatal(err)
			}
			var creates []string
			for _, call := range strings.Split(string(data), "CALL ")[1:] {
				if strings.Contains(call, " rm ") {
					t.Fatalf("a filtered launch removed service containers: %q", call)
				}
				if strings.Contains(call, " up --no-start ") {
					creates = append(creates, call)
				}
			}
			if len(creates) != 2 {
				t.Fatalf("Compose creates = %d, want the seed and the final:\n%s", len(creates), data)
			}
			seed, final := creates[0], creates[1]
			if got := strings.Contains(seed, "subnet: 172.31.0.0/16"); got != tc.seedSubnet || strings.Contains(seed, "ipv4_address") {
				t.Fatalf("seed declares the subnet = %v, want %v, and no fixed address:\n%s", got, tc.seedSubnet, seed)
			}
			if !strings.Contains(final, "subnet: 172.31.0.0/16") || !strings.Contains(final, "ipv4_address") {
				t.Fatalf("final override does not pin the subnet and the addresses:\n%s", final)
			}
		})
	}
}

// networkAfterSeed is a daemon whose filtered network appears only once the seed has created it.
type networkAfterSeed struct {
	*filteredDaemonFixture
	looked bool
}

func (d *networkAfterSeed) Networks(ctx context.Context) ([]runtime.DockerNetwork, error) {
	networks, err := d.filteredDaemonFixture.Networks(ctx)
	if d.looked || err != nil {
		return networks, err
	}
	d.looked = true
	return slices.DeleteFunc(networks, func(n runtime.DockerNetwork) bool { return strings.HasSuffix(n.Name, "_filtered") }), nil
}

// overrideRecorderRuntime records each Compose call after "CALL ", followed by the filtered-services
// override files it was given: they are written and removed around the call.
func overrideRecorderRuntime(t *testing.T, recorder string) runtime.Runtime {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "rt")
	script := "#!/bin/sh\n" +
		"printf 'CALL %s\\n' \"$*\" >> " + strconv.Quote(recorder) + "\n" +
		"prev=\n" +
		"for arg in \"$@\"; do\n" +
		"  case \"$prev:$arg\" in -f:*coop-filtered-services-*) cat \"$arg\" >> " + strconv.Quote(recorder) + " ;; esac\n" +
		"  prev=$arg\n" +
		"done\n" +
		"case \"$*\" in *\"config --services\"*) printf '%s\\n' db ;; esac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}
}

// The last Compose step, starting the services, says why it failed too: an unhealthy service or a
// taken port is otherwise only an exit status.
func TestFilteredServicesStartNamesComposesReason(t *testing.T) {
	repo := t.TempDir()
	compose := filepath.Join(repo, "compose.yml")
	if err := os.WriteFile(compose, []byte("services:\n  db:\n    image: app:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	digests, err := composeServiceDigests(compose, repo, false, []string{"db"})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("c", 64)
	docker := &filteredDaemonFixture{
		networkMembers:  map[string]netip.Addr{id: netip.MustParseAddr("172.31.0.2")},
		composeServices: map[string]string{"db": id},
		extraNetworks: []runtime.DockerNetwork{{Name: ComposeProject(repo) + "_filtered", Internal: true,
			Subnets: []netip.Prefix{netip.MustParsePrefix("172.31.0.0/16")}, Gateways: []netip.Addr{netip.MustParseAddr("172.31.0.1")}}},
	}
	shim := filepath.Join(t.TempDir(), "rt")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *\"config --services\"*) printf '%s\\n' db ;;\n" +
		"  *\"up -d --wait\"*) echo ' Container app-db-1 Starting' >&2; echo 'Error response from daemon: port 5432 is already allocated' >&2; exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, _, prepared, err := resolveServiceBindings(t.Context(), docker, runtime.Runtime{Name: shim}, RunSpec{Repo: repo}, compose,
		&networkstate.Approval{Services: digests}, serviceGrants(servicePolicy(t, "db")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(prepared.cleanup)
	err = prepared.start(t.Context())
	if err == nil || !strings.HasSuffix(err.Error(), "Error response from daemon: port 5432 is already allocated") ||
		!strings.Contains(err.Error(), "starting them failed") {
		t.Fatalf("start error = %v, want Compose's own reason", err)
	}
}

func TestComposeFailureNamesComposesOwnReason(t *testing.T) {
	err := composeFailure("prepare filtered services", errors.New("compose up --no-start exited with status 1"),
		" Container app-db-1 Stopping\n Container app-db-1 Stopped\nError response from daemon: container 7dff is not connected to the network app_filtered\n")
	want := "prepare filtered services: compose up --no-start exited with status 1: Container app-db-1 Stopped\n" +
		"Error response from daemon: container 7dff is not connected to the network app_filtered"
	if err == nil || err.Error() != want {
		t.Fatalf("composeFailure = %q, want %q", err, want)
	}
	if err := composeFailure("prepare filtered services", errors.New("exited with status 1"), "  \n"); err.Error() != "prepare filtered services: exited with status 1" {
		t.Fatalf("an empty stderr added noise: %q", err)
	}
}

// A network that is gone is called missing: "not internal" sent a supervisor looking for a wrong setting
// on a network another start's sweep had removed (2026-11-05). One that exists but is not internal
// still says so.
func TestAMissingFilteredServiceNetworkIsCalledMissing(t *testing.T) {
	if _, _, err := filteredServiceAddresses(runtime.DockerNetwork{}, []string{"db"}); err == nil || err.Error() != "filtered service network is missing" {
		t.Fatalf("a missing network = %v", err)
	}
	if _, _, err := filteredServiceAddresses(runtime.DockerNetwork{Name: "app_filtered"}, []string{"db"}); err == nil || err.Error() != "filtered service network is not internal" {
		t.Fatalf("a network that is not internal = %v", err)
	}
}

// With no Compose file there is nothing to draft: the network COOP_SERVICES_NET names is the whole
// problem, so the refusal has no rules, and no approval could settle it.
func TestServicesNetRefusalHasNoRulesToDraft(t *testing.T) {
	err := checkServicesHaveRules(&config.Config{ServicesNet: "shared"}, RunSpec{Network: true}, "", nil)
	const want = "This box cannot start because a filtered box does not join the network COOP_SERVICES_NET names\n\n" +
		"  Unset COOP_SERVICES_NET, or set box.network: false in .agent/project.yaml"
	var refused *ServicesNeedRules
	if !errors.As(err, &refused) {
		t.Fatalf("the refusal is a %T: %v", err, err)
	}
	if err.Error() != want || refused.Rules != "" || refused.Ask != "" {
		t.Fatalf("refusal =\n%v\nwant:\n%s\nrules=%q ask=%q", err, want, refused.Rules, refused.Ask)
	}
}

// A filtered box never joins the shared services network, so a project with services and no
// approved service grant is refused before the runtime is touched, and the refusal carries the
// rules to ask for: whole, drafted from the Compose file, with the port wherever the file says it.
func TestFilteredServicesRefusalDraftsCompleteRules(t *testing.T) {
	store, err := networkstate.Open(filepath.Join(t.TempDir(), "network"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mode := egress.Filtered
	policy, err := store.Admit(repo, networkstate.Admission{InvocationMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(repo, ".agent", "compose.yml")
	if err := os.MkdirAll(filepath.Dir(compose), 0o755); err != nil {
		t.Fatal(err)
	}
	// db declares nothing (coop's own Postgres), api publishes 3000 on loopback, cache exposes two
	// ports and a UDP one, and Bad_Name is no DNS label, so no rule can name it.
	if err := os.WriteFile(compose, []byte(`services:
  db:
    image: postgres:18
  api:
    image: example/api:1
    ports: ["127.0.0.1:8080:3000", {target: 9229, host_ip: 127.0.0.1}]
  cache:
    image: example/cache:1
    expose: ["6379", 6380, "6381/udp"]
  Bad_Name:
    image: example/other:1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	capture := &CapturedEgress{Store: store, Project: repo, Fingerprint: policy.Fingerprint}
	cfg := &config.Config{Egress: "filtered", ConfigDir: t.TempDir()}
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	_, err = prepareFilteredExecution(t.Context(), cfg, recorderRuntime(t, recorder),
		RunSpec{Repo: repo, Cmd: []string{"true"}, Network: true}, capture, compose, nil, nil)
	const want = `This box cannot start because filtered boxes reach project services only through approved network rules

  Ask for each service it uses under box.egress_rules in .agent/project.yaml:
    - to: {service: "api"}
      protocol: tcp
      ports: [3000, 9229]
    - to: {service: "cache"}
      protocol: tcp
      ports: [6379, 6380]
    - to: {service: "db"}
      protocol: tcp
      ports: [<port>]
  Replace <port> with the port the service listens on.
  Then review it: coop approve
  Or run coop approve in a terminal, which offers to add them for you.
  To start without project services, set box.network: false.`
	if err == nil || err.Error() != want {
		t.Fatalf("refusal =\n%v\nwant:\n%s", err, want)
	}
	if _, err := os.Stat(recorder); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the refused launch reached the runtime")
	}
	// Every drafted rule is one the project file accepts once its ports are filled in.
	draft := want[strings.Index(want, "    - to:"):strings.Index(want, "  Replace")]
	if _, err := project.Parse([]byte("box:\n  egress: filtered\n  egress_rules:\n" + strings.ReplaceAll(draft, "<port>", "5432"))); err != nil {
		t.Errorf("the drafted rules do not parse: %v\n%s", err, draft)
	}
	// A thread shows the parts apart, so the rules are the rules and nothing else.
	var refused *ServicesNeedRules
	if !errors.As(err, &refused) {
		t.Fatalf("the refusal is a %T", err)
	}
	if refused.Rules+"\n" != draft {
		t.Errorf("the refusal's rules =\n%s\nwant only\n%s", refused.Rules, draft)
	}
	// box.network: false starts without the services, and so without this refusal.
	_, err = prepareFilteredExecution(t.Context(), cfg, recorderRuntime(t, recorder),
		RunSpec{Repo: repo, Cmd: []string{"true"}}, capture, compose, nil, nil)
	if err != nil && strings.Contains(err.Error(), "project services") {
		t.Errorf("box.network: false still refused on services: %v", err)
	}
}

// A filtered box reaches a service only through its rule, at its Compose name, so it gets the URL
// of each exposed port its rule allows there, with the scheme the service's label names. An open
// box's localhost URL is never offered: nothing forwards it in a filtered box.
func TestFilteredServiceEnvNamesTheServicesTheRulesAllow(t *testing.T) {
	body := "services:\n" +
		"  web:\n    image: a\n    expose: [\"8443\", \"9000\"]\n    labels: {coop.service.scheme: https}\n" +
		"  auth-db:\n    image: b\n    expose: [5432]\n    labels: [\"coop.service.scheme=postgresql\"]\n" +
		"  cache:\n    image: c\n    expose: [\"6379\"]\n" +
		"  queue:\n    image: d\n    ports: [\"127.0.0.1:5672:5672\"]\n" +
		"  unasked:\n    image: e\n    expose: [\"80\"]\n"
	repo, path := writeCompose(t, body)
	grant := func(service, protocol string, ports ...int) egress.Grant {
		return egress.Grant{Rule: egress.Rule{To: egress.Destination{Service: service}, Protocol: protocol, Ports: ports}}
	}
	grants := []egress.Grant{
		grant("web", "tcp", 8443), // 9000 is exposed but not allowed
		grant("auth-db", "tcp", 5432),
		grant("cache", "udp", 6379), // a udp rule carries no TCP URL
		grant("queue", "tcp", 5672), // published with ports:, not exposed
	}
	got := filteredServiceEnv(path, repo, false, grants)
	want := []string{"-e", "COOP_SERVICE_AUTH_DB_URL=postgresql://auth-db:5432", "-e", "COOP_SERVICE_WEB_URL=https://web:8443"}
	if !slices.Equal(got, want) {
		t.Fatalf("filtered service env = %q, want %q", got, want)
	}
	if got := filteredServiceEnv(filepath.Join(repo, "missing.yml"), repo, false, grants); got != nil {
		t.Fatalf("a Compose file Coop cannot read gave %q", got)
	}
}

// A filtered box whose approved rules name services starts them itself, so turning that off, with
// COOP_AUTO_UP=0 or box.auto_up: false, refuses the box and names the setting that did it.
func TestFilteredBoxWithServiceRulesHonorsAutoUpOff(t *testing.T) {
	grants := []egress.Grant{{Rule: egress.Rule{To: egress.Destination{Service: "db"}, Protocol: "tcp", Ports: []int{5432}}}}
	if err := checkFilteredAutoUp(&config.Config{AutoUp: true}, grants); err != nil {
		t.Fatalf("auto-up on refused the box: %v", err)
	}
	if err := checkFilteredAutoUp(&config.Config{AutoUp: false}, nil); err != nil {
		t.Fatalf("a box with no service rules was refused: %v", err)
	}
	off := false
	fromProject := applyProjectPolicy(&config.Config{AutoUp: true}, &project.Project{Box: project.Box{AutoUp: &off}}, &RunSpec{})
	err := checkFilteredAutoUp(fromProject, grants)
	if err == nil || !strings.Contains(err.Error(), "box.auto_up: false in "+project.File) || !strings.Contains(err.Error(), "remove box.auto_up") ||
		strings.Contains(err.Error(), "COOP_AUTO_UP") {
		t.Fatalf("box.auto_up: false = %v", err)
	}
	conf := filepath.Join(t.TempDir(), "coop.conf")
	if err := os.WriteFile(conf, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_CONF", conf)
	t.Setenv("COOP_AUTO_UP", "0")
	explicit, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	err = checkFilteredAutoUp(applyProjectPolicy(explicit, &project.Project{}, &RunSpec{}), grants)
	if err == nil || !strings.Contains(err.Error(), "COOP_AUTO_UP=0") || !strings.Contains(err.Error(), "unset COOP_AUTO_UP") ||
		!strings.Contains(err.Error(), "run coop approve") {
		t.Fatalf("COOP_AUTO_UP=0 = %v", err)
	}
}
