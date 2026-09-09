package egress

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func testBundle(client Client, auth string) Bundle {
	return Bundle{Provider: "model", Client: client, Backend: "direct", AuthMode: auth, Version: "1",
		Core: []Rule{tlsRule("api.example.com")}, Features: map[string][]Rule{"cloud-mcp": {tlsRule("mcp.example.com")}}}
}

func TestProviderVariantsUnionOnlySelectedAuthorityWithFullProvenance(t *testing.T) {
	bundles := []Bundle{testBundle(ClientCLI, "key"), testBundle(ClientCLI, "oauth"), testBundle(ClientACP, "oauth")}
	request := Rule{To: Destination{Provider: "model", Features: []string{"cloud-mcp"}}}
	inputs := []Input{
		{Rules: []Rule{request, request}, Origin: Origin{Kind: "operator", Name: "policy-a"}},
		{Rules: []Rule{request}, Origin: Origin{Kind: "operator", Name: "policy-b"}},
	}
	first, err := Compile("test", Filtered, inputs, bundles, false, ownerKey())
	if err != nil || len(first.Dependencies) != 3 || len(first.Grants) != 2 {
		t.Fatal("selected variants lost authority", first, err)
	}
	for _, grant := range first.Grants {
		want := 3
		if grant.Rule.To.Domain == "mcp.example.com" {
			want = 6
		}
		if len(grant.Origins) != want {
			t.Fatal("overlap lost provenance or duplicated origins", grant)
		}
		for _, origin := range grant.Origins {
			if origin.Provider != "model" || origin.Client == "" || origin.Backend != "direct" || origin.AuthMode == "" || origin.BundleVersion != "1" {
				t.Fatal("incomplete variant provenance", origin)
			}
			if want == 6 && origin.Name != "policy-a" && origin.Name != "policy-b" {
				t.Fatal("feature expansion lost requesting authority", origin)
			}
		}
	}
	slices.Reverse(bundles)
	slices.Reverse(inputs)
	second, err := Compile("test", Filtered, inputs, bundles, false, ownerKey())
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("variant/input order changed frozen authority", err)
	}
	for _, mode := range []Mode{Open, None} {
		got, err := Compile("test", mode, nil, bundles, false, ownerKey())
		if err != nil || len(got.Dependencies) != 0 || len(got.Grants) != 0 {
			t.Fatal("provider selection created an exception to mode", mode, got, err)
		}
	}
}

func TestProviderFeatureMustExistInEverySelectedVariant(t *testing.T) {
	cli, acp := testBundle(ClientCLI, "oauth"), testBundle(ClientACP, "oauth")
	acp.Features = nil
	input := []Input{{Rules: []Rule{{To: Destination{Provider: "model", Features: []string{"cloud-mcp"}}}}, Origin: Origin{Kind: "operator"}}}
	if got, err := Compile("test", Filtered, input, []Bundle{cli, acp}, false, ownerKey()); err == nil || got.Fingerprint != "" {
		t.Fatal("unsupported selected variant was ignored", got, err)
	}
	if got, err := Compile("test", Filtered, input, []Bundle{cli}, false, ownerKey()); err != nil || len(got.Dependencies) != 1 {
		t.Fatal("unselected variant affected authority", got, err)
	}
	if _, err := FeatureExpansions([]Rule{{To: Destination{Provider: "unselected"}}}, []Bundle{cli}); err == nil {
		t.Fatal("empty-feature request selected an unauthorized provider")
	}
}

func TestBundleSelectionRejectsCompetingAndMalformedVariants(t *testing.T) {
	for _, version := range []string{"1", "2"} {
		a, b := testBundle(ClientCLI, "key"), testBundle(ClientCLI, "key")
		b.Version = version
		if _, err := SelectedBundles([]Bundle{a, b}); err == nil {
			t.Fatal("competing same-variant releases were unioned", version)
		}
	}
	for _, field := range []string{"client", "empty-client", "backend", "auth", "version", "provider", "nested", "empty-feature", "source"} {
		bundle := testBundle(ClientCLI, "key")
		switch field {
		case "client":
			bundle.Client = "unknown"
		case "empty-client":
			bundle.Client = ""
		case "backend":
			bundle.Backend = "direct/other"
		case "auth":
			bundle.AuthMode = strings.Repeat("a", 64)
		case "version":
			bundle.Version = "1\n2"
		case "provider":
			bundle.Provider = ""
		case "nested":
			bundle.Core = []Rule{{To: Destination{Provider: "other"}}}
		case "empty-feature":
			bundle.Features["cloud-mcp"] = nil
		case "source":
			bundle.Sources = []string{"bad\nsource"}
		}
		if got, err := SelectedBundles([]Bundle{bundle}); err == nil || got != nil {
			t.Fatal("malformed bundle accepted", field, got, err)
		}
	}
}

func TestFeatureExpansionsAreCanonicalAndDeduplicated(t *testing.T) {
	bundles := []Bundle{testBundle(ClientCLI, "oauth"), testBundle(ClientACP, "oauth")}
	for i := range bundles {
		bundles[i].Features["tools"] = []Rule{tlsRule("mcp.example.com")}
	}
	rules := []Rule{
		{To: Destination{Provider: "model", Features: []string{"cloud-mcp", "tools"}}},
		{To: Destination{Provider: "model", Features: []string{"tools"}}},
	}
	first, err := FeatureExpansions(rules, bundles)
	if err != nil || len(first) != 4 {
		t.Fatal("overlapping requests multiplied expansions", first, err)
	}
	slices.Reverse(rules)
	slices.Reverse(bundles)
	second, err := FeatureExpansions(rules, bundles)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("feature approval depends on input ordering", err)
	}
}

func TestBundleSourcesAreCanonicalAndDisplaySafe(t *testing.T) {
	bundle := testBundle(ClientCLI, "key")
	first, err := SelectedBundles([]Bundle{bundle})
	if err != nil {
		t.Fatal(err)
	}
	bundle.Sources = []string{}
	second, err := SelectedBundles([]Bundle{bundle})
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("empty source representation changes bundle identity", err)
	}
	bundle.Sources = []string{"https://b.example.com", "https://a.example.com", "https://a.example.com"}
	selected, err := SelectedBundles([]Bundle{bundle})
	if err != nil || !slices.Equal(selected[0].Sources, []string{"https://a.example.com", "https://b.example.com"}) {
		t.Fatal("sources are not canonical", selected, err)
	}
	if len(bundle.Sources) != 3 || bundle.Sources[0] != "https://b.example.com" {
		t.Fatal("normalization mutated its caller")
	}
	for _, control := range []string{"\u0085", "\u202e", "\u2066", "\xff"} {
		bundle.Sources = []string{"https://example.com/" + control}
		if _, err := SelectedBundles([]Bundle{bundle}); err == nil {
			t.Fatalf("unsafe source accepted: %q", control)
		}
	}
}

func TestProviderExpansionPreservesRequestingVersion(t *testing.T) {
	bundle := testBundle(ClientCLI, "key")
	request := Rule{To: Destination{Provider: "model", Features: []string{"cloud-mcp"}}}
	inputs := []Input{
		{Rules: []Rule{request}, Origin: Origin{Kind: "operator", Name: "policy", Version: "source-1"}},
		{Rules: []Rule{request}, Origin: Origin{Kind: "operator", Name: "policy", Version: "source-2"}},
	}
	snapshot, err := Compile("test", Filtered, inputs, []Bundle{bundle}, false, ownerKey())
	if err != nil {
		t.Fatal(err)
	}
	for _, grant := range snapshot.Grants {
		if grant.Rule.To.Domain != "mcp.example.com" {
			continue
		}
		if len(grant.Origins) != 2 || grant.Origins[0].Version == grant.Origins[1].Version {
			t.Fatal("requesting versions were overwritten or collapsed", grant.Origins)
		}
		for _, origin := range grant.Origins {
			if origin.BundleVersion != bundle.Version {
				t.Fatal("derived version was lost", origin)
			}
		}
	}
}

func TestInputProvenanceCannotSupplyDerivedIdentityOrDisplayControls(t *testing.T) {
	for _, origin := range []Origin{
		{Kind: "operator", Provider: "model"}, {Kind: "operator", Client: ClientCLI},
		{Kind: "operator", Backend: "direct"}, {Kind: "operator", AuthMode: "key"},
		{Kind: "operator", Feature: "cloud-mcp"}, {Kind: "operator", BundleVersion: "1"},
		{Kind: "operator", Name: "policy\u202e"}, {Kind: "operator", Version: "1\u0085"},
		{Kind: "operator", Name: "invalid\xff"}, {Kind: "operator\n"},
	} {
		if got, err := Compile("test", Filtered, []Input{{Rules: []Rule{tlsRule("example.com")}, Origin: origin}}, nil, false, ownerKey()); err == nil || got.Fingerprint != "" {
			t.Fatalf("invalid requesting provenance accepted: %#v", origin)
		}
	}
}

func TestProviderExpansionAndOriginBudgetsBoundSharedDestinations(t *testing.T) {
	t.Run("definition occurrences", func(t *testing.T) {
		var bundles []Bundle
		for i := 0; i <= MaxExpandedRules/MaxRules; i++ {
			bundle := testBundle(ClientCLI, "key")
			bundle.Backend = fmt.Sprintf("backend-%d", i)
			bundle.Features = nil
			bundle.Core = make([]Rule, MaxRules)
			for j := range bundle.Core {
				bundle.Core[j] = tlsRule("same.example.com")
			}
			bundles = append(bundles, bundle)
		}
		if got, err := Compile("test", Filtered, nil, bundles, false, ownerKey()); err == nil || got.Fingerprint != "" {
			t.Fatal("duplicate definitions bypassed work budget", err)
		}
	})
	t.Run("variant features", func(t *testing.T) {
		var bundles []Bundle
		var features []string
		for j := 0; j < MaxConstraints; j++ {
			features = append(features, fmt.Sprintf("feature-%d", j))
		}
		for i := 0; i <= MaxFeatureExpansions/MaxConstraints; i++ {
			bundle := testBundle(ClientCLI, "key")
			bundle.Backend, bundle.Features = fmt.Sprintf("backend-%d", i), map[string][]Rule{}
			for _, feature := range features {
				bundle.Features[feature] = []Rule{tlsRule("same.example.com")}
			}
			bundles = append(bundles, bundle)
		}
		rules := []Rule{{To: Destination{Provider: "model", Features: features}}}
		if got, err := FeatureExpansions(rules, bundles); err == nil || got != nil {
			t.Fatal("shared destinations bypassed feature budget", err)
		}
		if got, err := Compile("test", Filtered, []Input{{Rules: rules, Origin: Origin{Kind: "operator"}}}, bundles, false, ownerKey()); err == nil || got.Fingerprint != "" {
			t.Fatal("compiler ignored feature budget", err)
		}
	})
	t.Run("grant origins", func(t *testing.T) {
		var inputs []Input
		var rules []Rule
		for j := 0; j <= MaxGrantOrigins/MaxRules; j++ {
			rules = append(rules, tlsRule(fmt.Sprintf("destination-%d.example.com", j)))
		}
		for i := 0; i < MaxRules; i++ {
			inputs = append(inputs, Input{Rules: rules, Origin: Origin{Kind: "operator", Name: fmt.Sprintf("source-%d", i)}})
		}
		if got, err := Compile("test", Filtered, inputs, nil, false, ownerKey()); err == nil || got.Fingerprint != "" {
			t.Fatal("few grants bypassed aggregate provenance budget", err)
		}
	})
}
