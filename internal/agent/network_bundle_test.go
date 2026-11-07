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
const pinnedNetworkBundleVersion = "2026-11-07.1"

var pinnedNetworkBundles = map[string]string{
	"claude/cli": "76aa80e4c58104b7b31d9838dbb941a0394858792c4c5db27f7bcf5bbd97a5c1",
	"claude/acp": "b2ea6ec4bf984182d70c005e8a3fb4ec3c79bea484b75b579ed988a529b867f2",
	"codex/cli":  "67fe3680236e60c76a17dad03d03a3fe481a3f9fba8c08a814a5845474468d21",
	"codex/acp":  "2962e1727ca3243d43f9d1968dccad295dbf69974d629a4d766709077b81620e",
	"gemini/cli": "30342592d19dd03fee8fd28e5aa0b65731e83d7a07afd93da42ea462bb32de12",
	"gemini/acp": "fc782c95e1f868d4d7e990c9403e9fd58b8c3d1465af36e467152d146f0dce06",
	"grok/cli":   "8b641d6caa9a176918f92d288713d38fe416a4a85a024041a79a2b862a08248f",
	"grok/acp":   "795497beb094dbe5e0db8a8915670db3fa646dced73a0ec24dc514c257931fdf",
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
