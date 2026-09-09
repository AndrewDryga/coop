package networkstate

import (
	"errors"
	"slices"
	"strings"
)

// RuntimeBinding is owner-private qualification identity, not a public runtime
// capability or permission to replay on another worker. No semver compatibility
// is inferred from these observations.
type RuntimeBinding struct {
	HostFamily      string   `json:"host_family"`
	Endpoint        string   `json:"endpoint"`
	DaemonID        string   `json:"daemon_id"`
	OS              string   `json:"os"`
	Architecture    string   `json:"architecture"`
	ServerVersion   string   `json:"server_version"`
	KernelVersion   string   `json:"kernel_version"`
	SecurityOptions []string `json:"security_options"`
}

// CandidateSpec contains construction observations from the bound host builder.
// Image labels, user-supplied tags and remote requests cannot supply these facts.
// The trusted builder is responsible for observing the exact returned images. It
// is inlined into the qualification that proves it; there is no separate record.
type CandidateSpec struct {
	Runtime          RuntimeBinding `json:"runtime"`
	ClientImage      string         `json:"client_image"`
	GatewayImage     string         `json:"gateway_image"`
	ClientDefinition string         `json:"client_definition"`
	ClientClosure    string         `json:"client_closure"`
	GatewaySource    string         `json:"gateway_source"`
	Libc             string         `json:"libc"`
	NodeBase         string         `json:"node_base"`
	GoBase           string         `json:"go_base"`
}

func canonicalRuntimeBinding(binding RuntimeBinding) (RuntimeBinding, error) {
	if !slices.Contains([]string{"linux", "darwin"}, binding.HostFamily) ||
		!localEndpoint(binding.Endpoint) || !safeRecordToken(binding.DaemonID, 128) ||
		binding.OS != "linux" || !slices.Contains([]string{"arm64", "amd64"}, binding.Architecture) ||
		!safeRecordToken(binding.ServerVersion, 128) || !safeRecordToken(binding.KernelVersion, 256) ||
		len(binding.SecurityOptions) > 32 {
		return RuntimeBinding{}, errors.New("invalid restricted-network runtime binding")
	}
	binding.SecurityOptions = append([]string{}, binding.SecurityOptions...)
	for _, option := range binding.SecurityOptions {
		if !safeRecordToken(option, 256) || strings.Contains(option, "rootless") || strings.Contains(option, "userns") {
			return RuntimeBinding{}, errors.New("runtime security options are outside the restricted-network candidate contract")
		}
	}
	slices.Sort(binding.SecurityOptions)
	binding.SecurityOptions = slices.Compact(binding.SecurityOptions)
	return binding, nil
}

// EqualRuntimeBinding canonicalizes order only. A daemon, endpoint, engine,
// kernel, host family or security-mode change needs separate qualification.
func EqualRuntimeBinding(a, b RuntimeBinding) bool {
	a, err := canonicalRuntimeBinding(a)
	if err != nil {
		return false
	}
	b, err = canonicalRuntimeBinding(b)
	return err == nil && equalJSON(a, b)
}

func imageDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && lowerHex(strings.TrimPrefix(value, "sha256:"), 64)
}

func pinnedBase(value string) bool {
	name, digest, ok := strings.Cut(value, "@")
	return ok && safeRecordToken(name, 256) && !strings.HasPrefix(name, "-") &&
		strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789/._:-") == "" && imageDigest(digest)
}

func canonicalCandidate(spec CandidateSpec) (CandidateSpec, error) {
	var err error
	spec.Runtime, err = canonicalRuntimeBinding(spec.Runtime)
	if err != nil {
		return CandidateSpec{}, err
	}
	if !imageDigest(spec.ClientImage) || !imageDigest(spec.GatewayImage) || spec.ClientImage == spec.GatewayImage ||
		!lowerHex(spec.ClientDefinition, 64) || !lowerHex(spec.ClientClosure, 64) || !lowerHex(spec.GatewaySource, 64) ||
		spec.Libc != "glibc" || !pinnedBase(spec.NodeBase) || !pinnedBase(spec.GoBase) {
		return CandidateSpec{}, errors.New("network candidate requires an exact constructed image pair and closure")
	}
	return spec, nil
}
