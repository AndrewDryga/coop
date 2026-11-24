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
const pinnedNetworkBundleVersion = "2026-11-24.1"

var pinnedNetworkBundles = map[string]string{
	"claude/cli": "e0c7e53cf5220d8a5940ae8da1ef6a5e05c11d4ce187d861e1d452505cd71209",
	"claude/acp": "cc402cc3cd3189f328d0802ffd982d34cd5687c86934063302e11b24064b5af0",
	"codex/cli":  "f6b6bb02820bed5cf0a94b365c6fe9829d8628e70cb47ac9f4354e15fd6b982f",
	"codex/acp":  "c0093d2847621434d2792422b5fb6c6cea3dab2f099cda89657e059bf57707a4",
	"gemini/cli": "eb7af90153554bb5487395f8affbef2ea69a32a07e51ba5694f9d4c5878fd4db",
	"gemini/acp": "be4e9ae37bed8c3b861ddd19d2037f45ca34ee8dd995f7a1f9931535a0ed1a04",
	"grok/cli":   "1cdc7f6ab82a3227756810a541dc72ec9a1416af999fe924ae36dec767c1c708",
	"grok/acp":   "8189f0be63e915b80918d02aa50f356989e0d8b6d2bcf2319a5ac12594489c25",
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
