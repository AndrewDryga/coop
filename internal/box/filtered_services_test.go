package box

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkstate"
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
	options, published, env := filteredPublish(&config.Config{}, spec, func(int) bool { return true })
	if len(options) != 2 || options[0] != "-p" || !strings.HasSuffix(options[1], ":3000") || !strings.HasPrefix(options[1], "127.0.0.1:") {
		t.Fatalf("publish options bind the wrong interface or port: %v", options)
	}
	if len(published) != 1 || published[0] != 3000 {
		t.Fatalf("the published port list is wrong: %v", published)
	}
	if len(env) != 2 || !strings.HasPrefix(env[1], "COOP_SERVE_URL_3000=http://localhost:") {
		t.Fatalf("the box was not told its own URL: %v", env)
	}
	// A taken host port skips publication without failing the launch, and the
	// discovery URL still names the workspace's assigned port.
	options, published, env = filteredPublish(&config.Config{}, spec, func(int) bool { return false })
	if len(options) != 0 || len(published) != 0 || len(env) != 2 {
		t.Fatalf("a taken host port changed more than the publication: %v %v %v", options, published, env)
	}
	if options, published, env := filteredPublish(&config.Config{}, RunSpec{Repo: t.TempDir()}, func(int) bool { return true }); options != nil || published != nil || env != nil {
		t.Fatal("a run without serve ports published something")
	}
	if options, published, env := filteredPublish(&config.Config{}, RunSpec{Repo: t.TempDir(), servePorts: []int{3000}}, func(int) bool {
		t.Fatal("a run that did not request Serve must not inspect host ports")
		return true
	}); options != nil || published != nil || env != nil {
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
	_, _, err := resolveServiceBindings(context.Background(), nil, runtime.Runtime{Name: "docker"}, RunSpec{Repo: t.TempDir()}, "", nil, grants, nil, nil)
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
	// The same refusal is what a launch gets, before anything is started.
	_, _, launchErr := resolveServiceBindings(context.Background(), nil, runtime.Runtime{Name: "docker"}, RunSpec{Repo: repo}, compose, approval, serviceGrants(servicePolicy(t, "db")), nil, nil)
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
	}
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	if _, _, err := resolveServiceBindings(t.Context(), docker, recorderRuntime(t, recorder), RunSpec{Repo: repo}, compose,
		&networkstate.Approval{Services: digests}, serviceGrants(servicePolicy(t, "db")), nil, nil); err != nil {
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

func TestFilteredServiceStartSkipsComposeWhileAnotherBoxRuns(t *testing.T) {
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

	id := strings.Repeat("a", 64)
	docker := &filteredDaemonFixture{
		networkMembers:  map[string]netip.Addr{id: netip.MustParseAddr("172.31.0.2")},
		composeServices: map[string]string{"db": id},
	}
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	network, bindings, err := resolveServiceBindings(t.Context(), docker, recorderRuntime(t, recorder), RunSpec{Repo: repo}, compose,
		&networkstate.Approval{Services: digests}, serviceGrants(servicePolicy(t, "db")), nil, nil)
	if err != nil || network != ComposeProject(repo)+"_default" || len(bindings) != 1 {
		t.Fatalf("running service discovery = network %q bindings %+v err %v", network, bindings, err)
	}
	if data, readErr := os.ReadFile(recorder); readErr == nil && len(data) != 0 {
		t.Fatalf("filtered launch executed Compose beside a live box:\n%s", data)
	} else if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	docker.composeServices = nil
	if _, _, err := resolveServiceBindings(t.Context(), docker, recorderRuntime(t, recorder), RunSpec{Repo: repo}, compose,
		&networkstate.Approval{Services: digests}, serviceGrants(servicePolicy(t, "db")), nil, nil); err == nil || !strings.Contains(err.Error(), "is not running") {
		t.Fatalf("missing already-running service was accepted: %v", err)
	}
}
