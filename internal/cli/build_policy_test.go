package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestFilteredBuildInterruptReturnsThroughCleanup(t *testing.T) {
	if os.Getenv("COOP_TEST_BUILD_INTERRUPT") == "child" {
		cfg := &config.Config{RepoOverride: os.Getenv("COOP_TEST_BUILD_REPO"), ConfigDir: t.TempDir(), BoxHome: t.TempDir()}
		a := &app{cfg: cfg, rt: runtime.Runtime{Name: os.Getenv("COOP_TEST_BUILD_RUNTIME")}, rtSet: true}
		code, err := a.cmdBuild([]string{"--egress", "filtered"})
		fmt.Printf("build returned: %d %v\n", code, err)
		return
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			root := t.TempDir()
			ready, stopped := filepath.Join(root, "ready"), filepath.Join(root, "stopped")
			rt := filepath.Join(root, "docker")
			body := "#!/bin/sh\ncase \"$1\" in\ninfo) exit 0 ;;\nimage) exit 1 ;;\ncontext)\n" +
				"trap 'touch " + strconv.Quote(stopped) + "; exit 143' TERM\n" +
				"echo $$ > " + strconv.Quote(ready) + "\nwhile :; do sleep 0.05; done ;;\nesac\n"
			if err := os.WriteFile(rt, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFilteredBuildInterruptReturnsThroughCleanup$")
			cmd.Env = append(os.Environ(), "COOP_TEST_BUILD_INTERRUPT=child", "COOP_TEST_BUILD_REPO="+root,
				"COOP_TEST_BUILD_RUNTIME="+rt, "XDG_STATE_HOME="+t.TempDir(),
				"TMPDIR="+root,
				"DOCKER_HOST=", "DOCKER_CONTEXT=", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			runtimePID := 0
			defer func() {
				if runtimePID != 0 {
					_ = syscall.Kill(-runtimePID, syscall.SIGKILL)
				}
				_ = cmd.Process.Kill()
			}()
			wait.For(t, "the build's runtime child", func() bool {
				select {
				case err := <-done:
					t.Fatalf("build exited before runtime readiness: %v\n%s", err, &output)
				default:
				}
				data, err := os.ReadFile(ready)
				if err == nil {
					runtimePID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
				}
				return runtimePID > 0
			})
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil || !strings.Contains(output.String(), "build returned: 1") {
				t.Fatalf("interrupt bypassed build cleanup: %v\n%s", err, &output)
			}
			if err := syscall.Kill(runtimePID, 0); err == nil {
				t.Fatal("the runtime child survived cancellation")
			}
		})
	}
}

func TestFilteredBuildNamesUntrackedCustomDockerfile(t *testing.T) {
	cfg := projectBoxConfig(t, false)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.Rename(filepath.Join(cfg.RepoOverride, ".agent", "Dockerfile"), filepath.Join(cfg.RepoOverride, ".agent", "Customfile")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.RepoOverride, ".agent", "project.yaml"), []byte("box:\n  dockerfile: .agent/Customfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg, rt: buildShim{daemonUp: true}.build(t), rtSet: true}
	out := captureTerminal(t, func() { _, _ = a.cmdBuild([]string{"--egress", "filtered"}) })
	for _, want := range []string{"The box Dockerfile is not tracked in Git", ".agent/Customfile controls what runs in the box.", "Dockerfile: .agent/Customfile", "ordinary networking"} {
		if !strings.Contains(out, want) {
			t.Errorf("filtered build omitted %q:\n%s", want, out)
		}
	}
}

func TestBuildModeGrammar(t *testing.T) {
	mode, err := parseBuildMode(nil)
	if err != nil || mode != nil {
		t.Fatalf("default must resolve project posture, not force open: %v / %v", mode, err)
	}
	for _, want := range []egress.Mode{egress.Open, egress.Filtered, egress.None} {
		for _, args := range [][]string{{"--egress", string(want)}, {"--egress=" + string(want)}} {
			mode, err := parseBuildMode(args)
			if err != nil || mode == nil || *mode != want {
				t.Fatalf("%v: %v / %v", args, mode, err)
			}
		}
	}
	for _, args := range [][]string{
		{"--egress"}, {"--egress", "Filtered"}, {"--egress", "open", "--egress", "none"},
		{"--allow-domain", "example.com"}, {"--egress-rules", "policy.yaml"}, {"extra"}, {"--"},
	} {
		if _, err := parseBuildMode(args); err == nil {
			t.Errorf("accepted unsupported build arguments: %v", args)
		}
	}
}

func TestExplicitBuildFailureKeepsItsMode(t *testing.T) {
	for _, mode := range []string{"open", "none"} {
		t.Run(mode, func(t *testing.T) {
			cfg := projectBoxConfig(t, true)
			cfg.Egress = "filtered"
			a := &app{cfg: cfg, rt: buildShim{daemonUp: true, buildExit: 1}.build(t), rtSet: true}
			var code int
			var err error
			out := captureTerminal(t, func() { code, err = a.cmdBuild([]string{"--egress", mode}) })
			if code != 1 || err == nil || !strings.Contains(out, "[Build output]") || !strings.Contains(out, "run coop build --egress "+mode+" again") {
				t.Fatalf("explicit build did not preserve its recovery mode: (%d, %v)\n%s", code, err, out)
			}
		})
	}
}

func TestLaunchDefersOrdinaryImageUntilPostureIsKnown(t *testing.T) {
	for _, mode := range []string{"open", "none", "filtered"} {
		t.Run(mode, func(t *testing.T) {
			cfg := projectBoxConfig(t, true)
			cfg.Egress = mode
			a := &app{cfg: cfg, rt: buildShim{daemonUp: true}.build(t), rtSet: true}
			repo, image, err := a.resolveLaunchImage()
			if err != nil || repo != cfg.RepoOverride || image != box.ImageForRepo(repo, cfg.BaseImage, "") {
				t.Fatalf("image-name resolution required the unused ordinary tag: %q %q %v", repo, image, err)
			}
			if mode != "filtered" {
				if err := a.checkCoopBox(repo, image); err == nil || !strings.Contains(err.Error(), "not built") || !strings.Contains(err.Error(), "coop build --egress open") {
					t.Fatalf("%s allowed an implicit runtime pull: %v", mode, err)
				}
			}
		})
	}
}

// A controller job's repository is code, not box settings. A worker stages every job source in a
// folder named "repository", so the tag of its Dockerfile would be one image, never built, for
// every repository; the job runs the worker's base (or the operator's image) instead, and a
// project file this Coop cannot read — one written for a newer Coop — cannot refuse it.
func TestControllerJobLaunchRunsTheWorkersBase(t *testing.T) {
	cfg := projectBoxConfig(t, true)
	a := &app{cfg: cfg, rt: buildShim{daemonUp: true}.build(t), rtSet: true}
	if err := os.WriteFile(filepath.Join(cfg.RepoOverride, ".agent", "project.yaml"), []byte("from_a_newer_coop: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.resolveLaunchImage(); err == nil {
		t.Fatal("a local launch accepted a project file it cannot read")
	}
	t.Setenv(box.ControllerJobEnv, strings.Repeat("a", 64))
	if repo, image, err := a.resolveLaunchImage(); err != nil || repo != cfg.RepoOverride || image != cfg.BaseImage {
		t.Fatalf("job launch = %q %q %v, want the worker's base %q", repo, image, err, cfg.BaseImage)
	}
	cfg.ImageOverride = "operator:1"
	if _, image, err := a.resolveLaunchImage(); err != nil || image != "operator:1" {
		t.Fatalf("job launch = %q %v, want the operator's image", image, err)
	}
	t.Setenv(box.ControllerJobEnv, "forged")
	if _, _, err := a.resolveLaunchImage(); err == nil || !strings.Contains(err.Error(), "controller job identity is invalid") {
		t.Fatalf("a malformed controller job marker = %v, want its own refusal", err)
	}
}

// The daemon's review gate resolves the job image too. The candidate has no Git history, so the
// gate stops just after choosing its image: a startup error about the review base proves it got
// past the job repository's never-built project tag and malformed project settings without starting a box.
func TestSessionReviewGateUsesTheJobImage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("COOP_GATE", "true")
	t.Setenv("COOP_BASE_IMAGE", "worker-box")
	repo := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte("FROM debian\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("box: [unrecognized project settings]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(t.TempDir(), "docker") // only the worker's base exists; the daemon answers nothing else
	if err := os.WriteFile(shim, []byte("#!/bin/sh\ncase \"$1$2$3\" in imageinspectworker-box) exit 0 ;; esac\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var result sessionsvc.ReviewGateResult
	captureTerminal(t, func() {
		result, err = defaultSessionReviewGate(cfg, runtime.Runtime{Name: shim}).Run(context.Background(), sessionsvc.ReviewGateRequest{
			Repository: repo, Candidate: t.TempDir(), NetworkMode: "none", Command: []string{"true"},
			Resources: workerproto.JobResources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 256},
		})
	})
	if err != nil || !result.Configured || !strings.Contains(result.StartupError, "review base") {
		t.Fatalf("job review gate = %+v, %v; want it past image selection", result, err)
	}
}

// A daemon review may need to rebuild Coop's base before it can start the gate. Both build
// streams belong to this review's output, not the worker's own terminal log.
func TestSessionReviewGateKeepsBaseRepairOutput(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("COOP_GATE", "true")
	shim := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"image) exit 1 ;;\n" +
		"info) echo linux/x86_64; exit 0 ;;\n" +
		"build) echo 'base build stdout'; echo 'base build stderr' >&2; exit 23 ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.BoxHome = t.TempDir()
	cfg.BaseImage = "coop-box:" + strings.Repeat("a", 32)
	box.StampImageMeta(cfg, box.ManagedBaseRepository, "v-old")
	var output bytes.Buffer
	result, err := defaultSessionReviewGate(cfg, runtime.Runtime{Name: shim}).Run(context.Background(), sessionsvc.ReviewGateRequest{
		Repository: t.TempDir(), Candidate: t.TempDir(), NetworkMode: "none", Output: &output,
		Command: []string{"true"}, Resources: workerproto.JobResources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 256},
	})
	if err != nil || !result.Configured || !strings.Contains(result.StartupError, "build exited with status 23") {
		t.Fatalf("review base repair = %+v, %v", result, err)
	}
	for _, want := range []string{"base build stdout", "base build stderr"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("review output lacks %q:\n%s", want, &output)
		}
	}
}

func TestRestrictedACPDoesNotAutomaticallyBuildProject(t *testing.T) {
	for _, mode := range []string{"filtered", "none"} {
		t.Run(mode, func(t *testing.T) {
			cfg := projectBoxConfig(t, true)
			cfg.Egress = mode
			a := &app{cfg: cfg, rt: buildShim{daemonUp: true}.build(t), rtSet: true}
			var err error
			out := captureTerminal(t, func() { err = a.ensureACPImage() })
			if err == nil || strings.Contains(out, "[Build output]") || strings.Contains(out, "Building it now") {
				t.Fatalf("restricted ACP built the project: %v\n%s", err, out)
			}
		})
	}
}
