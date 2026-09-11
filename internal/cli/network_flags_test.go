package cli

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestExtractNetworkFlagsPreservesPresenceAndSeparator(t *testing.T) {
	args := []string{"--egress=filtered", "--allow-domain", "EXAMPLE.com.", "--peer", "codex", "--egress-rules", "rules.yaml", "--allow-domain=api.example.com", "--", "--egress", "open"}
	before := slices.Clone(args)
	flags, rest, err := extractNetworkFlags("coop run", args)
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
		got, _, err := extractNetworkFlags("coop run", test.args)
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
		_, _, err := extractNetworkFlags("coop run", args)
		if err == nil {
			t.Fatalf("accepted %q", args)
		}
		var usage *ui.UsageError // every launch flag is refused in the one shared block
		if !errors.As(err, &usage) {
			t.Errorf("%q refused outside the shared block: %v", args, err)
		}
	}
	args := make([]string, egress.MaxRules+1)
	for i := range args {
		args[i] = "--allow-domain=example.com"
	}
	if _, _, err := extractNetworkFlags("coop run", args); err == nil {
		t.Fatal("unbounded domain flags")
	}
}

// `coop loop` takes the same egress flags as every other launch, and its own
// grammar never sees them: an unrecognized argument there is a usage error.
func TestLoopTakesTheSameEgressFlags(t *testing.T) {
	args := []string{"claude", "--egress", "filtered", "--allow-domain", "example.com", "--max-tasks", "2"}
	flags, rest, err := extractNetworkFlags("coop run", args)
	if err != nil || flags.Mode == nil || *flags.Mode != egress.Filtered || !slices.Equal(flags.Domains, []string{"example.com"}) {
		t.Fatalf("loop egress flags = %#v: %v", flags, err)
	}
	if _, _, _, _, _, _, maxTasks, err := parseLoopArgs(rest, false); err != nil || maxTasks != 2 {
		t.Fatalf("loop grammar after stripping = (%d, %v), want the loop's own flags only", maxTasks, err)
	}
	if _, _, _, _, _, _, _, err := parseLoopArgs(args, false); err == nil {
		t.Fatal("the loop grammar accepted an egress flag it does not own")
	}
}

// A detached fork worker admits its OWN policy at its loop start, so the parent
// forwards the flags verbatim — with the rules file absolutized, since the worker
// starts in the parent repository.
func TestForkLoopForwardsTheEgressFlagsToItsWorker(t *testing.T) {
	fa, err := parseForkCreate([]string{"risky", "claude", "--loop", "-d", "--egress", "filtered", "--allow-domain", "example.com"})
	if err != nil {
		t.Fatalf("fork loop with egress flags: %v", err)
	}
	forwarded, err := fa.network.args()
	if err != nil || !slices.Equal(forwarded, []string{"--egress", "filtered", "--allow-domain", "example.com"}) {
		t.Fatalf("forwarded = %q: %v", forwarded, err)
	}
	rules, err := parseForkCreate([]string{"risky", "claude", "--loop", "--egress-rules", "rules.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	forwarded, err = rules.network.args()
	if err != nil {
		t.Fatal(err)
	}
	if len(forwarded) != 2 || !filepath.IsAbs(forwarded[1]) {
		t.Errorf("forwarded rules file = %q, want an absolute path", forwarded)
	}
	// An interactive fork is not wired into restricted networking; refuse rather
	// than accept a flag that would silently do nothing.
	if _, err := parseForkCreate([]string{"risky", "claude", "--egress", "filtered"}); err == nil {
		t.Error("an interactive fork accepted --egress")
	}
}
