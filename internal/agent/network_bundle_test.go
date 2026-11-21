package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

// pinnedNetworkBundles is each release-owned bundle's canonical content under one
// NetworkBundleVersion: the sha256 of its JSON after egress.SelectedBundles, by provider/client.
const pinnedNetworkBundleVersion = "2026-11-21.1"

var pinnedNetworkBundles = map[string]string{
	"claude/cli": "73215887895726e47241ca0616b6f636906ecc165c77f578ecd83dd9b2e49c82",
	"claude/acp": "0f9a4f5fbf8ee3793f1f1b023569fce1d6d2379cce14a09c698a8de0faf65727",
	"codex/cli":  "fd318c522ce8f260b519474c967492f17e89b29f38b6bd8f4d841e6c8f1ced80",
	"codex/acp":  "bfb1fa9b8911483ed9de57e903cd23a82a3629d42c57845a6eb07f4d22956e7c",
	"gemini/cli": "cadfc59464024abec0ff78b6b326b367c4eb9f2808af3862b8d05de36c9d45ad",
	"gemini/acp": "b2e3e70e62cfe43e9355ef95954fba3aff5fa52aa372448bbbddc26b5afb24df",
	"grok/cli":   "317cf8a565a21af6da61f716eb2ea386e4c6addd3997eadb7fab3b416adc4f28",
	"grok/acp":   "7a59ceb0829a8fca95c090cd9e1aa61b25e578c186f3be6f7a0516a47b6efc36",
}

// A bundle's content under its version never changes: a new endpoint set needs a new
// NetworkBundleVersion, so a host never meets two contents under one release identity. This pins
// what each host once pinned on first admission.
func TestNetworkBundlesChangeOnlyWithANewVersion(t *testing.T) {
	bumped := NetworkBundleVersion != pinnedNetworkBundleVersion
	if bumped {
		t.Errorf("NetworkBundleVersion is %s: set pinnedNetworkBundleVersion to it and each digest to the one reported below", NetworkBundleVersion)
	}
	seen := map[string]bool{}
	for _, name := range Names() {
		ag, _ := Get(name)
		for _, client := range []egress.Client{egress.ClientCLI, egress.ClientACP} {
			key := name + "/" + string(client)
			bundle, err := ag.NetworkBundle(NetworkBundleInput{Client: client})
			if err != nil {
				t.Fatalf("%s: %v", key, err)
			}
			selected, err := egress.SelectedBundles([]egress.Bundle{bundle})
			if err != nil {
				t.Fatalf("%s: %v", key, err)
			}
			data, err := json.Marshal(selected[0])
			if err != nil {
				t.Fatal(err)
			}
			got := fmt.Sprintf("%x", sha256.Sum256(data))
			switch want, ok := pinnedNetworkBundles[key]; {
			case bumped:
				t.Logf("%q: %q,", key, got)
			case !ok || got != want:
				t.Errorf("%s bundle is %s under NetworkBundleVersion %s, pinned %q: changed endpoints need a new NetworkBundleVersion",
					key, got, NetworkBundleVersion, want)
			}
			seen[key] = true
		}
	}
	for key := range pinnedNetworkBundles {
		if !seen[key] {
			t.Errorf("%s is pinned but no adapter builds it any more: remove its digest", key)
		}
	}
}
