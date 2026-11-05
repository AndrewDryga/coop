package forkctl

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/testutil/starttoken"
)

type forkLoopJSONStatus struct {
	State     string `json:"state"`
	LoopError string `json:"loop_error"`
}

func forkLoopFixture(t *testing.T) (repo, workspace string, identity forkspace.Identity, control *Control) {
	t.Helper()
	repo = filepath.Join(t.TempDir(), "repo")
	workspace = forkspace.Workspace(repo, "perf")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	identity, err = forkspace.EnsureGenerationLocked(repo, "perf")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	control = &Control{cfg: &config.Config{RepoOverride: repo}}
	return repo, workspace, identity, control
}

func readForkLoopListing(t *testing.T, control *Control) (text, stderr string, status forkLoopJSONStatus) {
	t.Helper()
	text, stderr, raw := listForkStatus(t, control, "perf")
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		t.Fatalf("decode fork status %q: %v", raw, err)
	}
	return text, stderr, status
}

// These are loopExitCode's queue outcomes, not a failed provider or admission.
func TestForkLoopExitDoesNotCallQueueOutcomesFailures(t *testing.T) {
	for _, code := range []int{1, 3} {
		repo, _, identity, control := forkLoopFixture(t)
		NoteForkLoopExit(repo, identity, code, nil)
		text, stderr, status := readForkLoopListing(t, control)
		if stderr != "" || status.State == "failed" || status.LoopError != "" || strings.Contains(text, "Loop failed:") {
			t.Fatalf("queue outcome %d reported a failure: status %+v, stderr %q, listing:\n%s", code, status, stderr, text)
		}
	}
}

func TestForkLoopExitInterruptedReviewIsNotFailure(t *testing.T) {
	repo, _, identity, control := forkLoopFixture(t)
	NoteForkLoopExit(repo, identity, 130, errors.New("review interrupted"))
	text, stderr, status := readForkLoopListing(t, control)
	if stderr != "" || status.State == "failed" || status.LoopError != "" || strings.Contains(text, "Loop failed:") {
		t.Fatalf("interrupted review reported a failure: status %+v, stderr %q, listing:\n%s", status, stderr, text)
	}
}

func TestForkLoopExitClearsForSuccessfulAndInterruptedCodes(t *testing.T) {
	for _, tc := range []struct {
		code int
		name string
	}{{0, "success"}, {130, "interrupt"}, {143, "terminate"}} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			repo, _, identity, control := forkLoopFixture(t)
			NoteForkLoopExit(repo, identity, 1, errors.New("previous failure"))
			NoteForkLoopExit(repo, identity, tc.code, nil)

			text, stderr, status := readForkLoopListing(t, control)
			if stderr != "" || status.State != "idle" || status.LoopError != "" || strings.Contains(text, "Loop failed:") {
				t.Fatalf("exit %d left a loop failure: status %+v, stderr %q, listing:\n%s", tc.code, status, stderr, text)
			}
		})
	}
}

func TestForkLoopExitBoundsReasonAndEscapesListing(t *testing.T) {
	repo, _, identity, control := forkLoopFixture(t)
	NoteForkLoopExit(repo, identity, 1, errors.New("\x1b[2J"+strings.Repeat("界", 400)))

	text, stderr, status := readForkLoopListing(t, control)
	if stderr != "" || status.State != "failed" {
		t.Fatalf("fork loop status = %+v, stderr %q", status, stderr)
	}
	if got := len([]rune(status.LoopError)); got != 300 {
		t.Fatalf("stored reason has %d runes, want 300: %q", got, status.LoopError)
	}
	if !strings.HasPrefix(status.LoopError, "\x1b[2J") || !strings.HasSuffix(status.LoopError, "…") {
		t.Fatalf("bounded reason lost its control prefix or ellipsis: %q", status.LoopError)
	}
	if !strings.Contains(text, `\u001B[2J`) || strings.ContainsRune(text, '\x1b') {
		t.Fatalf("human listing did not escape the control character:\n%s", text)
	}
}

func TestForkLoopExitLetsCurrentStatusOverrideFailure(t *testing.T) {
	cases := []struct {
		name           string
		state          string
		stderrContains string
		apply          func(t *testing.T, repo, workspace string, identity forkspace.Identity)
	}{
		{
			name:  "running",
			state: "running",
			apply: func(t *testing.T, repo, _ string, identity forkspace.Identity) {
				t.Helper()
				if err := forkspace.WritePid(repo, identity.Name, os.Getpid(), identity.Generation); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:           "cleanup",
			state:          "cleanup",
			stderrContains: "detached worker state is missing a supported owner header",
			apply: func(t *testing.T, repo, _ string, identity forkspace.Identity) {
				t.Helper()
				if err := os.WriteFile(forkspace.PidPath(repo, identity.Name), []byte("malformed\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:           "unknown sandbox liveness",
			state:          "unknown",
			stderrContains: "sandbox " + strings.Repeat("c", 32) + " was started on " + forkspace.ProcTokenSystem(starttoken.Foreign()),
			apply: func(t *testing.T, repo, workspace string, identity forkspace.Identity) {
				t.Helper()
				record := forkspace.ExecutionRecord{
					Version: 1, ID: strings.Repeat("c", 32), Kind: forkspace.ExecutionForkLoop,
					Role: forkspace.ExecutionRoleDetachedWorker, Workspace: workspace, Fork: &identity,
					SourceID: "fork-loop-perf-" + string(identity.Generation), PID: os.Getpid(),
					Token: starttoken.Foreign(), StartedAt: time.Now().UTC(),
				}
				body, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(forkspace.StateDir(repo), "executions")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, record.ID+".json"), append(body, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			repo, workspace, identity, control := forkLoopFixture(t)
			NoteForkLoopExit(repo, identity, 1, errors.New("previous failure"))
			tc.apply(t, repo, workspace, identity)

			text, stderr, status := readForkLoopListing(t, control)
			if status.State != tc.state || status.LoopError != "" || strings.Contains(text, "Loop failed:") ||
				(tc.stderrContains == "" && stderr != "") ||
				(tc.stderrContains != "" && !strings.Contains(stderr, tc.stderrContains)) {
				t.Fatalf("%s state did not override old failure: status %+v, stderr %q, listing:\n%s", tc.state, status, stderr, text)
			}
		})
	}
}

func TestForkLoopExitIgnoresRetiredGeneration(t *testing.T) {
	repo, _, oldIdentity, control := forkLoopFixture(t)
	NoteForkLoopExit(repo, oldIdentity, 1, errors.New("retired generation failure"))

	unlock, err := forkspace.LockState(repo, oldIdentity.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := forkspace.RemoveGenerationIfMatchesLocked(repo, oldIdentity); err != nil {
		unlock()
		t.Fatal(err)
	}
	currentIdentity, err := forkspace.EnsureGenerationLocked(repo, oldIdentity.Name)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if currentIdentity == oldIdentity {
		t.Fatal("replacement fork reused the retired generation")
	}
	text, stderr, status := readForkLoopListing(t, control)
	if stderr != "" || status.State != "idle" || status.LoopError != "" || strings.Contains(text, "Loop failed:") {
		t.Fatalf("replacement generation read retired error: status %+v, stderr %q, listing:\n%s", status, stderr, text)
	}

	NoteForkLoopExit(repo, currentIdentity, 1, errors.New("current generation failure"))
	NoteForkLoopExit(repo, oldIdentity, 1, errors.New("stale generation overwrite"))
	NoteForkLoopExit(repo, oldIdentity, 0, nil)
	text, stderr, status = readForkLoopListing(t, control)
	if stderr != "" || status.State != "failed" || status.LoopError != "current generation failure" ||
		!strings.Contains(text, "Loop failed: current generation failure") {
		t.Fatalf("retired generation changed current error: status %+v, stderr %q, listing:\n%s", status, stderr, text)
	}
}

func TestForkLoopExitDoesNotWaitForLifecycleLock(t *testing.T) {
	repo, _, identity, control := forkLoopFixture(t)
	NoteForkLoopExit(repo, identity, 1, errors.New("kept failure"))

	unlock, err := forkspace.LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	NoteForkLoopExit(repo, identity, 1, errors.New("must not overwrite while stop holds the lock"))
	unlock()

	text, stderr, status := readForkLoopListing(t, control)
	if stderr != "" || status.State != "failed" || status.LoopError != "kept failure" ||
		!strings.Contains(text, "Loop failed: kept failure") {
		t.Fatalf("held lock changed loop error: status %+v, stderr %q, listing:\n%s", status, stderr, text)
	}
}

func TestForkLoopRestartStoppedUnderLifecycleLockKeepsNoOldFailure(t *testing.T) {
	repo, workspace, identity, control := forkLoopFixture(t)
	NoteForkLoopExit(repo, identity, 1, errors.New("old approval refusal"))
	if err := ClearForkLoopError(workspace); err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	NoteForkLoopExit(repo, identity, 130, errors.New("review interrupted"))
	unlock()
	text, stderr, status := readForkLoopListing(t, control)
	if stderr != "" || status.State == "failed" || status.LoopError != "" || strings.Contains(text, "old approval refusal") {
		t.Fatalf("stopped restart kept an old failure: status %+v, stderr %q, listing:\n%s", status, stderr, text)
	}
}

func TestForkLoopFailureCannotHideReadyCandidate(t *testing.T) {
	if got := (forkStatus{Ready: true, LoopError: "agent-writable note"}).stateCell(); got != "ready" {
		t.Fatalf("ready candidate with an error hint = %q", got)
	}
}

func TestForkLoopStartClearsOldLoopError(t *testing.T) {
	repo, workspace, identity, control := forkLoopFixture(t)
	NoteForkLoopExit(repo, identity, 1, errors.New("old loop failure"))
	NoteForkStart(workspace, false, true, nil)

	text, stderr, status := readForkLoopListing(t, control)
	if stderr != "" || status.State != "idle" || status.LoopError != "" || strings.Contains(text, "Loop failed:") {
		t.Fatalf("successful interactive start left loop error: status %+v, stderr %q, listing:\n%s", status, stderr, text)
	}
}
