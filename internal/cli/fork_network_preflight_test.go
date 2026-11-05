package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

const forkNetworkPreflightTaskID = "2026-11-05-network-preflight"

const forkNetworkPreflightProject = "box:\n  egress: filtered\n  egress_rules:\n    - to: {domain: fork.example}\n      protocol: tls\n      ports: [443]\n"

func seedForkNetworkPreflightTask(t *testing.T, repo string) string {
	t.Helper()
	taskDir := filepath.Join(repo, ".agent", "tasks", "00_todo", forkNetworkPreflightTaskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nid: " + forkNetworkPreflightTaskID + "\ntitle: Network preflight fixture\nlabels: []\nupdated: 2026-11-05T00:00:00Z\n---\n\n" +
		"# Network preflight fixture\n\n" +
		"**Context:** exercise fork network approval before canonical task ownership changes.\n\n" +
		"**Acceptance criteria:** a refused network policy leaves this task in todo.\n\n" +
		"**Approach:** use one valid task with a required checklist item.\n\n" +
		"## Subtasks\n\n- [ ] Verify the network preflight\n"
	if err := os.WriteFile(filepath.Join(taskDir, "task.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return forkNetworkPreflightTaskID
}

func writeForkNetworkPreflightProject(t *testing.T, repo string) {
	t.Helper()
	path := filepath.Join(repo, project.File)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(forkNetworkPreflightProject), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitForkNetworkPreflightFixture(t *testing.T, repo string) {
	t.Helper()
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "seed fork network preflight fixture")
}

func forkNetworkPreflightApp(t *testing.T, repo string) *app {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("COOP_REPO", repo)
	t.Setenv("COOP_EGRESS", "open")
	t.Setenv("COOP_BASE_IMAGE", "test-image")
	configDir := t.TempDir()
	t.Setenv("COOP_CONFIG_DIR", configDir)
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	rt := recordingRuntime(t, recorder)
	t.Setenv("COOP_RUNTIME", rt.Name)
	cfg := &config.Config{
		RepoOverride: repo, ConfigDir: configDir, BaseImage: "test-image", Egress: "open",
	}
	signInCred(t, cfg, "claude", "default")
	return &app{cfg: cfg, rt: rt, rtSet: true}
}

func assertForkNetworkTaskUnassigned(t *testing.T, repo, taskID string) {
	t.Helper()
	queue := filepath.Join(repo, ".agent", "tasks")
	todo := filepath.Join(queue, "00_todo", taskID)
	inProgress := filepath.Join(queue, "10_in_progress", taskID)
	if _, err := os.Stat(todo); err != nil {
		t.Errorf("canonical task is not still todo: %v", err)
	}
	if _, err := os.Stat(inProgress); err == nil {
		t.Errorf("canonical task moved to in_progress: %s", inProgress)
	} else if !os.IsNotExist(err) {
		t.Errorf("stat canonical in_progress task: %v", err)
	}
	record, owned, err := tasks.ReadTaskOwnerRecord(queue, taskID)
	if err != nil || owned {
		t.Errorf("canonical task owner record = (%+v, owned=%v, err=%v), want no assignment", record, owned, err)
	}
}

func TestDetachedForkLoopChecksNetworkApprovalBeforeStarting(t *testing.T) {
	repo := initRepo(t)
	taskID := seedForkNetworkPreflightTask(t, repo)
	writeForkNetworkPreflightProject(t, repo)
	commitForkNetworkPreflightFixture(t, repo)
	a := forkNetworkPreflightApp(t, repo)
	name := "networked"

	// The re-exec runs this test binary's CLI path. Keep a stop handle for only this fixture fork
	// until the child has cleared it, including when an assertion below fails.
	t.Setenv(detachedChildEnv, "1")
	t.Cleanup(func() {
		if !pathExists(forkspace.PidPath(repo, name)) {
			return
		}
		code, err := a.forkctl().ForkStop([]string{name})
		if code != 0 || err != nil {
			t.Errorf("cleanup fork stop = (%d, %v)", code, err)
		}
		if pathExists(forkspace.PidPath(repo, name)) {
			t.Errorf("cleanup left a worker pid record for fixture fork %s", name)
		}
	})

	var code int
	var runErr error
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			code, runErr = a.forkCreate([]string{name, "claude", "--loop", "-d"})
		})
	})
	output := stdout + stderr
	if strings.Contains(output, "Started fork") {
		// On the buggy path the parent returns after detach. Wait for that one fixture worker to
		// reach its approval refusal and clear its record before inspecting canonical ownership.
		wait.For(t, "detached fixture worker approval refusal", func() bool {
			data, err := os.ReadFile(forkspace.LogPath(repo, name))
			return err == nil && strings.Contains(string(data), "Review it: coop approve")
		})
		wait.For(t, "detached fixture worker pid record to clear", func() bool {
			return !pathExists(forkspace.PidPath(repo, name))
		})
	}
	if code != 1 || runErr == nil || !strings.Contains(runErr.Error(), "Review it: coop approve") {
		t.Errorf("detached fork without network approval = (%d, %v), want exit 1 and the approval refusal", code, runErr)
	}
	if strings.Contains(output, "Started fork") {
		t.Errorf("detached fork printed a successful start before approval:\n%s", output)
	}
	if pathExists(forkspace.PidPath(repo, name)) {
		t.Error("refused detached fork left a worker pid record")
	}
	assertForkNetworkTaskUnassigned(t, repo, taskID)
}

func TestRunForkLoopChecksNetworkApprovalBeforeAssigningCanonicalTask(t *testing.T) {
	for _, detached := range []bool{false, true} {
		name := "foreground"
		if detached {
			name = "detached"
		}
		t.Run(name, func(t *testing.T) {
			repo := initRepo(t)
			taskID := seedForkNetworkPreflightTask(t, repo)
			writeForkNetworkPreflightProject(t, repo)
			commitForkNetworkPreflightFixture(t, repo)
			a := forkNetworkPreflightApp(t, repo)
			forkName := "networked"
			ws, err := forkspace.Setup(repo, forkName)
			if err != nil {
				t.Fatal(err)
			}
			identity := bindTestFork(t, repo, forkName)

			code, err := a.runForkLoop(repo, ws, identity, "claude", "", "", "", "", nil, networkFlags{}, detached)
			if code != 1 || err == nil || !strings.Contains(err.Error(), "Review it: coop approve") {
				t.Errorf("runForkLoop(detached=%v) = (%d, %v), want the approval refusal", detached, code, err)
			}
			assertForkNetworkTaskUnassigned(t, repo, taskID)
		})
	}
}

func TestRunForkLoopChecksForkWorkspaceNetworkPolicyBeforeAssigning(t *testing.T) {
	repo := initRepo(t)
	taskID := seedForkNetworkPreflightTask(t, repo)
	commitForkNetworkPreflightFixture(t, repo)
	if pathExists(filepath.Join(repo, project.File)) {
		t.Fatal("canonical fixture unexpectedly has a project network policy")
	}
	a := forkNetworkPreflightApp(t, repo)
	forkName := "workspace-policy"
	ws, err := forkspace.Setup(repo, forkName)
	if err != nil {
		t.Fatal(err)
	}
	identity := bindTestFork(t, repo, forkName)
	writeForkNetworkPreflightProject(t, ws)

	code, err := a.runForkLoop(repo, ws, identity, "claude", "", "", "", "", nil, networkFlags{}, false)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "Review it: coop approve") {
		t.Errorf("runForkLoop with a workspace-only network request = (%d, %v), want the approval refusal", code, err)
	}
	assertForkNetworkTaskUnassigned(t, repo, taskID)
}
