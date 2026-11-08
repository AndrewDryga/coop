package box

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func writeToolVersions(t *testing.T, repo, pins string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, ".tool-versions"), []byte(pins), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A repo that pins tools and has no box Dockerfile runs in a toolchain image named by what it is
// built from: new pins or a new base are a new image, and the same pins share one.
func TestToolchainImageIsNamedByWhatItBuildsFrom(t *testing.T) {
	repo, twin := t.TempDir(), t.TempDir()
	writeToolVersions(t, repo, "golang 1.26.5\n")
	writeToolVersions(t, twin, "golang 1.26.5\n")
	img := ImageForRepo(repo, "coop-box:a", "")
	if !IsToolchainImage(img) || !strings.HasPrefix(img, "coop-toolchain:") {
		t.Fatalf("a repo with .tool-versions runs in %q, want a toolchain image", img)
	}
	if got := ImageForRepo(twin, "coop-box:a", ""); got != img {
		t.Errorf("the same pins in another repo = %q, want the shared %q", got, img)
	}
	if got := ImageForRepo(repo, "coop-box:b", ""); got == img {
		t.Error("a new base kept the toolchain image built on the old one")
	}
	writeToolVersions(t, repo, "golang 1.26.6\n")
	if got := ImageForRepo(repo, "coop-box:a", ""); got == img || !IsToolchainImage(got) {
		t.Errorf("changed pins = %q, want a new toolchain image", got)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ImageForRepo(repo, "coop-box:a", ""); IsToolchainImage(got) {
		t.Errorf("a repo with its own box Dockerfile runs in %q, want its project image", got)
	}
}

// toolchainRuntime is a runtime that has the base image but no toolchain image yet. Each build
// records its arguments and the files of the context it was given.
func toolchainRuntime(t *testing.T) (runtime.Runtime, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := `#!/bin/sh
case "$1 $2" in
"image inspect") case "$*" in *coop-toolchain*) [ -e "` + dir + `/built" ] || exit 1 ;; esac; exit 0 ;;
esac
if [ "$1" = build ]; then
	echo "build $*" >> "` + log + `"
	for last; do :; done
	ls -A "$last" >> "` + log + `"
	touch "` + dir + `/built"
fi
exit 0
`
	shim := filepath.Join(dir, "runtime")
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}, log
}

func TestToolchainBuildSeesOnlyTheGeneratedFiles(t *testing.T) {
	repo := t.TempDir()
	writeToolVersions(t, repo, "golang 1.26.5\n")
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt, log := toolchainRuntime(t)
	cfg := &config.Config{BaseImage: "coop-box:test", Egress: "open"}
	plan, err := PlanBuild(rt, cfg, repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Project || !plan.Toolchain || plan.Image != ImageForRepo(repo, cfg.BaseImage, "") || plan.BaseFirst {
		t.Fatalf("plan = %+v", plan)
	}
	if err := BuildPlanned(rt, cfg, repo, plan, false, "", strings.NewReader(""), io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "COOP_BASE_IMAGE=coop-box:test") || !strings.Contains(got, "-t "+plan.Image) {
		t.Errorf("build args = %s", got)
	}
	if files := strings.Fields(got[strings.Index(got, "\n")+1:]); strings.Join(files, " ") != ".tool-versions Dockerfile" {
		t.Errorf("the build context holds %v, want only .tool-versions and Dockerfile", files)
	}
}

// Only an open-network launch builds a missing toolchain image by itself; other modes are told to
// run coop build.
func TestOnlyOpenLaunchesBuildTheToolchainThemselves(t *testing.T) {
	repo := t.TempDir()
	writeToolVersions(t, repo, "golang 1.26.5\n")
	img := ImageForRepo(repo, "coop-box:test", "")
	for _, mode := range []string{"none", "filtered"} {
		rt, log := toolchainRuntime(t)
		cfg := &config.Config{BaseImage: "coop-box:test", Egress: mode}
		if err := EnsureToolchainImage(rt, cfg, repo, img, "", io.Discard); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(log); !os.IsNotExist(err) {
			t.Errorf("a %s launch built the toolchain image", mode)
		}
		if err := BuildWith(rt, cfg, repo, false, "", strings.NewReader(""), io.Discard); err == nil || !strings.Contains(err.Error(), "coop build --egress open") {
			t.Errorf("an automatic %s build = %v, want a refusal naming coop build", mode, err)
		}
	}
	rt, log := toolchainRuntime(t)
	var out bytes.Buffer
	if err := EnsureToolchainImage(rt, &config.Config{BaseImage: "coop-box:test", Egress: "open"}, repo, img, "", &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); err != nil {
		t.Error("an open launch did not build its missing toolchain image")
	}
}

// A worker job with no project runtime used to install its pins when its box started; an
// open-network job now runs in the toolchain image built from its frozen source instead.
func TestOpenJobsRunInTheirToolchainImage(t *testing.T) {
	repo := t.TempDir()
	writeToolVersions(t, repo, "golang 1.26.5\n")
	rt, log := toolchainRuntime(t)
	base := &config.Config{BaseImage: "coop-box:test", Egress: "none"}
	if img, err := controllerToolchainImage(base, rt, repo, io.Discard); err != nil || img != "coop-box:test" {
		t.Fatalf("an offline job runs in %q, %v; want the shared image", img, err)
	}
	open := &config.Config{BaseImage: "coop-box:test", Egress: "open"}
	img, err := controllerToolchainImage(open, rt, repo, io.Discard)
	if err != nil || img != ImageForRepo(repo, "coop-box:test", "") {
		t.Fatalf("an open job runs in %q, %v; want its toolchain image", img, err)
	}
	if _, err := os.Stat(log); err != nil {
		t.Error("the open job's toolchain image was not built")
	}
}

// A toolchain image must hold for every process an agent starts: the pins become the home default,
// so a process outside the checkout finds its versions, and the shims are on the login PATH too.
func TestToolchainImagePutsThePinsAndShimsOnEveryPath(t *testing.T) {
	repo := t.TempDir()
	writeToolVersions(t, repo, "elixir 1.19.0\n")
	dockerfile, _, ok, err := toolchainDefinition(repo)
	if err != nil || !ok {
		t.Fatalf("toolchain definition = %v, %v", ok, err)
	}
	for _, want := range []string{
		"COPY --chown=node:node .tool-versions /home/node/.tool-versions",
		`ENV PATH="/opt/coop/bin:/home/node/.asdf/shims:${PATH}"`,
		`> /etc/profile.d/zz-asdf.sh`,
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("the toolchain Dockerfile lacks %q:\n%s", want, dockerfile)
		}
	}
}
