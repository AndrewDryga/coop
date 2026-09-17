package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// The capture is a reference the daemon writes and the child reads back. Every field has to
// survive that round trip: a dropped session or attempt id would leave the run unattributable,
// which is exactly what the daemon later checks the fingerprint against.
func TestSessionNetworkCaptureRoundTripsEveryField(t *testing.T) {
	want := SessionNetworkCapture{
		Project: "/srv/repos/app", Fingerprint: strings.Repeat("a", 64),
		Qualification: strings.Repeat("b", 64), SessionID: "remote_1234", AttemptID: "session-abc",
	}
	encoded, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSessionNetworkCapture(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("decoded capture = %+v, want %+v", got, want)
	}
}

func TestSessionNetworkCaptureRefusesIncompleteAndUnknownShapes(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown field": `{"project":"/srv/app","fingerprint":"` + strings.Repeat("a", 64) +
			`","qualification":"q","session_id":"s","attempt_id":"a","mode":"open"}`,
		"missing attempt": `{"project":"/srv/app","fingerprint":"` + strings.Repeat("a", 64) +
			`","qualification":"q","session_id":"s"}`,
		"not json":  "filtered",
		"too large": `{"project":"` + strings.Repeat("x", sessionNetworkCaptureLimit) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := decodeSessionNetworkCapture(raw); err == nil {
				t.Fatalf("accepted %s: %+v", name, got)
			}
		})
	}
	if _, err := (SessionNetworkCapture{Project: "/srv/app"}).Encode(); err == nil {
		t.Fatal("encoded an incomplete capture")
	}
}

// A child launched with no capture is every ordinary launch. It must stay exactly as it is at
// HEAD: no store opened, no authority read, no capture.
func TestCapturedEgressFromEnvironmentIsInertWithoutTheVariable(t *testing.T) {
	cfg, repo, _ := admissionFixture(t)
	t.Setenv(SessionNetworkCaptureEnv, "")
	capture, err := CapturedEgressFromEnvironment(cfg, RunSpec{Repo: repo})
	if err != nil || capture != nil {
		t.Fatalf("ordinary launch produced capture %+v, err %v", capture, err)
	}
}

// The environment names a snapshot; it does not carry one. Only the owner-private store can
// produce the named policy, so a child handed another project's fingerprint — or a fingerprint
// that was never admitted — cannot launch filtered.
func TestCapturedEgressFromEnvironmentAuthenticatesAgainstTheOwnerStore(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	mode := egress.Filtered
	policy, err := store.Admit(repo, networkstate.Admission{InvocationMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := SessionNetworkCapture{
		Project: repo, Fingerprint: policy.Fingerprint, Qualification: strings.Repeat("c", 64),
		SessionID: "remote_1234", AttemptID: "session-abc",
	}
	for name, test := range map[string]struct {
		capture SessionNetworkCapture
		reject  string
	}{
		// Admitted for this project: the snapshot loads, and the launch is refused one step
		// later, on the host setup record it names — which is what proves it got that far.
		"authentic reference": {capture: base, reject: "no longer set up the way this session was started"},
		"another project": {capture: func() SessionNetworkCapture {
			c := base
			c.Project = other
			return c
		}(), reject: "network rules are not on this host"},
		"unadmitted policy": {capture: func() SessionNetworkCapture {
			c := base
			c.Fingerprint = strings.Repeat("d", 64)
			return c
		}(), reject: "network rules are not on this host"},
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := test.capture.Encode()
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(SessionNetworkCaptureEnv, encoded)
			got, err := CapturedEgressFromEnvironment(cfg, RunSpec{Repo: repo})
			if got != nil {
				_ = got.Close()
				t.Fatalf("%s produced a capture", name)
			}
			if err == nil || !strings.Contains(err.Error(), test.reject) {
				t.Fatalf("%s error = %v, want one saying %q", name, err, test.reject)
			}
		})
	}
}

// A session policy is EXPLICIT operator authority. When it disagrees with the posture an operator
// remembered for the project, creation is refused: the API may not reconcile that on its own.
func TestAdmitSessionNetworkRefusesAPolicyThatDisagreesWithTheRememberedPosture(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	rememberFilteredPosture(t, root, repo, nil)
	workspace := sessionWorkspaceFixture(t, repo, "remote-1")
	_, capture, err := AdmitSessionNetwork(cfg, runtime.Runtime{Name: "must-not-execute"},
		RunSpec{Repo: workspace, PolicyRepo: repo, ForkName: "remote-1"},
		SessionNetworkAdmission{Mode: egress.Open})
	if capture != nil {
		_ = capture.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "network_policy_conflict") {
		t.Fatalf("open policy against a remembered restriction = %v, want a policy conflict", err)
	}
}

// A policy with no `egress:` block is not a request to widen anything, so the project's own
// remembered posture still decides — exactly as it does for a direct launch in that repository.
func TestAdmitSessionNetworkLetsTheRememberedPostureDecideWithoutAPolicyMode(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	rememberFilteredPosture(t, root, repo, nil)
	workspace := sessionWorkspaceFixture(t, repo, "remote-1")
	_, capture, err := AdmitSessionNetwork(cfg, runtime.Runtime{Name: "must-not-execute"},
		RunSpec{Repo: workspace, PolicyRepo: repo, ForkName: "remote-1"}, SessionNetworkAdmission{})
	if capture != nil {
		_ = capture.Close()
	}
	// Resolution reached filtered; this fixture's runtime is not Docker, so the runtime preflight
	// stops it before any authority state exists.
	if err == nil || !strings.Contains(err.Error(), filteredReachedRuntimePreflight) {
		t.Fatalf("silent policy under a remembered restriction = %v, want filtered resolution", err)
	}
}

// Admission must not mutate the daemon's shared configuration: one process serves every session,
// so a posture written onto cfg by one create would leak into the next.
func TestAdmitSessionNetworkLeavesTheSharedConfigurationAlone(t *testing.T) {
	cfg, repo, _ := admissionFixture(t)
	workspace := sessionWorkspaceFixture(t, repo, "remote-1")
	mode, capture, err := AdmitSessionNetwork(cfg, runtime.Runtime{Name: "must-not-execute"},
		RunSpec{Repo: workspace, PolicyRepo: repo, ForkName: "remote-1"},
		SessionNetworkAdmission{Mode: egress.Open})
	if capture != nil {
		_ = capture.Close()
	}
	if err != nil || mode != egress.Open {
		t.Fatalf("open session admission = %q, err %v", mode, err)
	}
	if cfg.Egress != "open" {
		t.Fatalf("shared configuration egress = %q, want it untouched", cfg.Egress)
	}
}

// sessionWorkspaceFixture creates the fork workspace a remote session's box would mount, so the
// admission spec describes the same two paths the child later presents.
func sessionWorkspaceFixture(t *testing.T, repo, fork string) string {
	t.Helper()
	workspace := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-forks", fork)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	return workspace
}

// A repository that asks for restricted egress gets it for its remote sessions too. The named
// policy outranks it when it writes a posture; when it stays silent, the project still speaks —
// exactly as it does for a direct launch in the same checkout.
func TestAdmitSessionNetworkHonorsTheProjectRequestedMode(t *testing.T) {
	cfg, repo, _ := admissionFixture(t)
	writeCopyFixture(t, filepath.Join(repo, ".agent", "project.yaml"), "box:\n  egress: filtered\n")
	workspace := sessionWorkspaceFixture(t, repo, "remote-1")
	spec := RunSpec{Repo: workspace, PolicyRepo: repo, ForkName: "remote-1"}
	// Resolution reached filtered; this fixture's runtime is not Docker, so the runtime preflight
	// stops it.
	if _, capture, err := AdmitSessionNetwork(cfg, runtime.Runtime{Name: "must-not-execute"},
		spec, SessionNetworkAdmission{}); capture != nil || err == nil ||
		!strings.Contains(err.Error(), filteredReachedRuntimePreflight) {
		t.Fatalf("project-requested filtered = %v", err)
	}
	// An explicit policy posture is operator authority and outranks the repository's request.
	mode, capture, err := AdmitSessionNetwork(cfg, runtime.Runtime{Name: "must-not-execute"},
		spec, SessionNetworkAdmission{Mode: egress.Open})
	if capture != nil {
		_ = capture.Close()
	}
	if err != nil || mode != egress.Open {
		t.Fatalf("named open policy over a project request = %q, err %v", mode, err)
	}
}

// The destinations a session may reach are the ones its box can actually use. A policy that
// withholds the shared MCP configuration withholds its hosts with it.
func TestSessionAutomaticDependenciesFollowTheMCPProjection(t *testing.T) {
	cfg, spec := mcpDependencyFixture(t, `{"mcpServers":{"remote":{"url":"https://mcp.example.com"}}}`)
	automatic, err := sessionAutomaticDependencies(cfg, spec, SessionNetworkAdmission{})
	if err != nil || len(automatic) != 1 || automatic[0].Rules[0].To.Domain != "mcp.example.com" {
		t.Fatalf("session with MCP derived %+v, err %v", automatic, err)
	}
	if automatic, err = sessionAutomaticDependencies(cfg, spec, SessionNetworkAdmission{OmitMCP: true}); err != nil || automatic != nil {
		t.Fatalf("session policy with project_mcp: false derived %+v, err %v", automatic, err)
	}
}
