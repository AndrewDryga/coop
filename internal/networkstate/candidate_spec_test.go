package networkstate

import (
	"slices"
	"strings"
	"testing"
)

func candidateFixture() CandidateSpec {
	return CandidateSpec{Runtime: RuntimeBinding{HostFamily: "darwin", Endpoint: "unix:///fixture.sock", DaemonID: "fixture-daemon",
		OS: "linux", Architecture: "arm64", ServerVersion: "29.4.0", KernelVersion: "7.0.14-fixture", SecurityOptions: []string{"name=seccomp,profile=builtin", "name=cgroupns"}},
		ClientImage: "sha256:" + strings.Repeat("a", 64), GatewayImage: "sha256:" + strings.Repeat("b", 64),
		ClientDefinition: strings.Repeat("c", 64), ClientClosure: strings.Repeat("d", 64), GatewaySource: strings.Repeat("e", 64),
		Libc: "glibc", NodeBase: "node:24-slim@sha256:" + strings.Repeat("f", 64), GoBase: "golang:1.26.6-bookworm@sha256:" + strings.Repeat("0", 64)}
}

func TestCandidateSpecRejectsInexactConstruction(t *testing.T) {
	cases := map[string]func(*CandidateSpec){
		"client tag":     func(s *CandidateSpec) { s.ClientImage = "coop-clients:tag" },
		"helper tag":     func(s *CandidateSpec) { s.GatewayImage = "coop-network:tag" },
		"same image":     func(s *CandidateSpec) { s.GatewayImage = s.ClientImage },
		"definition":     func(s *CandidateSpec) { s.ClientDefinition = strings.Repeat("C", 64) },
		"closure":        func(s *CandidateSpec) { s.ClientClosure = "" },
		"helper source":  func(s *CandidateSpec) { s.GatewaySource = "latest" },
		"base tag":       func(s *CandidateSpec) { s.NodeBase = "node:24-slim" },
		"base control":   func(s *CandidateSpec) { s.GoBase = "evil\n@" + s.ClientImage },
		"libc":           func(s *CandidateSpec) { s.Libc = "musl" },
		"host":           func(s *CandidateSpec) { s.Runtime.HostFamily = "windows" },
		"remote":         func(s *CandidateSpec) { s.Runtime.Endpoint = "tcp://daemon:2375" },
		"daemon":         func(s *CandidateSpec) { s.Runtime.DaemonID = "" },
		"platform":       func(s *CandidateSpec) { s.Runtime.OS = "windows" },
		"arch":           func(s *CandidateSpec) { s.Runtime.Architecture = "x86_64" },
		"server":         func(s *CandidateSpec) { s.Runtime.ServerVersion = "\x1b[0m" },
		"kernel":         func(s *CandidateSpec) { s.Runtime.KernelVersion = "" },
		"rootless":       func(s *CandidateSpec) { s.Runtime.SecurityOptions = []string{"name=rootless"} },
		"userns":         func(s *CandidateSpec) { s.Runtime.SecurityOptions = []string{"name=userns"} },
		"security bound": func(s *CandidateSpec) { s.Runtime.SecurityOptions = make([]string, 33) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			spec := candidateFixture()
			change(&spec)
			if got, err := canonicalCandidate(spec); err == nil || got.ClientImage != "" {
				t.Fatal("inexact construction accepted")
			}
		})
	}
}

func TestCandidateSpecCanonicalizationDoesNotMutateTheCaller(t *testing.T) {
	input := candidateFixture()
	canonical, err := canonicalCandidate(input)
	if err != nil {
		t.Fatal(err)
	}
	if input.Runtime.SecurityOptions[0] != "name=seccomp,profile=builtin" {
		t.Fatal("caller runtime observation was mutated")
	}
	input.Runtime.SecurityOptions[0] = "changed"
	if canonical.Runtime.SecurityOptions[0] == "changed" {
		t.Fatal("canonical candidate shares the caller's slice")
	}
}

func TestRuntimeBindingOrderingOnlyIsEqual(t *testing.T) {
	original := candidateFixture().Runtime
	reordered := candidateFixture().Runtime
	slices.Reverse(reordered.SecurityOptions)
	if !EqualRuntimeBinding(original, reordered) {
		t.Fatal("security option ordering changed runtime")
	}
	changes := []func(*RuntimeBinding){
		func(r *RuntimeBinding) { r.HostFamily = "linux" },
		func(r *RuntimeBinding) { r.Endpoint = "unix:///other.sock" },
		func(r *RuntimeBinding) { r.DaemonID = "other-daemon" },
		func(r *RuntimeBinding) { r.Architecture = "amd64" },
		func(r *RuntimeBinding) { r.ServerVersion = "29.4.1" },
		func(r *RuntimeBinding) { r.KernelVersion = "7.0.15-fixture" },
		func(r *RuntimeBinding) { r.SecurityOptions = nil },
	}
	for _, change := range changes {
		changed := candidateFixture().Runtime
		change(&changed)
		if EqualRuntimeBinding(original, changed) {
			t.Fatal("unqualified runtime change accepted", changed)
		}
	}
}
