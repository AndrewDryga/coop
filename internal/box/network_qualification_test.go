package box

import (
	"context"
	"reflect"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/gatewayimage"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestNetworkQualificationBindsCurrentDefinitionAndEntireRuntime(t *testing.T) {
	platform := agents.ClientPlatform{OS: "linux", Architecture: "arm64", Libc: "glibc"}
	definition, _, closure, err := lockedImageDefinition(platform)
	if err != nil {
		t.Fatal(err)
	}
	binding := networkstate.RuntimeBinding{HostFamily: "darwin", Endpoint: "unix:///fixture.sock", DaemonID: "fixture", OS: "linux", Architecture: "arm64",
		ServerVersion: "29.4.0", KernelVersion: "fixture", SecurityOptions: []string{"name=seccomp,profile=builtin"}}
	candidate := networkstate.CandidateSpec{
		Runtime: binding, ClientImage: "sha256:" + strings.Repeat("b", 64), GatewayImage: "sha256:" + strings.Repeat("c", 64),
		ClientDefinition: definition.Labels["coop.clients.definition"], ClientClosure: closure.Digest, GatewaySource: gatewayimage.Fingerprint(),
		Libc: "glibc", NodeBase: pinnedNodeImage, GoBase: pinnedGoImage}
	for _, override := range []string{"", candidate.ClientImage} {
		if err := verifyNetworkCandidate(candidate, binding, override); err != nil {
			t.Fatal("current exact binding rejected", err)
		}
	}
	for _, override := range []string{"coop-clients:latest", "sha256:" + strings.Repeat("f", 64), candidate.GatewayImage} {
		if err := verifyNetworkCandidate(candidate, binding, override); err == nil {
			t.Fatal("foreign image override accepted")
		}
	}
	for name, change := range map[string]func(*networkstate.RuntimeBinding){
		"host":     func(b *networkstate.RuntimeBinding) { b.HostFamily = "linux" },
		"endpoint": func(b *networkstate.RuntimeBinding) { b.Endpoint = "unix:///changed.sock" },
		"daemon":   func(b *networkstate.RuntimeBinding) { b.DaemonID = "changed" },
		"arch":     func(b *networkstate.RuntimeBinding) { b.Architecture = "amd64" },
		"engine":   func(b *networkstate.RuntimeBinding) { b.ServerVersion = "29.4.1" },
		"kernel":   func(b *networkstate.RuntimeBinding) { b.KernelVersion = "changed" },
		"security": func(b *networkstate.RuntimeBinding) { b.SecurityOptions = nil },
	} {
		t.Run(name, func(t *testing.T) {
			changed := binding
			change(&changed)
			if err := verifyNetworkCandidate(candidate, changed, ""); err == nil {
				t.Fatal("changed runtime inherited qualification")
			}
		})
	}
	for name, change := range map[string]func(*networkstate.CandidateSpec){
		"definition": func(c *networkstate.CandidateSpec) { c.ClientDefinition = strings.Repeat("f", 64) },
		"closure":    func(c *networkstate.CandidateSpec) { c.ClientClosure = strings.Repeat("f", 64) },
		"gateway":    func(c *networkstate.CandidateSpec) { c.GatewaySource = strings.Repeat("f", 64) },
		"node":       func(c *networkstate.CandidateSpec) { c.NodeBase = "node:latest" },
		"go":         func(c *networkstate.CandidateSpec) { c.GoBase = "golang:latest" },
		"libc":       func(c *networkstate.CandidateSpec) { c.Libc = "musl" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := candidate
			change(&changed)
			if err := verifyNetworkCandidate(changed, binding, ""); err == nil {
				t.Fatal("stale definition inherited qualification")
			}
		})
	}
}

func TestNetworkTrialCannotFallThroughToOpenRun(t *testing.T) {
	code, err := runWithNetworkTrial(&config.Config{}, runtime.Runtime{}, RunSpec{Ctx: context.Background()}, defaultCompositionArtifactOps(), &networkTrialLaunch{})
	if code != -1 || err == nil || !strings.Contains(err.Error(), "requires a filtered capture") {
		t.Fatal("trial reached ordinary host work", code, err)
	}
	for _, name := range []string{"AllowUnqualified", "Trial", "TrialPurpose"} {
		if _, found := reflect.TypeFor[RunSpec]().FieldByName(name); found {
			t.Fatal("public run spec contains trial bypass")
		}
	}
}

// A built image pair is construction, not proof. Only a completed qualification
// record can hand a workload an image, and a missing or forged reference fails.
func TestNetworkCaptureRequiresACompletedQualification(t *testing.T) {
	f, d := filteredFixture(t)
	capture := &CapturedEgress{Store: f.store, Project: f.record.Project, Fingerprint: f.policy.Fingerprint}
	for _, id := range []string{"", strings.Repeat("f", 64)} {
		capture.QualificationID = id
		if got, err := capturedNetworkCandidate(capture, f.policy, "none", nil); err == nil || got.ClientImage != "" {
			t.Fatal("missing or forged reference became qualification")
		}
	}
	// A trial launch carries its own unproven candidate, and only when the
	// capture claims no completed qualification of its own.
	trial := &networkTrialLaunch{authority: d.trial}
	got, err := capturedNetworkCandidate(&CapturedEgress{Store: f.store}, f.policy, "none", trial)
	if err != nil || got.ClientImage != f.image {
		t.Fatal("trial permit lost its candidate", got, err)
	}
	if _, err := capturedNetworkCandidate(capture, f.policy, "none", trial); err == nil {
		t.Fatal("trial permit reused a workload qualification reference")
	}
}
