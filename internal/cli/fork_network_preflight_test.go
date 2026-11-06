package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/loop"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
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

// reviewIn is the review line a launch refused for folder's network request ends with, when coop
// runs in another project: coop approve reviews the project it runs in, so it names folder.
func reviewIn(t *testing.T, folder string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(folder)
	if err != nil {
		t.Fatal(err)
	}
	return "Review it: COOP_REPO=" + canonical + " coop approve"
}

// forkReview is reviewIn for a fork's checkout, which holds the fork's own approvals.
func forkReview(t *testing.T, repo, name string) string {
	t.Helper()
	return reviewIn(t, forkspace.Workspace(repo, name))
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

// A detached start notes the command that continues its loop as the person started it, each --tasks
// queue in order and the network flags included, for coop fork stop to repeat.
func TestDetachedForkLoopNotesHowToContinueIt(t *testing.T) {
	repo := initRepo(t)
	root, web := filepath.Join(repo, ".agent", "tasks"), filepath.Join(repo, "web", ".agent", "tasks")
	for _, queue := range []string{root, web} {
		if err := os.MkdirAll(filepath.Join(queue, tasks.StateTodo), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	a := forkNetworkPreflightApp(t, repo)
	name := "noted"
	t.Setenv(detachedChildEnv, "1") // its worker finds no task, so it ends by itself
	t.Cleanup(func() {
		if pathExists(forkspace.PidPath(repo, name)) {
			if code, err := a.forkctl().ForkStop([]string{name}); code != 0 || err != nil {
				t.Errorf("cleanup fork stop = (%d, %v)", code, err)
			}
		}
	})
	var code int
	var runErr error
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			code, runErr = a.forkCreate([]string{name, "claude", "--loop", "-d", "--tasks", web, "--tasks", root, "--egress", "open"})
		})
	})
	if code != 0 || runErr != nil {
		t.Fatalf("detached start = (%d, %v)\n%s%s", code, runErr, stdout, stderr)
	}
	want := "coop fork " + name + " claude --loop --tasks " + shellWord(web) + " --tasks " + shellWord(root) + " --egress open"
	if got := forkspace.ReadWorkerContinue(repo, name); got != want {
		t.Errorf("the detached start noted %q, want %q", got, want)
	}
	wait.For(t, "the drained worker to clear its record", func() bool { return !pathExists(forkspace.PidPath(repo, name)) })
}

// With no --tasks, a fork loop takes the queues COOP_TASKS names, as coop loop does: relative to the
// project, in order, skipping one that does not exist yet. coop fork stop's Continue line repeats them,
// so pasting it in a shell without COOP_TASKS still works those queues. --tasks replaces them.
func TestDetachedForkLoopTakesTheQueuesCOOPTASKSNames(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		flagRoot  bool // --tasks names the root queue as well
		wantQueue func(root, web, api string) []string
		wantErr   string
	}{
		{name: "COOP_TASKS alone", env: "api/.agent/tasks web/.agent/tasks missing/.agent/tasks",
			wantQueue: func(_, web, api string) []string { return []string{api, web} }},
		{name: "COOP_TASKS and --tasks", env: "api/.agent/tasks web/.agent/tasks", flagRoot: true,
			wantQueue: func(root, _, _ string) []string { return []string{root} }},
		{name: "COOP_TASKS outside the project", env: "../elsewhere", wantErr: `Invalid value "../elsewhere" in COOP_TASKS`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t)
			root := filepath.Join(repo, ".agent", "tasks")
			web, api := filepath.Join(repo, "web", ".agent", "tasks"), filepath.Join(repo, "api", ".agent", "tasks")
			for _, queue := range []string{root, web, api} {
				if err := os.MkdirAll(filepath.Join(queue, tasks.StateTodo), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			a := forkNetworkPreflightApp(t, repo)
			t.Setenv("COOP_TASKS", tc.env) // what the detached worker reads; this process's config is set below
			a.cfg.TasksFiles = strings.Fields(tc.env)
			name := "queued"
			t.Setenv(detachedChildEnv, "1") // its worker finds no task, so it ends by itself
			t.Cleanup(func() {
				if pathExists(forkspace.PidPath(repo, name)) {
					if code, err := a.forkctl().ForkStop([]string{name}); code != 0 || err != nil {
						t.Errorf("cleanup fork stop = (%d, %v)", code, err)
					}
				}
			})
			args := []string{name, "claude", "--loop", "-d", "--egress", "open"}
			if tc.flagRoot {
				args = append(args, "--tasks", root)
			}
			var code int
			var runErr error
			var stderr string
			stdout := captureStdout(t, func() {
				stderr = captureStderr(t, func() { code, runErr = a.forkCreate(args) })
			})
			if tc.wantErr != "" {
				if code != 2 || runErr == nil || !strings.Contains(runErr.Error(), tc.wantErr) {
					t.Errorf("start = (%d, %v), want exit 2 and %q", code, runErr, tc.wantErr)
				}
				if pathExists(forkspace.Workspace(repo, name)) {
					t.Error("a refused start created the fork")
				}
				return
			}
			if code != 0 || runErr != nil {
				t.Fatalf("detached start = (%d, %v)\n%s%s", code, runErr, stdout, stderr)
			}
			// A COOP_TASKS queue is joined to the project as Git names it, a --tasks one kept as typed.
			project, err := filepath.EvalSymlinks(repo)
			if err != nil {
				t.Fatal(err)
			}
			want := "coop fork " + name + " claude --loop"
			for _, queue := range tc.wantQueue(root, filepath.Join(project, "web", ".agent", "tasks"), filepath.Join(project, "api", ".agent", "tasks")) {
				want += " --tasks " + shellWord(queue)
			}
			want += " --egress open"
			if got := forkspace.ReadWorkerContinue(repo, name); got != want {
				t.Errorf("the detached start noted %q, want %q", got, want)
			}
			wait.For(t, "the drained worker to clear its record", func() bool { return !pathExists(forkspace.PidPath(repo, name)) })
		})
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
			return err == nil && strings.Contains(string(data), forkReview(t, repo, name))
		})
		wait.For(t, "detached fixture worker pid record to clear", func() bool {
			return !pathExists(forkspace.PidPath(repo, name))
		})
	}
	if code != 1 || runErr == nil || !strings.Contains(runErr.Error(), forkReview(t, repo, name)) {
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

			code, err := a.runForkLoop(repo, ws, identity, "claude", nil, "", "", "", nil, networkFlags{}, false, detached)
			if code != 1 || err == nil || !strings.Contains(err.Error(), reviewIn(t, ws)) {
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

	code, err := a.runForkLoop(repo, ws, identity, "claude", nil, "", "", "", nil, networkFlags{}, false, false)
	if code != 1 || err == nil || !strings.Contains(err.Error(), reviewIn(t, ws)) {
		t.Errorf("runForkLoop with a workspace-only network request = (%d, %v), want the approval refusal", code, err)
	}
	assertForkNetworkTaskUnassigned(t, repo, taskID)
}

// unapprovedDockerfile is build files no person approved: nothing on the fixture Docker built them.
const unapprovedDockerfile = "ARG COOP_BASE_IMAGE\nFROM ${COOP_BASE_IMAGE}\nRUN echo unapproved\n"

// forkBuildPreflightApp is a filtered project whose .agent/Dockerfile, when dockerfile is not empty,
// was never built, on a fixture Docker that answers info and has no images. Any other Docker command
// fails, so nothing builds.
func forkBuildPreflightApp(t *testing.T, dockerfile string) (*app, string, string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", dockersock.Serve(t, func() (dockersock.Info, error) {
		return dockersock.Info{ID: "fixture-daemon", ServerVersion: "29.4.0", KernelVersion: "fixture", OSType: "linux", Architecture: "aarch64"}, nil
	}))
	repo := initRepo(t)
	taskID := seedForkNetworkPreflightTask(t, repo)
	files := map[string]string{project.File: "box:\n  egress: filtered\n"}
	if dockerfile != "" {
		files[".agent/Dockerfile"] = dockerfile
	}
	for rel, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commitForkNetworkPreflightFixture(t, repo)
	docker := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n" +
		"while [ \"$1\" = --host ] || [ \"$1\" = --config ]; do shift 2; done\n" +
		"case \"$1\" in\n" +
		"info) printf '%s\\n' '{\"ID\":\"fixture-daemon\",\"ServerVersion\":\"29.4.0\",\"KernelVersion\":\"fixture\",\"OSType\":\"linux\",\"Architecture\":\"aarch64\",\"SecurityOptions\":[]}' ;;\n" +
		"image) exit 1 ;;\n" +
		"*) exit 91 ;;\n" +
		"esac\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// A detached start re-executes this test binary as coop: give it the same settings.
	configDir := t.TempDir()
	t.Setenv("COOP_REPO", repo)
	t.Setenv("COOP_CONFIG_DIR", configDir)
	t.Setenv("COOP_BASE_IMAGE", "test-image")
	t.Setenv("COOP_RUNTIME", docker)
	t.Setenv("COOP_EGRESS", "filtered")
	cfg := &config.Config{RepoOverride: repo, ConfigDir: configDir, BoxHome: t.TempDir(), BaseImage: "test-image"}
	signInCred(t, cfg, "claude", "default")
	return &app{cfg: cfg, rt: runtime.Runtime{Name: docker}, rtSet: true}, repo, taskID
}

// A detached start reports a build it may not run in the terminal, before a worker starts, with the
// command that builds the fork's checkout: coop build in the project would build the project.
func TestDetachedForkLoopChecksTheProjectBuildBeforeStarting(t *testing.T) {
	a, repo, taskID := forkBuildPreflightApp(t, unapprovedDockerfile)
	name := "unbuilt"
	t.Setenv(detachedChildEnv, "1")
	t.Cleanup(func() {
		if pathExists(forkspace.PidPath(repo, name)) {
			if code, err := a.forkctl().ForkStop([]string{name}); code != 0 || err != nil {
				t.Errorf("cleanup fork stop = (%d, %v)", code, err)
			}
		}
	})
	var code int
	var runErr error
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() { code, runErr = a.forkCreate([]string{name, "claude", "--loop", "-d"}) })
	})
	output := stdout + stderr
	if strings.Contains(output, "Started fork") {
		// On the buggy path the worker hits the refusal in the background; let it finish first.
		wait.For(t, "detached fixture worker build refusal", func() bool {
			data, err := os.ReadFile(forkspace.LogPath(repo, name))
			return err == nil && strings.Contains(string(data), "coop build --egress filtered")
		})
		wait.For(t, "detached fixture worker pid record to clear", func() bool {
			return !pathExists(forkspace.PidPath(repo, name))
		})
		t.Errorf("detached fork with unapproved build files started a worker:\n%s", output)
	}
	workspace, err := filepath.EvalSymlinks(forkspace.Workspace(repo, name))
	if err != nil {
		t.Fatal(err)
	}
	build := "run 'COOP_REPO=" + workspace + " coop build --egress filtered' on the host"
	var required *box.ProjectBuildRequired
	if code != 1 || !errors.As(runErr, &required) || required.Approved || !strings.Contains(runErr.Error(), build) {
		t.Errorf("detached fork with unapproved build files = (%d, %v), want exit 1 and the refusal to %q", code, runErr, build)
	}
	if pathExists(forkspace.PidPath(repo, name)) {
		t.Error("refused detached fork left a worker pid record")
	}
	assertForkNetworkTaskUnassigned(t, repo, taskID)
}

// A fork loop checks its build files before each task, as it checks its network approval: build files
// a task added are refused before the next task is assigned, with the fork's own build command, not
// by the next task's box once the fork holds that task.
func TestForkLoopChecksItsBuildFilesBeforeEachTask(t *testing.T) {
	a, repo, first := forkBuildPreflightApp(t, "")
	second := "2026-11-06-fork-build-second"
	seedForkDecisionTask(t, repo, second)
	commitForkNetworkPreflightFixture(t, repo)
	var ran []string
	park := parkingForkPass(t, &ran)
	a.forkLoopRun = func(spec loop.RunSpec) (int, error) {
		if err := os.WriteFile(filepath.Join(spec.Repo, ".agent", "Dockerfile"), []byte(unapprovedDockerfile), 0o644); err != nil {
			t.Error(err)
		}
		return park(spec)
	}
	ws, identity := setUpForkForDecision(t, repo, "rebuilt")
	code, output, err := runForkLoopForDecision(t, a, repo, ws, identity)
	workspace, wsErr := filepath.EvalSymlinks(ws)
	if wsErr != nil {
		t.Fatal(wsErr)
	}
	build := "run 'COOP_REPO=" + workspace + " coop build --egress filtered' on the host"
	var required *box.ProjectBuildRequired
	if !slices.Equal(ran, []string{first}) || code != 1 || !errors.As(err, &required) || !strings.Contains(err.Error(), build) {
		t.Fatalf("fork loop = (%d, %v) after working %v; want it to stop before the next task, refusing with %q\n%s", code, err, ran, build, output)
	}
	assertForkNetworkTaskUnassigned(t, repo, second)
}
