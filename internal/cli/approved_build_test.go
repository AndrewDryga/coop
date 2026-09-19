package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// The approved `coop build` transcripts: which image is being built and from what, the runtime's
// own build output passing through, and the one result line — plus the effect on work that is
// already running, which is reported only from counts the runtime actually returned.

// buildShim answers like a runtime with nothing running: no boxes to list, no boxes to remove,
// and a build that emits its own output before exiting with buildExit.
type buildShim struct {
	buildExit int
	daemonUp  bool
}

func (s buildShim) build(t *testing.T) runtime.Runtime {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	var script strings.Builder
	script.WriteString("#!/bin/sh\ncase \"$1\" in\n")
	if s.daemonUp {
		script.WriteString("  info) [ \"$2\" = --format ] && echo linux/aarch64; exit 0 ;;\n")
	} else {
		script.WriteString("  info) exit 1 ;;\n")
	}
	script.WriteString("  build) echo '[Build output]'; exit " + strconv.Itoa(s.buildExit) + " ;;\n")
	script.WriteString("  image) exit 1 ;;\n") // no image exists yet
	script.WriteString("esac\nexit 0\n")
	if err := os.WriteFile(path, []byte(script.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: path}
}

func TestApprovedBuild(t *testing.T) {
	// A repo with no box Dockerfile builds the shared image: no path worth printing, only its tag,
	// which names the box definition so a reader can tell which Coop's base it is.
	t.Run("31a-build-shared-base", func(t *testing.T) {
		cfg := &config.Config{RepoOverride: t.TempDir(), BoxHome: t.TempDir(), BaseImage: "coop-box:5db4c3f8c69555471772326ae9c3e960"}
		a := &app{cfg: cfg, rt: buildShim{daemonUp: true}.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdBuild(nil) })
		if code != 0 {
			t.Fatalf("cmdBuild = %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "31a-build-shared-base", out)
	})

	// A project box Dockerfile is a file the reader can have changed, so both it and the tag it
	// produces are named.
	t.Run("31b-build-project", func(t *testing.T) {
		cfg := projectBoxConfig(t, true)
		a := &app{cfg: cfg, rt: buildShim{daemonUp: true}.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdBuild(nil) })
		if code != 0 {
			t.Fatalf("cmdBuild = %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "31b-build-project", normalizeImage(cfg, out))
	})

	// An untracked box definition is the agent-authored case. The build still runs — it is an
	// explicit human action — but the notice comes first, and it is about USING the result.
	t.Run("31d-build-untracked-dockerfile", func(t *testing.T) {
		cfg := projectBoxConfig(t, false)
		a := &app{cfg: cfg, rt: buildShim{daemonUp: true}.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdBuild(nil) })
		if code != 0 {
			t.Fatalf("cmdBuild = %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "31d-build-untracked-dockerfile", normalizeImage(cfg, out))
	})

	// A build that ran and failed keeps the runtime's output and says what to fix.
	t.Run("31f-build-failed", func(t *testing.T) {
		cfg := projectBoxConfig(t, true)
		a := &app{cfg: cfg, rt: buildShim{daemonUp: true, buildExit: 1}.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdBuild(nil) })
		if code != 1 {
			t.Errorf("a failed build exited %d, want 1", code)
		}
		assertApprovedOutput(t, "31f-build-failed", normalizeImage(cfg, out))
	})

	// No runtime means nothing was built and nothing was said about an image.
	t.Run("31g-build-runtime-unavailable", func(t *testing.T) {
		cfg := &config.Config{RepoOverride: t.TempDir(), BoxHome: t.TempDir(), BaseImage: "coop-box"}
		a := &app{cfg: cfg, rt: buildShim{}.build(t), rtSet: true}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdBuild(nil) })
		if code != 1 {
			t.Errorf("a build with no runtime exited %d, want 1", code)
		}
		assertApprovedOutput(t, "31g-build-runtime-unavailable", out)
	})
}

// projectBoxConfig writes a repo with its own box Dockerfile. tracked decides whether git knows
// about it — the one thing that separates a reviewed box definition from an agent-authored one.
func projectBoxConfig(t *testing.T, tracked bool) *config.Config {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, git := gitrepo.New(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte("FROM debian\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if tracked {
		git("add", ".agent/Dockerfile")
		git("commit", "-qm", "box")
	}
	return &config.Config{RepoOverride: repo, BoxHome: t.TempDir(), BaseImage: "coop-box"}
}
