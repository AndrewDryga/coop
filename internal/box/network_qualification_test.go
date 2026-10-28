package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

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
		Libc: "glibc", NodeBase: pinnedNodeImage, GoBase: gatewayimage.GoBase()}
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

func TestNetworkSmokeCannotFallThroughToOpenRun(t *testing.T) {
	code, err := runWithNetworkSmoke(&config.Config{}, runtime.Runtime{}, RunSpec{Ctx: context.Background()}, defaultCompositionArtifactOps(), &networkSmokeLaunch{})
	if code != -1 || err == nil || !strings.Contains(err.Error(), "requires a filtered capture") {
		t.Fatal("preflight reached ordinary host work", code, err)
	}
	for _, name := range []string{"AllowUnqualified", "Smoke", "SmokePurpose"} {
		if _, found := reflect.TypeFor[RunSpec]().FieldByName(name); found {
			t.Fatal("public run spec contains a preflight bypass")
		}
	}
}

// A built image pair is construction, not proof. Only a completed host record
// can hand a workload an image, and a missing or forged reference fails.
func TestNetworkCaptureRequiresACompletedQualification(t *testing.T) {
	f, d := filteredFixture(t)
	capture := &CapturedEgress{Store: f.store, Project: f.record.Project, Fingerprint: f.policy.Fingerprint}
	for _, id := range []string{"", strings.Repeat("f", 64)} {
		capture.QualificationID = id
		if got, err := capturedNetworkCandidate(capture, f.policy, nil); err == nil || got.ClientImage != "" {
			t.Fatal("missing or forged reference became qualification")
		}
	}
	// A preflight launch carries its own unproven candidate, and only when the
	// capture claims no completed record of its own.
	smoke := &networkSmokeLaunch{authority: d.smoke}
	got, err := capturedNetworkCandidate(&CapturedEgress{Store: f.store}, f.policy, smoke)
	if err != nil || got.ClientImage != f.image {
		t.Fatal("preflight permit lost its candidate", got, err)
	}
	if _, err := capturedNetworkCandidate(capture, f.policy, smoke); err == nil {
		t.Fatal("preflight permit reused a workload record reference")
	}
}

func qualificationInventoryFixture(t *testing.T) (runtime.Runtime, *runtime.Docker, networkstate.CandidateSpec, string) {
	t.Helper()
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", dockersock.Serve(t, func() (dockersock.Info, error) {
		return dockersock.Info{ID: "fixture-daemon", ServerVersion: "29.4.0", KernelVersion: "fixture", OSType: "linux", Architecture: "aarch64"}, nil
	}))
	images := t.TempDir()
	shim := filepath.Join(t.TempDir(), "docker")
	body := "#!/bin/sh\nwhile [ \"$1\" = --host ] || [ \"$1\" = --config ]; do shift 2; done\n" +
		"case \"$1 $2\" in \"image inspect\") for image in \"$@\"; do :; done; cat " + strconv.Quote(images) + "/\"${image#sha256:}\" ;; *) exit 91 ;; esac\n"
	if err := os.WriteFile(shim, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	rt := runtime.Runtime{Name: shim}
	docker, err := runtime.InspectDocker(t.Context(), rt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docker.Close() })
	binding := networkRuntimeBinding(docker.Info(), docker.Endpoint())
	definition, _, closure, err := lockedImageDefinition(agents.ClientPlatform{OS: binding.OS, Architecture: binding.Architecture, Libc: "glibc"})
	if err != nil {
		t.Fatal(err)
	}
	candidate := networkstate.CandidateSpec{Runtime: binding, ClientImage: "sha256:" + strings.Repeat("b", 64), GatewayImage: "sha256:" + strings.Repeat("a", 64), ClientDefinition: definition.Labels["coop.clients.definition"], ClientClosure: closure.Digest, GatewaySource: gatewayimage.Fingerprint(), Libc: "glibc", NodeBase: pinnedNodeImage, GoBase: gatewayimage.GoBase()}
	return rt, docker, candidate, images
}

func writeQualificationImageFixture(t *testing.T, dir, requested, actual string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"ID": actual, "Labels": map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, strings.TrimPrefix(requested, "sha256:")), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// Synthetic persisted record for selection/admission tests, not runtime smoke proof.
func writeQualificationFixture(t *testing.T, store *networkstate.Store, candidate networkstate.CandidateSpec, client egress.Client, at time.Time) networkstate.Qualification {
	t.Helper()
	q := networkstate.Qualification{Version: 2, Contract: networkstate.QualificationContract, Candidate: candidate, Clients: []networkstate.QualifiedClient{{Provider: "claude", Client: client, Version: "fixture"}}, SmokeRun: strings.Repeat("1", 32), CompletedAt: at.UTC()}
	data, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(append([]byte("network-qualification-v2\x00"), data...))
	q.ID = hex.EncodeToString(sum[:])
	data, err = json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Path(), "qualification-"+q.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := store.Qualification(q.ID)
	if err != nil {
		t.Fatal("invalid synthetic qualification fixture", err)
	}
	return got
}

func TestCurrentQualificationSelectsNewestCompatiblePresentImagePair(t *testing.T) {
	for _, name := range []string{"newest", "stale definition", "other daemon", "missing client", "missing gateway", "replaced client", "wrong client kind", "none compatible"} {
		t.Run(name, func(t *testing.T) {
			_, docker, base, images := qualificationInventoryFixture(t)
			store, err := networkstate.Open(filepath.Join(t.TempDir(), "network"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			older := writeQualificationFixture(t, store, base, egress.ClientACP, stamp)
			for _, id := range []string{base.ClientImage, base.GatewayImage} {
				writeQualificationImageFixture(t, images, id, id)
			}
			candidate := base
			candidate.ClientImage = "sha256:" + strings.Repeat("c", 64)
			candidate.GatewayImage = "sha256:" + strings.Repeat("d", 64)
			client := egress.ClientACP
			switch name {
			case "stale definition", "none compatible":
				candidate.ClientDefinition = strings.Repeat("f", 64)
			case "other daemon":
				candidate.Runtime.DaemonID = "another-daemon"
			case "wrong client kind":
				client = egress.ClientCLI
			}
			newer := writeQualificationFixture(t, store, candidate, client, stamp.Add(time.Hour))
			if name != "missing client" {
				actual := candidate.ClientImage
				if name == "replaced client" {
					actual = "sha256:" + strings.Repeat("e", 64)
				}
				writeQualificationImageFixture(t, images, candidate.ClientImage, actual)
			}
			if name != "missing gateway" {
				writeQualificationImageFixture(t, images, candidate.GatewayImage, candidate.GatewayImage)
			}
			if name == "none compatible" {
				if err := os.Remove(filepath.Join(store.Path(), "qualification-"+older.ID+".json")); err != nil {
					t.Fatal(err)
				}
			}
			policy := egress.Snapshot{Mode: egress.Filtered, Dependencies: []egress.Dependency{{Provider: "claude", Client: egress.ClientACP}}}
			got, err := currentQualification(t.Context(), docker, store, policy, "")
			if err != nil {
				t.Fatal(err)
			}
			if name == "none compatible" {
				if got != nil {
					t.Fatalf("unqualified candidate selected: %+v", got)
				}
				return
			}
			want := older.ID
			if name == "newest" {
				want = newer.ID
			}
			if got == nil || got.ID != want {
				t.Fatalf("selected %+v, want %s", got, want)
			}
		})
	}
}
