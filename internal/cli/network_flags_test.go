package cli

import (
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

func TestExtractNetworkFlagsPreservesPresenceAndSeparator(t *testing.T) {
	args := []string{"--egress=filtered", "--allow-domain", "EXAMPLE.com.", "--peer", "codex", "--egress-rules", "rules.yaml", "--allow-domain=api.example.com", "--", "--egress", "open"}
	before := slices.Clone(args)
	flags, rest, err := extractNetworkFlags(args)
	if err != nil || flags.Mode == nil || *flags.Mode != egress.Filtered || flags.RulesFile != "rules.yaml" || !slices.Equal(flags.Domains, []string{"example.com", "api.example.com"}) {
		t.Fatalf("flags %#v: %v", flags, err)
	}
	if !slices.Equal(rest, []string{"--peer", "codex", "--", "--egress", "open"}) || !slices.Equal(args, before) {
		t.Fatalf("separator or input changed: rest %q input %q", rest, args)
	}
	for _, test := range []struct {
		args []string
		mode *egress.Mode
	}{{nil, nil}, {[]string{"--allow-domain", "example.com"}, nil}} {
		got, _, err := extractNetworkFlags(test.args)
		if err != nil || got.Mode != test.mode {
			t.Fatal("parser inferred posture", got, err)
		}
	}
	input := flags.admission()
	if input.InvocationMode == nil || *input.InvocationMode != egress.Filtered || input.RulesFile != "rules.yaml" ||
		!slices.Equal(input.Domains, []string{"example.com", "api.example.com"}) {
		t.Fatalf("admission lost the invocation-only request: %#v", input)
	}
}

func TestExtractNetworkFlagsRejectsAmbiguousInput(t *testing.T) {
	for _, args := range [][]string{
		{"--egress"}, {"--egress="}, {"--egress", "--allow-domain", "example.com"}, {"--egress", "Filtered"},
		{"--egress=open", "--egress=open"}, {"--egress-rules=a", "--egress-rules=b"},
		{"--allow-domain", "https://example.com"}, {"--allow-domain", "*.example.com"}, {"--allow-domain", "1.1.1.1"},
		{"--allow-domain="}, {"--egress-rules"}, {"--egress-rules="},
	} {
		if _, _, err := extractNetworkFlags(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	args := make([]string, egress.MaxRules+1)
	for i := range args {
		args[i] = "--allow-domain=example.com"
	}
	if _, _, err := extractNetworkFlags(args); err == nil {
		t.Fatal("unbounded domain flags")
	}
}
