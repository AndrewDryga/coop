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

func TestControllerJobEnvironmentRequiresExactDigest(t *testing.T) {
	t.Setenv(ControllerJobEnv, strings.Repeat("a", 64))
	if selected, err := ControllerJobFromEnvironment(); err != nil || !selected {
		t.Fatalf("valid controller job marker = %t, %v", selected, err)
	}
	for _, invalid := range []string{"short", strings.Repeat("A", 64), strings.Repeat("x", 64)} {
		t.Setenv(ControllerJobEnv, invalid)
		if selected, err := ControllerJobFromEnvironment(); err == nil || selected {
			t.Fatalf("invalid controller job marker %q = %t, %v", invalid, selected, err)
		}
	}
}

func TestControllerJobNetworkingIgnoresRepositoryAndLocalPosture(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("box: [invalid local settings]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rememberFilteredPosture(t, root, repo, nil)
	spec := RunSpec{Repo: repo, PolicyRepo: repo, ControllerJob: true}
	job := ControllerJobNetwork{JobDigest: strings.Repeat("a", 64), SessionID: "remote_one", Mode: egress.Open}
	mode, capture, err := AdmitControllerJobNetwork(cfg, runtime.Runtime{Name: "must-not-execute"}, spec, job)
	if err != nil || mode != egress.Open || capture != nil {
		t.Fatalf("controller open networking = %q %+v %v", mode, capture, err)
	}
	job.Mode = egress.None
	mode, capture, err = AdmitControllerJobNetwork(cfg, runtime.Runtime{Name: "must-not-execute"}, spec, job)
	if err != nil || mode != egress.None || capture != nil {
		t.Fatalf("controller offline networking = %q %+v %v", mode, capture, err)
	}
	job.Mode = egress.Filtered
	if _, capture, err = AdmitControllerJobNetwork(cfg, runtime.Runtime{Name: "must-not-execute"}, spec, job); capture != nil || err == nil || !strings.Contains(err.Error(), filteredReachedRuntimePreflight) {
		t.Fatalf("controller filtered runtime preflight = %+v %v", capture, err)
	}
}

func TestControllerJobChildReprovesItsOwnSnapshot(t *testing.T) {
	cfg, repo, root := admissionFixture(t)
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref := networkstate.JobSnapshotRef{JobDigest: strings.Repeat("a", 64), SessionID: "remote_one"}
	snapshot, err := store.CaptureJob(ref, []egress.Rule{{To: egress.Destination{Domain: "job.example.com"}, Protocol: "tls", Ports: []int{443}}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ControllerJobEnv, ref.JobDigest)
	reference := SessionNetworkCapture{Project: repo, Fingerprint: snapshot.Fingerprint,
		Qualification: strings.Repeat("b", 64), SessionID: ref.SessionID, AttemptID: "turn_one", JobDigest: ref.JobDigest}
	encoded, err := reference.Encode()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(SessionNetworkCaptureEnv, encoded)
	if capture, err := CapturedEgressFromEnvironment(cfg, RunSpec{Repo: repo, ControllerJob: true}); capture != nil || err == nil || !strings.Contains(err.Error(), "no longer set up") {
		if capture != nil {
			_ = capture.Close()
		}
		t.Fatalf("authentic job snapshot reached qualification = %+v %v", capture, err)
	}
	reference.SessionID = "remote_other"
	encoded, err = reference.Encode()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(SessionNetworkCaptureEnv, encoded)
	if capture, err := CapturedEgressFromEnvironment(cfg, RunSpec{Repo: repo, ControllerJob: true}); capture != nil || err == nil || !strings.Contains(err.Error(), "not on this host") {
		if capture != nil {
			_ = capture.Close()
		}
		t.Fatalf("wrong job session reused snapshot = %+v %v", capture, err)
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
