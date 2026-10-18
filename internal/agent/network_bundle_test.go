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
const pinnedNetworkBundleVersion = "2026-09-30.1"

var pinnedNetworkBundles = map[string]string{
	"claude/cli": "a203f7554bb784cef595b531c2c3de3fd427b98b2e6ede966905ecfd6d63c255",
	"claude/acp": "66b121ba12c10752b0bef26f091769760d21a3260e1a56ebde1d91bc5c3f1d54",
	"codex/cli":  "891500cf45e9f5eb658cae30dbe3a7bc9225f2ea7777de64baf5d349a29cde2b",
	"codex/acp":  "eab8dbdad826fa5a4e76f8562eb1ab63ac8ace5fd782c6efc639679a38f26d1f",
	"gemini/cli": "ea33953ee1df2819dba07e8d0311fb2489c66cf6516e94c5d0b1eb451bc9fd76",
	"gemini/acp": "3036676716e368050e9ec1c45d74e99ad6523470c75ca14ad7e8b15754b5615f",
	"grok/cli":   "081c35c9d84eed84217f99f7bdd082d6fdebcdc3c00332e5b0c09144e3bdff83",
	"grok/acp":   "a6e31dc9e680865b020f05a8cfd12454ca5ba0ec8d08a4d6d1d5fe9aabf22f14",
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
