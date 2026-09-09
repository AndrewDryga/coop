package networkstate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

func TestDeclaredNetworkFingerprintIsCanonicalPrivateAndSeparate(t *testing.T) {
	store, project := openStore(t), t.TempDir()
	rules := []egress.Rule{
		{To: egress.Destination{Provider: "anthropic", Features: []string{"remote-mcp", "cloud-mcp", "cloud-mcp"}}},
		{To: egress.Destination{IP: "198.51.100.7"}, Protocol: "tcp", Ports: []int{443}},
		{To: egress.Destination{Domain: "*.api.example.com"}, Protocol: "tls", Ports: []int{443}},
	}
	before, _ := json.Marshal(rules)
	got, err := store.DeclaredNetworkFingerprint(project, egress.Filtered, rules, false)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(rules)
	if !bytes.Equal(before, after) {
		t.Fatal("declaration mutated caller rules")
	}
	equivalent := []egress.Rule{rules[2], rules[1], {To: egress.Destination{Provider: "anthropic", Features: []string{"cloud-mcp", "remote-mcp"}}}}
	if again, err := store.DeclaredNetworkFingerprint(project, egress.Filtered, equivalent, false); err != nil || again != got {
		t.Fatal("equivalent declaration changed fingerprint", err)
	}
	for name, changed := range map[string]func([]egress.Rule){
		"wildcard": func(r []egress.Rule) { r[2].To.Domain = "*.other.example.com" },
		"ip":       func(r []egress.Rule) { r[1].To.IP = "198.51.100.8" },
		"cidr":     func(r []egress.Rule) { r[1].To.IP, r[1].To.CIDR = "", "198.51.100.0/24" },
		"protocol": func(r []egress.Rule) { r[1].Protocol = "udp" },
		"feature":  func(r []egress.Rule) { r[0].To.Features = []string{"different-feature"} },
	} {
		t.Run(name, func(t *testing.T) {
			var copy []egress.Rule
			if err := json.Unmarshal(before, &copy); err != nil {
				t.Fatal(err)
			}
			changed(copy)
			if next, err := store.DeclaredNetworkFingerprint(project, egress.Filtered, copy, false); err != nil || next == got {
				t.Fatal("changed authority retained fingerprint", err)
			}
		})
	}
	for _, test := range []struct {
		store   *Store
		project string
		export  bool
	}{
		{store, t.TempDir(), false}, {store, project, true}, {openStore(t), project, false},
	} {
		if next, err := test.store.DeclaredNetworkFingerprint(test.project, egress.Filtered, rules, test.export); err != nil || next == got {
			t.Fatal("project, owner, or export identity was omitted", err)
		}
	}
	mode := egress.Open
	declared, err := store.DeclaredNetworkFingerprint(project, mode, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := store.Admit(project, Admission{PolicyMode: &mode})
	if err != nil || declared == effective.Fingerprint {
		t.Fatal("declared and effective capture domains overlap", err)
	}
}

func TestDeclaredNetworkFingerprintRejectsInvalidEnvelope(t *testing.T) {
	store, project := openStore(t), t.TempDir()
	oversized := make([]egress.Rule, egress.MaxRules)
	for i := range oversized {
		features := make([]string, egress.MaxConstraints)
		for j := range features {
			features[j] = fmt.Sprintf("f%062d", j)
		}
		oversized[i].To = egress.Destination{Provider: fmt.Sprintf("p%062d", i), Features: features}
	}
	for _, test := range []struct {
		name  string
		mode  egress.Mode
		rules []egress.Rule
	}{
		{"mode", "invalid", nil}, {"open-rules", egress.Open, []egress.Rule{rule("example.com")}},
		{"count", egress.Filtered, make([]egress.Rule, egress.MaxRules+1)},
		{"provider", egress.Filtered, []egress.Rule{{To: egress.Destination{Provider: "Anthropic"}}}},
		{"utf8", egress.Filtered, []egress.Rule{{To: egress.Destination{Provider: string([]byte{'a', 0xff})}}}},
		{"bytes", egress.Filtered, oversized},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.DeclaredNetworkFingerprint(project, test.mode, test.rules, false); err == nil {
				t.Fatal("invalid declaration accepted")
			}
		})
	}
}
