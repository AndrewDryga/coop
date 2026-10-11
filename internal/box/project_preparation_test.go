package box

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
)

func projectPreparationFixture(t *testing.T) (*filteredExecution, *filteredDaemonFixture, string, func(string, string)) {
	t.Helper()
	f, d := filteredFixture(t)
	derivedImageFixture(t, d)
	definition, _, _, err := lockedImageDefinition(agents.ClientPlatform{OS: "linux", Architecture: "arm64", Libc: "glibc"})
	if err != nil {
		t.Fatal(err)
	}
	d.images[definition.Tag] = d.images[fixtureLockedImage]
	repo, write := gitProject(t, reusableDockerfile)
	projectBuildEndpoint(t)
	return f, d, repo, write
}

func projectBuildEndpoint(t *testing.T) {
	t.Helper()
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", dockersock.Serve(t, func() (dockersock.Info, error) {
		return dockersock.Info{ID: "fixture-daemon", ServerVersion: "29.4.0", KernelVersion: "fixture", OSType: "linux", Architecture: "aarch64"}, nil
	}))
}

func frozenProjectRuntime(t *testing.T, script string) runtime.Runtime {
	t.Helper()
	rt, err := (runtime.Runtime{Name: script}).FreezeCompose(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestFilteredProjectReadinessNamesMissingAndChangedInputs(t *testing.T) {
	f, d, repo, write := projectPreparationFixture(t)
	candidate := fixtureCandidate()
	definition, _, closure, err := lockedImageDefinition(agents.ClientPlatform{OS: "linux", Architecture: "arm64", Libc: "glibc"})
	if err != nil {
		t.Fatal(err)
	}
	spec := RunSpec{Repo: repo}
	check := func(want string) {
		t.Helper()
		err := checkFilteredProjectReadiness(t.Context(), d, f.store, spec, candidate.Runtime)
		if want == "" {
			if err != nil {
				t.Fatal(err)
			}
			return
		}
		var required *ProjectBuildRequired
		if !errors.As(err, &required) || !strings.Contains(required.Reason, want) {
			t.Fatalf("readiness = %v, want %q", err, want)
		}
	}
	check("no explicit project build")
	tag := filteredProjectTag(repo, candidate.ClientImage)
	inputs := projectBuildInputs(projectContextDigest(t, repo), candidate, closure, definition.Tag, tag, ".agent/Dockerfile")
	if err := f.store.ApproveProjectBuild(tag, inputs, fixtureBuiltImage); err != nil {
		t.Fatal(err)
	}
	check("")
	write("main.go", "package main // changed\n")
	check("inputs or settings changed")
	write("main.go", "package main\n")
	built := d.images[fixtureBuiltImage]
	delete(d.images, fixtureBuiltImage)
	check("image is missing")
	d.images[fixtureBuiltImage] = built
	delete(d.images, definition.Tag)
	check("client image has not been prepared")
	for name, excluded := range map[string]RunSpec{
		"login": {Repo: repo, Login: true}, "controller": {Repo: repo, ControllerJob: true},
		"readonly": {Repo: repo, Mode: agents.ModeReadOnly}, "bare": {Repo: repo, Mode: agents.ModeBare},
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkFilteredProjectReadiness(t.Context(), d, f.store, excluded, candidate.Runtime); err != nil {
				t.Fatalf("excluded launch consulted project instructions: %v", err)
			}
		})
	}
}

func TestFilteredAdmissionRefusesProjectBuildBeforeQualification(t *testing.T) {
	cfg, _, root := admissionFixture(t)
	repo, _ := gitProject(t, reusableDockerfile)
	canonical, err := canonicalProjectDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", dockersock.Serve(t, func() (dockersock.Info, error) {
		return dockersock.Info{ID: "fixture-daemon", ServerVersion: "29.4.0", KernelVersion: "fixture", OSType: "linux", Architecture: "aarch64"}, nil
	}))
	script := filepath.Join(t.TempDir(), "docker")
	body := "#!/bin/sh\nwhile [ \"$1\" = --host ] || [ \"$1\" = --config ]; do shift 2; done\n" +
		"case \"$1\" in\ninfo) printf '%s\\n' '{\"ID\":\"fixture-daemon\",\"ServerVersion\":\"29.4.0\",\"KernelVersion\":\"fixture\",\"OSType\":\"linux\",\"Architecture\":\"aarch64\",\"SecurityOptions\":[]}' ;;\nimage) exit 1 ;;\n*) exit 91 ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	mode := egress.Filtered
	qualified := false
	_, err = admitFilteredNetwork(cfg, runtime.Runtime{Name: script}, RunSpec{Repo: canonical}, store, canonical,
		networkstate.Admission{InvocationMode: &mode}, func(context.Context) error { qualified = true; return errors.New("must not qualify") })
	var required *ProjectBuildRequired
	if !errors.As(err, &required) || qualified {
		t.Fatalf("missing project build reached network setup: %v / qualified=%v", err, qualified)
	}
}

func TestReviewedProjectBuildExecutesOnlyItsPrivateSnapshot(t *testing.T) {
	f, d, repo, write := projectPreparationFixture(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	write(".env", "SECRET=never staged\n")
	prepared, err := PrepareFilteredProjectBuild(t.Context(), &config.Config{BoxHome: t.TempDir(), ConfigDir: t.TempDir()}, RunSpec{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	root, err := NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(prepared.cleanupDir) != root || filepath.Dir(prepared.dir) != prepared.cleanupDir {
		t.Fatalf("snapshot escaped protected host state: %q", prepared.dir)
	}
	if _, err := os.Stat(filepath.Join(prepared.dir, ".env")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("snapshot included a secret: %v", err)
	}
	write("main.go", "package main // edited while waiting\n")
	write(".agent/Dockerfile", "FROM unreviewed\n")
	script := filepath.Join(t.TempDir(), "docker")
	body := "#!/bin/sh\nfor arg do staged=$arg; done\n" +
		"[ \"$(cat \"$staged/main.go\")\" = 'package main' ] || exit 92\n" +
		"[ \"$(head -n 1 \"$staged/.agent/Dockerfile\")\" = 'ARG COOP_BASE_IMAGE' ] || exit 93\n" +
		"while [ $# -gt 0 ]; do if [ \"$1\" = --iidfile ]; then printf '%s\\n' '" + fixtureBuiltImage + "' > \"$2\"; fi; shift; done\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := fixtureCandidate()
	if _, _, err := buildPreparedFilteredProject(t.Context(), frozenProjectRuntime(t, script), &config.Config{}, d, f.store, prepared, candidate, io.Discard); err != nil {
		t.Fatalf("reviewed snapshot was not the build source: %v", err)
	}
	_, _, err = filteredProjectImage(t.Context(), runtime.Runtime{}, &config.Config{}, d, f.store, RunSpec{Repo: repo, Quiet: true}, candidate)
	if err == nil || !strings.Contains(err.Error(), "coop build --egress filtered") {
		t.Fatalf("later unreviewed edits acquired launch authority: %v", err)
	}
	write("main.go", "package main\n")
	write(".agent/Dockerfile", reusableDockerfile)
	image, _, err := filteredProjectImage(t.Context(), runtime.Runtime{}, &config.Config{}, d, f.store, RunSpec{Repo: repo, Quiet: true}, candidate)
	if err != nil || image != fixtureBuiltImage {
		t.Fatalf("actual reviewed inputs cannot reuse approved output: %q / %v", image, err)
	}
	dir := prepared.dir
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("review snapshot remained after closing: %v", err)
	}
	if err := prepared.Build(t.Context(), runtime.Runtime{}, &config.Config{}, nil, nil); err == nil {
		t.Fatal("closed snapshot could be built again")
	}
}

func TestReviewedProjectBuildRejectsChangedSettingsBeforeExecuting(t *testing.T) {
	_, _, repo, _ := projectPreparationFixture(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	prepared, err := PrepareFilteredProjectBuild(t.Context(), &config.Config{BoxHome: t.TempDir(), ConfigDir: t.TempDir()}, RunSpec{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	t.Setenv("SOURCE_DATE_EPOCH", "changed after review")
	if err := prepared.Build(t.Context(), runtime.Runtime{Name: "must-not-execute"}, &config.Config{}, nil, nil); err == nil || !strings.Contains(err.Error(), "settings changed") {
		t.Fatalf("changed settings reached build: %v", err)
	}
	if err := prepared.Build(t.Context(), runtime.Runtime{}, &config.Config{}, nil, nil); err == nil || !strings.Contains(err.Error(), "no longer available") {
		t.Fatalf("a consumed decision could be replayed: %v", err)
	}
}

type readinessObservationFailure struct{ filteredDocker }

func (readinessObservationFailure) Image(context.Context, string) (string, map[string]string, error) {
	return "", nil, errors.New("Docker daemon identity changed; custody remains pending")
}

func TestFilteredProjectReadinessPreservesObservationFailures(t *testing.T) {
	f, d, repo, _ := projectPreparationFixture(t)
	err := checkFilteredProjectReadiness(t.Context(), readinessObservationFailure{d}, f.store, RunSpec{Repo: repo}, fixtureCandidate().Runtime)
	var required *ProjectBuildRequired
	if err == nil || errors.As(err, &required) || !strings.Contains(err.Error(), "custody remains pending") {
		t.Fatalf("custody failure became build permission: %v", err)
	}
}

func TestReviewedProjectBuildSnapshotIsInspectableWhenCheckoutChanges(t *testing.T) {
	_, _, repo, write := projectPreparationFixture(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := reusableDockerfile + "RUN echo snapshot-instructions\n"
	write(".agent/Dockerfile", old)
	prepared, err := PrepareFilteredProjectBuild(t.Context(), &config.Config{BoxHome: t.TempDir(), ConfigDir: t.TempDir()}, RunSpec{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	write(".agent/Dockerfile", reusableDockerfile)
	data, err := os.ReadFile(filepath.Join(prepared.Directory(), prepared.Dockerfile()))
	if err != nil || string(data) != old {
		t.Fatalf("review target substituted the live checkout: %q / %v", data, err)
	}
}

func TestReviewedProjectBuildRejectsExposedSnapshotAuthority(t *testing.T) {
	_, _, repo, _ := projectPreparationFixture(t)
	t.Setenv("XDG_STATE_HOME", repo)
	if prepared, err := PrepareFilteredProjectBuild(t.Context(), &config.Config{BoxHome: t.TempDir(), ConfigDir: t.TempDir()}, RunSpec{Repo: repo}); err == nil {
		prepared.Close()
		t.Fatal("snapshot authority was placed inside the mounted checkout")
	}
}

func TestReviewedProjectBuildRejectsEditedSavedFiles(t *testing.T) {
	_, _, repo, _ := projectPreparationFixture(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	prepared, err := PrepareFilteredProjectBuild(t.Context(), &config.Config{BoxHome: t.TempDir(), ConfigDir: t.TempDir()}, RunSpec{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if err := prepared.verifySnapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(prepared.Directory(), ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := prepared.verifySnapshot(t.Context()); err == nil {
		t.Fatal("saved directory permissions retained old approval evidence")
	}
	if err := os.Chmod(filepath.Join(prepared.Directory(), ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prepared.Directory(), ".git-added-after-review"), []byte("not reviewed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepared.verifySnapshot(t.Context()); err == nil {
		t.Fatal("new saved file was ignored by snapshot proof")
	}
	if err := os.Remove(filepath.Join(prepared.Directory(), ".git-added-after-review")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prepared.Directory(), "main.go"), []byte("edited saved bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := prepared.verifySnapshot(t.Context()); err == nil {
		t.Fatal("edited saved bytes retained the old approval digest")
	}
}
