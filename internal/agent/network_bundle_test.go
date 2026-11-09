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
const pinnedNetworkBundleVersion = "2026-11-08.1"

var pinnedNetworkBundles = map[string]string{
	"claude/cli": "55448cad92c7a577504f2e5a17e412510d122e3d294c00e943aa1b7ebe31c1ac",
	"claude/acp": "0c1bdb5d59b0b074aca38e2b49bdfbac3c4d6e973ee3aab59ba88b69f01c5c30",
	"codex/cli":  "2811eb2fbee0570ef54e075e3bc36ec33fe0c6c127e98753c0ba6b34020707e7",
	"codex/acp":  "f26a4b152b771c4c9208747439575f427cc7ec5d80bd1aaf799a68215e5b7c73",
	"gemini/cli": "4f0a58ed1cada292509198dc2fb673af4cf94c1217f378dfb9fb0b94d81954d6",
	"gemini/acp": "06832a8bfacec8a9bba56ec5d756a5f09e22c676068fc61e878bd4c879168252",
	"grok/cli":   "63c55644afce9a91ae92e2d13a1d4c4f4abce530911c8d168ca58d1909acc9d1",
	"grok/acp":   "819ae731415fdf36617120aa7dae8e05efc06860328d4fce8ec0e74397ad93ad",
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
