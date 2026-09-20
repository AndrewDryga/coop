package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/eval"
)

func gradeApp(t *testing.T) *app {
	t.Helper()
	return &app{cfg: &config.Config{Egress: "open"}}
}

func verifierDir(t *testing.T, entry, body string, perm os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, entry), []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The three-valued contract: 0 passes, 1 fails, and ANY other exit is a grading error — a broken
// verifier must never read as a model regression.
func TestGradeSnapshotVerdictMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		err  error
		want eval.TrialStatus
	}{
		{"exit 0 passes", 0, nil, eval.TrialPassed},
		{"exit 1 fails", 1, nil, eval.TrialFailed},
		{"exit 2 is a grading error, not a failure", 2, nil, eval.TrialError},
		{"exit 127 (missing dependency) is a grading error", 127, nil, eval.TrialError},
		{"a launch failure is a grading error", -1, errors.New("no such image"), eval.TrialError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := gradeApp(t)
			dir := verifierDir(t, "verify.sh", "exit 0\n", 0o644)
			got := a.gradeSnapshot(context.Background(), gradeRequest{
				Image: "coop-box:test", Workspace: t.TempDir(), Verifier: dir, CaseID: "c",
			}, func(box.RunSpec) (int, error) { return tc.code, tc.err })
			if got.Status != tc.want {
				t.Errorf("status = %q, want %q (detail %q)", got.Status, tc.want, got.Detail)
			}
		})
	}
}

// A non-zero exit that is not 1 must SAY it is a grading error, so a reader is never left thinking
// the model failed.
func TestGradeSnapshotExplainsABrokenVerifier(t *testing.T) {
	a := gradeApp(t)
	dir := verifierDir(t, "verify.sh", "exit 0\n", 0o644)
	got := a.gradeSnapshot(context.Background(), gradeRequest{Image: "i", Workspace: t.TempDir(), Verifier: dir},
		func(box.RunSpec) (int, error) { return 3, nil })
	if !strings.Contains(got.Detail, "grading error") || !strings.Contains(got.Detail, "not a model failure") {
		t.Errorf("a broken verifier was not explained as a grading error: %q", got.Detail)
	}
}

// The grading container must carry no model credentials, no network, and must mount the verifier
// read-only at a path outside the workspace. This is the isolation the whole eval rests on.
func TestGradeSnapshotSandboxIsolation(t *testing.T) {
	a := gradeApp(t)
	dir := verifierDir(t, "verify.sh", "exit 0\n", 0o644)
	ws := t.TempDir()
	var got box.RunSpec
	policyEmpty := false
	a.gradeSnapshot(context.Background(), gradeRequest{Image: "coop-box:trusted", Workspace: ws, Verifier: dir},
		func(spec box.RunSpec) (int, error) {
			got = spec
			// Checked HERE: the policy directory only exists for the duration of the launch.
			entries, err := os.ReadDir(spec.PolicyRepo)
			policyEmpty = err == nil && len(entries) == 0
			return 0, nil
		})

	if got.Homes {
		t.Error("the grading box was given agent credentials — a verifier must never reach the user's tokens")
	}
	if !got.Batch || !got.Quiet {
		t.Errorf("grading should run batch+quiet, got batch=%v quiet=%v", got.Batch, got.Quiet)
	}
	if got.Network {
		t.Error("the grading box asked for a network")
	}
	if got.Repo != ws || got.Workdir != gradeWorkspaceDir {
		t.Errorf("workspace mount = %q at %q", got.Repo, got.Workdir)
	}
	mount := strings.Join(got.ExtraArgs, " ")
	if !strings.Contains(mount, dir+":"+gradeVerifierDir+":ro") {
		t.Errorf("verifier is not mounted read-only at the hidden path: %v", got.ExtraArgs)
	}
	// The verifier path must not be reachable from inside the workspace mount, or a candidate that
	// walked its own tree in an earlier phase could have found it.
	if strings.HasPrefix(gradeVerifierDir, gradeWorkspaceDir+"/") || gradeVerifierDir == gradeWorkspaceDir {
		t.Errorf("the verifier mount %q sits inside the candidate workspace %q", gradeVerifierDir, gradeWorkspaceDir)
	}
	if got.Cmd[len(got.Cmd)-1] != gradeVerifierDir+"/verify.sh" || got.Cmd[0] != "/bin/sh" {
		t.Errorf("verifier command = %v", got.Cmd)
	}
	if got.Cache {
		t.Error("the grading box was given the shared cache volume")
	}
	// The grader's box policy must not come from the candidate's snapshot: a .agent/project.yaml
	// there could re-open the network or rewrite PATH.
	if got.PolicyRepo == "" || got.PolicyRepo == got.Repo {
		t.Errorf("grading policy repo = %q; it must be a trusted directory, not the snapshot", got.PolicyRepo)
	}
	if !policyEmpty {
		t.Error("the grading policy directory is not an empty, trusted directory")
	}
}

// Production grading cuts egress on a COPY of the config, never on the caller's.
func TestGradeSnapshotDoesNotMutateTheCallersConfig(t *testing.T) {
	a := gradeApp(t)
	dir := verifierDir(t, "verify.sh", "exit 0\n", 0o644)
	a.gradeSnapshot(context.Background(), gradeRequest{Image: "i", Workspace: t.TempDir(), Verifier: dir},
		func(box.RunSpec) (int, error) { return 0, nil })
	if a.cfg.Egress != "open" {
		t.Errorf("grading changed the caller's egress to %q", a.cfg.Egress)
	}
}

func TestVerifierCommandEntryPoints(t *testing.T) {
	// verify.sh runs under the trusted image's own shell, by ABSOLUTE path — an unqualified "sh"
	// would resolve through a PATH the candidate may have tried to influence.
	cmd, err := verifierCommand(verifierDir(t, "verify.sh", "exit 0\n", 0o644))
	if err != nil || cmd[0] != "/bin/sh" {
		t.Errorf("verify.sh = %v %v", cmd, err)
	}
	// An executable `verify` runs directly.
	cmd, err = verifierCommand(verifierDir(t, "verify", "#!/bin/sh\nexit 0\n", 0o755))
	if err != nil || len(cmd) != 1 || !strings.HasSuffix(cmd[0], "/verify") {
		t.Errorf("verify = %v %v", cmd, err)
	}
	// A non-executable `verify` is a clear authoring error, not a silent skip.
	if _, err := verifierCommand(verifierDir(t, "verify", "exit 0\n", 0o644)); err == nil ||
		!strings.Contains(err.Error(), "chmod") {
		t.Errorf("non-executable verify: %v", err)
	}
	// No entry point at all says what to add.
	if _, err := verifierCommand(t.TempDir()); err == nil || !strings.Contains(err.Error(), "verify.sh") {
		t.Errorf("empty verifier dir: %v", err)
	}
}

// A missing entry point is a harness error, and grading must not launch anything at all.
func TestGradeSnapshotWithNoVerifierEntryLaunchesNothing(t *testing.T) {
	a := gradeApp(t)
	launched := false
	got := a.gradeSnapshot(context.Background(), gradeRequest{Image: "i", Workspace: t.TempDir(), Verifier: t.TempDir()},
		func(box.RunSpec) (int, error) { launched = true; return 0, nil })
	if got.Status != eval.TrialError {
		t.Errorf("status = %q, want error", got.Status)
	}
	if launched {
		t.Error("a box was launched for a verifier with no entry point")
	}
}

func TestGradeDetailKeepsTheTailBounded(t *testing.T) {
	long := strings.Repeat("x", 5000) + "THE-REASON"
	d := gradeDetail(long, "")
	if len(d) > 2<<10+8 {
		t.Errorf("detail not bounded: %d bytes", len(d))
	}
	if !strings.Contains(d, "THE-REASON") {
		t.Error("the tail — where a verifier says why — was truncated away")
	}
	if d := gradeDetail("out", "err"); !strings.Contains(d, "out") || !strings.Contains(d, "err") {
		t.Errorf("both streams should be kept: %q", d)
	}
}
