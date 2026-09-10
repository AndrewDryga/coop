package box

import (
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
)

func TestFilteredExtraArgsAcceptEveryBindSpelling(t *testing.T) {
	cases := []struct {
		name  string
		given []string
		want  []string
	}{
		{"short", []string{"-v", "/src:/dst:ro"}, []string{"-v", "/src:/dst:ro"}},
		{"long", []string{"--volume", "/src:/dst"}, []string{"-v", "/src:/dst"}},
		{"inline", []string{"--volume=/src:/dst"}, []string{"-v", "/src:/dst"}},
		{"mount", []string{"--mount", "type=bind,source=/src,target=/dst"}, []string{"-v", "/src:/dst"}},
		{"mount readonly", []string{"--mount", "type=bind,source=/src,target=/dst,readonly"}, []string{"-v", "/src:/dst:ro"}},
		{"mount aliases", []string{"--mount", "type=bind,src=/src,dst=/dst"}, []string{"-v", "/src:/dst"}},
		{"env", []string{"-e", "COOP_REVIEW=1"}, []string{"-e", "COOP_REVIEW=1"}},
		{"env long", []string{"--env", "K=v"}, []string{"-e", "K=v"}},
		{"env inline", []string{"--env=K=a=b"}, []string{"-e", "K=a=b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := filteredExtraArgs(tc.given, nil)
			if err != nil {
				t.Fatalf("filteredExtraArgs(%q) = %v", tc.given, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("filteredExtraArgs(%q) = %q, want %q", tc.given, got, tc.want)
			}
		})
	}
}

// A filtered run must not silently drop an argument the operator set: it would
// enforce a shape nobody chose. Every refusal NAMES what it refused.
func TestFilteredExtraArgsRefuseEverythingElseByName(t *testing.T) {
	cases := map[string]string{
		"--privileged":                    `"--privileged"`,
		"--network host":                  `"--network"`,
		"--user 0:0":                      `"--user"`,
		"-e HOME":                         "complete KEY=VALUE assignment",
		"-e =1":                           "plain KEY=VALUE assignment",
		"--mount type=tmpfs,target=/t":    "--mount type=tmpfs",
		"--mount type=bind,source=/src":   "source= and target=",
		"--mount type=bind,fake=1,src=/a": `"fake"`,
	}
	for given, want := range cases {
		t.Run(given, func(t *testing.T) {
			_, err := filteredExtraArgs(strings.Fields(given), nil)
			if err == nil {
				t.Fatalf("filteredExtraArgs(%q) was accepted", given)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		})
	}
}

func TestFilteredExtraArgsCombineConfigAndRunArguments(t *testing.T) {
	got, err := filteredExtraArgs([]string{"-v", "/a:/a:ro"}, []string{"-v", "/b:/b"})
	if err != nil {
		t.Fatalf("filteredExtraArgs: %v", err)
	}
	if !slices.Equal(got, []string{"-v", "/a:/a:ro", "-v", "/b:/b"}) {
		t.Errorf("got %q", got)
	}
	// A review run carries COOP_REVIEW=1 through its own extra arguments. The
	// gateway is the boundary, so the environment passes; nothing else does.
	if got, err := filteredExtraArgs(nil, []string{"-e", "COOP_REVIEW=1"}); err != nil || !slices.Equal(got, []string{"-e", "COOP_REVIEW=1"}) {
		t.Errorf("review environment = (%q, %v), want it accepted", got, err)
	}
	if _, err := filteredExtraArgs(nil, []string{"--env-file", "/etc/env"}); err == nil {
		t.Error("--env-file is not a KEY=VALUE assignment; it was accepted")
	}
	if got, err := filteredExtraArgs(nil, nil); err != nil || got != nil {
		t.Errorf("no extra arguments = (%q, %v), want (nil, nil)", got, err)
	}
	if _, err := filteredExtraArgs([]string{"-v"}, nil); err == nil {
		t.Error("a dangling -v was accepted")
	}
	if _, err := filteredExtraArgs([]string{"-e"}, nil); err == nil {
		t.Error("a dangling -e was accepted")
	}
}

func TestNetworkRuleDiffIsAPlainSetDifference(t *testing.T) {
	rule := func(domain string) egress.Rule {
		return egress.Rule{To: egress.Destination{Domain: domain}, Protocol: "tls", Ports: []int{443}}
	}
	add, remove := NetworkRuleDiff([]egress.Rule{rule("a.example"), rule("b.example")}, []egress.Rule{rule("b.example"), rule("c.example")})
	if len(add) != 1 || add[0].To.Domain != "c.example" {
		t.Errorf("add = %+v, want c.example", add)
	}
	if len(remove) != 1 || remove[0].To.Domain != "a.example" {
		t.Errorf("remove = %+v, want a.example", remove)
	}
	if add, remove := NetworkRuleDiff(nil, nil); add != nil || remove != nil {
		t.Errorf("empty diff = (%+v, %+v)", add, remove)
	}
}

func TestNetworkRuleTextAndYAMLReadLikeTheConfiguration(t *testing.T) {
	tls := egress.Rule{To: egress.Destination{Domain: "docs.example.com"}, Protocol: "tls", Ports: []int{443}}
	if got := NetworkRuleText(tls); got != "docs.example.com tls/443" {
		t.Errorf("NetworkRuleText = %q", got)
	}
	yaml := NetworkRuleYAML(tls)
	for _, want := range []string{"- to:", `domain: "docs.example.com"`, "protocol: tls", "ports: [443]"} {
		if !strings.Contains(yaml, want) {
			t.Errorf("NetworkRuleYAML is missing %q:\n%s", want, yaml)
		}
	}
	raw := egress.Rule{To: egress.Destination{CIDR: "10.0.0.0/24"}, Protocol: "udp", Ports: []int{123, 124}}
	if got := NetworkRuleText(raw); got != "10.0.0.0/24 udp/123,124" {
		t.Errorf("NetworkRuleText(cidr) = %q", got)
	}
	provider := egress.Rule{To: egress.Destination{Provider: "claude", Features: []string{"cloud-mcp"}}}
	if got := NetworkRuleText(provider); got != "claude features cloud-mcp" {
		t.Errorf("NetworkRuleText(provider) = %q", got)
	}
	wildcard := egress.Rule{To: egress.Destination{Domain: "*.example.com"}, Protocol: "tls", Ports: []int{443}}
	if got := NetworkRuleText(wildcard); got != "*.example.com tls/443" {
		t.Errorf("NetworkRuleText(wildcard) = %q", got)
	}
	if yaml := NetworkRuleYAML(wildcard); !strings.Contains(yaml, `domain: "*.example.com"`) {
		t.Errorf("NetworkRuleYAML(wildcard) = %q", yaml)
	}
	ping := egress.Rule{To: egress.Destination{CIDR: "10.0.0.0/8"}, Protocol: "icmp", Types: []string{"8"}, Codes: []int{0}}
	if got := NetworkRuleText(ping); got != "10.0.0.0/8 icmp types 8 codes 0" {
		t.Errorf("NetworkRuleText(icmp) = %q", got)
	}
	for _, want := range []string{`cidr: "10.0.0.0/8"`, "protocol: icmp", "types: [8]", "codes: [0]"} {
		if yaml := NetworkRuleYAML(ping); !strings.Contains(yaml, want) {
			t.Errorf("NetworkRuleYAML(icmp) is missing %q:\n%s", want, yaml)
		}
	}
	service := egress.Rule{To: egress.Destination{Service: "db"}, Protocol: "tcp", Ports: []int{5432}}
	if got := NetworkRuleText(service); got != "service db tcp/5432" {
		t.Errorf("NetworkRuleText(service) = %q", got)
	}
	if yaml := NetworkRuleYAML(service); !strings.Contains(yaml, `service: "db"`) {
		t.Errorf("NetworkRuleYAML(service) = %q", yaml)
	}
}

// The label must name the input that actually decided the mode, in the order
// admission resolves them — a remembered approval outranks the repository.
func TestPostureSourceNamesTheDecidingInput(t *testing.T) {
	filtered, open := egress.Filtered, egress.Open
	approval := &networkstate.Approval{Posture: egress.Filtered}
	rule := egress.Rule{To: egress.Destination{Domain: "a.example"}, Protocol: "tls", Ports: []int{443}}
	cases := []struct {
		name     string
		input    networkstate.Admission
		approval *networkstate.Approval
		want     string
	}{
		{"nothing", networkstate.Admission{}, nil, PostureFromDefault},
		{"approval wins", networkstate.Admission{HostPreference: &open, ProjectMode: &filtered}, approval, PostureFromApproval},
		{"host preference", networkstate.Admission{HostPreference: &open, ProjectMode: &filtered}, nil, PostureFromHost},
		{"project mode", networkstate.Admission{ProjectMode: &filtered}, nil, PostureFromProject},
		{"project rules", networkstate.Admission{Requests: []egress.Rule{rule}}, nil, PostureFromProject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := postureSource(tc.input, tc.approval); got != tc.want {
				t.Errorf("postureSource = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApprovalModeAsksAboutTheRepositorysOwnRequest(t *testing.T) {
	filtered, none, open := egress.Filtered, egress.None, egress.Open
	rule := egress.Rule{To: egress.Destination{Domain: "a.example"}, Protocol: "tls", Ports: []int{443}}
	remembered := &networkstate.Approval{Posture: egress.None}
	if got := approvalMode(networkstate.Admission{ProjectMode: &filtered}, remembered, &open); got != egress.Open {
		t.Errorf("explicit --mode = %q, want open", got)
	}
	if got := approvalMode(networkstate.Admission{ProjectMode: &none}, remembered, nil); got != egress.None {
		t.Errorf("project mode = %q, want none", got)
	}
	if got := approvalMode(networkstate.Admission{Requests: []egress.Rule{rule}}, nil, nil); got != egress.Filtered {
		t.Errorf("rules alone = %q, want filtered", got)
	}
	if got := approvalMode(networkstate.Admission{}, remembered, nil); got != egress.None {
		t.Errorf("silent project = %q, want the remembered posture", got)
	}
	if got := approvalMode(networkstate.Admission{}, nil, nil); got != egress.Filtered {
		t.Errorf("nothing at all = %q, want filtered", got)
	}
}
