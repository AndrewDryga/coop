package agent

import (
	"errors"
	"fmt"

	"github.com/AndrewDryga/coop/internal/egress"
)

// NetworkBundleVersion identifies the release-owned endpoint set, not an
// adapter or CLI version. Changing any bundle's contents requires a new value:
// the same version with different content is integrity drift, never an update
// (networkstate.checkBundles pins each version's content on first admission).
const NetworkBundleVersion = "2026-09-13.1"

// NetworkBundleInput names an ALREADY SELECTED target: the client this run
// launches plus, when the operator was explicit, the backend and auth variant.
// Empty Backend or AuthMode means the adapter's own default selection.
//
// It deliberately carries no captured configuration files and no credential
// bytes. Connectivity follows from what the operator selected; a token's
// contents are evidence of nothing and are never network authority.
type NetworkBundleInput struct {
	Client   egress.Client
	Backend  string
	AuthMode string
}

// NetworkAuthSelection is adapter-owned, non-secret evidence of the credential
// family a concrete profile selected. EnvKey names the exact portable authority
// the host must prove present; RequirePortable asks the host to prove a projected
// file remains usable for the restricted credential horizon.
type NetworkAuthSelection struct {
	AuthMode        string
	EnvKey          string
	RequirePortable bool
}

// NetworkAuthSelector is implemented only by providers whose filtered bundle
// depends on the selected account's credential family. Providers with one
// already-qualified family keep using NetworkBundle's default.
type NetworkAuthSelector interface {
	NetworkAuthSelection(profileDir string, markerPresent bool) (NetworkAuthSelection, error)
}

// directNetworkBundle is the provider's own API surface, reached directly. A
// custom gateway or another auth family is a different tuple that must be
// qualified separately, so an override that does not match refuses here.
func directNetworkBundle(provider, family string, input NetworkBundleInput, domains, sources []string) (egress.Bundle, error) {
	if input.Client != egress.ClientCLI && input.Client != egress.ClientACP {
		return egress.Bundle{}, errors.New("restricted provider requires an explicit CLI or ACP client")
	}
	if input.Backend != "" && input.Backend != "direct" || input.AuthMode != "" && input.AuthMode != family {
		return egress.Bundle{}, fmt.Errorf("restricted %s does not support the selected backend or authentication override", provider)
	}
	bundle := egress.Bundle{Provider: provider, Client: input.Client, Backend: "direct", AuthMode: family,
		Version: NetworkBundleVersion, Sources: sources}
	for _, domain := range domains {
		bundle.Core = append(bundle.Core, egress.Rule{To: egress.Destination{Domain: domain}, Protocol: "tls", Ports: []int{443}})
	}
	return bundle, nil
}
