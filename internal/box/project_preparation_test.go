package box

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
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
	pinHome(t)
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

// A fork loop asks before each task, building nothing and setting nothing up, whether the box it would
// launch has its build files ready, and gets the refusal that launch would give, naming the fork's
// checkout. Files a person approved count as ready: the launch rebuilds those itself.
func TestCheckProjectBuildRefusesOnlyFilesNobodyApproved(t *testing.T) {
	cfg, here, root := admissionFixture(t)
	fork, _ := gitProject(t, reusableDockerfile)
	fork, err := canonicalProjectDir(fork)
	if err != nil {
		t.Fatal(err)
	}
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
	filtered, open := egress.Filtered, egress.Open
	check := func(repo string, mode *egress.Mode) error {
		return CheckProjectBuild(cfg, runtime.Runtime{Name: script}, RunSpec{Repo: repo}, NetworkAdmission{InvocationMode: mode})
	}
	refused := func(reason string) {
		t.Helper()
		err := check(fork, &filtered)
		var required *ProjectBuildRequired
		want := "run 'COOP_REPO=" + fork + " coop build --egress filtered' on the host"
		if !errors.As(err, &required) || required.Approved || !strings.Contains(required.Reason, reason) || !strings.Contains(err.Error(), want) {
			t.Fatalf("check of build files nobody approved = %v, want %q and the command %q", err, reason, want)
		}
	}
	if err := check(here, &filtered); err != nil {
		t.Fatalf("check of a project without build files = %v", err)
	}
	if err := check(fork, &open); err != nil {
		t.Fatalf("check of an open launch = %v", err)
	}
	refused("no explicit project build")
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("the check made network state", err)
	}
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	refused("client image has not been prepared")
	if err := store.RecordProjectBuildReview(fork, projectBuildReview(projectContextDigest(t, fork), "fixture-daemon", ".agent/Dockerfile"), nil); err != nil {
		t.Fatal(err)
	}
	if err := check(fork, &filtered); err != nil {
		t.Fatalf("check of approved build files = %v, want none: the launch rebuilds them", err)
	}
	if got := BuildCommand(fork, fork); got != "coop build --egress filtered" {
		t.Errorf("BuildCommand(the same checkout) = %q, want plain coop build", got)
	}
}

func TestReviewedProjectBuildExecutesOnlyItsPrivateSnapshot(t *testing.T) {
	pinHome(t)
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
	if _, err := buildPreparedFilteredProject(t.Context(), frozenProjectRuntime(t, script), &config.Config{}, d, f.store, prepared, candidate, io.Discard); err != nil {
		t.Fatalf("reviewed snapshot was not the build source: %v", err)
	}
	_, _, err = filteredProjectImage(t.Context(), runtime.Runtime{}, &config.Config{}, d, f.store, RunSpec{Repo: repo, Quiet: true}, candidate)
	if err == nil || !strings.Contains(err.Error(), "coop build --egress filtered") {
		t.Fatalf("later unreviewed edits acquired launch authority: %v", err)
	}
	write("main.go", "package main\n")
	write(".agent/Dockerfile", reusableDockerfile)
	var image string
	output := captureStderr(t, func() {
		image, _, err = filteredProjectImage(t.Context(), runtime.Runtime{}, &config.Config{}, d, f.store, RunSpec{Repo: repo}, candidate)
		newLaunchSections(RunSpec{Agent: "claude"}).secrets(53)
	})
	if !strings.Contains(output, "Unchanged since it was built\n\nProtecting secrets\n") {
		t.Fatalf("project summary runs into secret protection: %q", output)
	}
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
	pinHome(t)
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
	pinHome(t)
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
	pinHome(t)
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

// approvedRebuildFixture is a project with a working builder and a daemon that can move to a new
// client image, the way a Coop upgrade does. Each build reports the next image and is counted.
type approvedRebuildFixture struct {
	t         *testing.T
	f         *filteredExecution
	d         *filteredDaemonFixture
	repo      string
	write     func(string, string)
	rt        runtime.Runtime
	cfg       *config.Config
	candidate networkstate.CandidateSpec
	tag       string // the locked image's pinned tag
	produced  string
	counter   string
}

func newApprovedRebuildFixture(t *testing.T) *approvedRebuildFixture {
	t.Helper()
	pinHome(t)
	f, d, repo, write := projectPreparationFixture(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	definition, _, _, err := lockedImageDefinition(agents.ClientPlatform{OS: "linux", Architecture: "arm64", Libc: "glibc"})
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	x := &approvedRebuildFixture{t: t, f: f, d: d, repo: repo, write: write, candidate: fixtureCandidate(), tag: definition.Tag,
		cfg: &config.Config{BoxHome: t.TempDir(), ConfigDir: t.TempDir()}, produced: filepath.Join(scratch, "produced"), counter: filepath.Join(scratch, "builds")}
	x.candidate.Runtime.DaemonID = "fixture-daemon"
	x.nextImage(fixtureBuiltImage)
	script := filepath.Join(scratch, "docker")
	body := "#!/bin/sh\ncase \"$1 $2\" in \"image ls\") exit 0 ;; esac\nprintf 'build\\n' >> '" + x.counter + "'\n" +
		"while [ $# -gt 0 ]; do\n  if [ \"$1\" = --iidfile ]; then cat '" + x.produced + "' > \"$2\"; fi\n  shift\ndone\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	x.rt = frozenProjectRuntime(t, script)
	return x
}

func (x *approvedRebuildFixture) nextImage(image string) {
	x.t.Helper()
	if err := os.WriteFile(x.produced, []byte(image+"\n"), 0o600); err != nil {
		x.t.Fatal(err)
	}
}

func (x *approvedRebuildFixture) builds() int {
	data, _ := os.ReadFile(x.counter)
	return strings.Count(string(data), "build\n")
}

// explicitBuild is what `coop build` does: a person asked, so these files become the approved ones.
func (x *approvedRebuildFixture) explicitBuild() {
	x.t.Helper()
	if _, err := buildFilteredProject(x.t.Context(), x.rt, x.cfg, x.d, x.f.store, x.repo, x.candidate, io.Discard); err != nil {
		x.t.Fatal(err)
	}
}

// upgradeClient moves the daemon to a new locked client image, and gets ready for a build on it.
func (x *approvedRebuildFixture) upgradeClient(client, built string) {
	x.t.Helper()
	x.d.mu.Lock()
	defer x.d.mu.Unlock()
	locked := x.d.images[fixtureLockedImage]
	locked.id = client
	x.d.images[client], x.d.images[x.tag] = locked, locked
	image := x.d.images[fixtureBuiltImage]
	image.id = built
	x.d.images[built] = image
	x.candidate.ClientImage = client
	x.nextImage(built)
}

func (x *approvedRebuildFixture) readiness() error {
	x.t.Helper()
	return checkFilteredProjectReadiness(x.t.Context(), x.d, x.f.store, RunSpec{Repo: x.repo}, x.candidate.Runtime)
}

func (x *approvedRebuildFixture) miss() *ProjectBuildRequired {
	x.t.Helper()
	var required *ProjectBuildRequired
	if err := x.readiness(); !errors.As(err, &required) {
		x.t.Fatalf("readiness = %v, want a project build miss", err)
	}
	return required
}

// automaticBuild is what a launch does on its own once it holds the project's lock.
func (x *approvedRebuildFixture) automaticBuild() error {
	x.t.Helper()
	_, err := rebuildApprovedLocked(x.t.Context(), x.rt, x.cfg, x.d, x.f.store, x.repo, x.candidate, nil, io.Discard)
	return err
}

// The whole point: a Coop upgrade or a lost image runs nothing a person has not approved, so it needs
// no person. The files stay approved across as many client images as there are.
func TestApprovedProjectFilesRebuildByThemselvesWhenOnlyCoopChanged(t *testing.T) {
	x := newApprovedRebuildFixture(t)
	if x.miss().Approved {
		t.Fatal("a project with no explicit build counted as approved")
	}
	x.explicitBuild()
	if err := x.readiness(); err != nil {
		t.Fatal(err)
	}

	for i, step := range []struct{ client, built string }{
		{"sha256:" + strings.Repeat("4", 64), "sha256:" + strings.Repeat("5", 64)},
		{"sha256:" + strings.Repeat("6", 64), "sha256:" + strings.Repeat("7", 64)},
	} {
		x.upgradeClient(step.client, step.built)
		if required := x.miss(); !required.Approved || !strings.Contains(required.Reason, "no explicit project build") {
			t.Fatalf("client image %d: %+v", i, required)
		}
		before := x.builds()
		if err := x.automaticBuild(); err != nil {
			t.Fatalf("client image %d: %v", i, err)
		}
		if x.builds() != before+1 {
			t.Fatalf("client image %d: %d builds, want %d", i, x.builds(), before+1)
		}
		if err := x.readiness(); err != nil {
			t.Fatalf("client image %d: the rebuilt image is not ready: %v", i, err)
		}
	}

	// The image goes missing, with nothing else changed.
	x.d.mu.Lock()
	built := x.d.images["sha256:"+strings.Repeat("7", 64)]
	delete(x.d.images, built.id)
	x.d.mu.Unlock()
	if required := x.miss(); !required.Approved || !strings.Contains(required.Reason, "image is missing") {
		t.Fatalf("a lost image: %+v", required)
	}
	x.d.mu.Lock()
	x.d.images[built.id] = built
	x.d.mu.Unlock()

	// Coop's own image has not been prepared yet: the files can still be recognized without it.
	x.d.mu.Lock()
	delete(x.d.images, x.tag)
	x.d.mu.Unlock()
	if required := x.miss(); !required.Approved || !strings.Contains(required.Reason, "client image has not been prepared") {
		t.Fatalf("an unprepared client image: %+v", required)
	}
}

// A person confirming the terminal's Build and continue approves files as much as `coop build` does.
func TestConfirmedProjectBuildCountsAsApproved(t *testing.T) {
	x := newApprovedRebuildFixture(t)
	prepared, err := PrepareFilteredProjectBuild(t.Context(), x.cfg, RunSpec{Repo: x.repo})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if _, err := buildPreparedFilteredProject(t.Context(), x.rt, x.cfg, x.d, x.f.store, prepared, x.candidate, io.Discard); err != nil {
		t.Fatal(err)
	}
	x.upgradeClient("sha256:"+strings.Repeat("4", 64), "sha256:"+strings.Repeat("5", 64))
	if !x.miss().Approved {
		t.Fatal("confirmed files did not count as approved")
	}
}

// Anything but the exact files a person approved, on the daemon and settings they approved them for,
// waits. This is the sandbox boundary: an agent can edit all of these files, and a project build runs
// them with ordinary networking.
func TestOnlyTheApprovedFilesRebuildByThemselves(t *testing.T) {
	const alternate = ".agent/Alt.Dockerfile"
	for name, test := range map[string]struct {
		prepare func(*approvedRebuildFixture)
		change  func(*approvedRebuildFixture)
	}{
		"a copied file": {change: func(x *approvedRebuildFixture) { x.write("main.go", "package main // edited by an agent\n") }},
		"the Dockerfile": {change: func(x *approvedRebuildFixture) {
			x.write(".agent/Dockerfile", reusableDockerfile+"RUN curl https://example.com\n")
		}},
		"a new file in the context": {change: func(x *approvedRebuildFixture) { x.write("planted.sh", "curl https://example.com\n") }},
		"a new file beside the project's settings": {change: func(x *approvedRebuildFixture) {
			x.write(".agent/setup.sh", "curl https://example.com\n")
		}},
		"a file's mode": {change: func(x *approvedRebuildFixture) {
			if err := os.Chmod(filepath.Join(x.repo, "main.go"), 0o755); err != nil {
				x.t.Fatal(err)
			}
		}},
		"an ignore file that lets more in": {
			prepare: func(x *approvedRebuildFixture) {
				x.write("docs/planted.sh", "curl https://example.com\n")
				x.write(".dockerignore", "docs/\n")
			},
			change: func(x *approvedRebuildFixture) { x.write(".dockerignore", "other/\n") },
		},
		// Every byte in the context is unchanged, so only the Dockerfile path can catch this one.
		"which Dockerfile builds": {
			prepare: func(x *approvedRebuildFixture) {
				x.write(alternate, reusableDockerfile+"RUN curl https://example.com\n")
			},
			change: func(x *approvedRebuildFixture) {
				before := projectContextDigest(x.t, x.repo)
				x.write(".agent/project.yaml", "box:\n  dockerfile: "+alternate+"\n")
				if projectContextDigest(x.t, x.repo) != before {
					x.t.Fatal("the fixture changed the context, so it proves nothing about the Dockerfile path")
				}
			},
		},
		"a build setting":      {change: func(x *approvedRebuildFixture) { x.t.Setenv("SOURCE_DATE_EPOCH", "1") }},
		"the daemon":           {change: func(x *approvedRebuildFixture) { x.candidate.Runtime.DaemonID = "another-daemon" }},
		"the first build ever": {}, // no person has approved anything for this project yet
	} {
		t.Run(name, func(t *testing.T) {
			x := newApprovedRebuildFixture(t)
			if test.prepare != nil {
				test.prepare(x)
			}
			if test.change != nil {
				x.explicitBuild()
				test.change(x)
			}
			x.upgradeClient("sha256:"+strings.Repeat("4", 64), "sha256:"+strings.Repeat("5", 64))
			before := x.builds()
			if x.miss().Approved {
				t.Fatal("readiness offered to rebuild files nobody approved")
			}
			var required *ProjectBuildRequired
			if err := x.automaticBuild(); !errors.As(err, &required) {
				t.Fatalf("automatic build = %v, want a refusal", err)
			}
			if x.builds() != before {
				t.Fatalf("the builder ran for files nobody approved: %d builds, want %d", x.builds(), before)
			}
		})
	}
}

// A refusal names the files whose change voided the approval, copied or not, or says that only the
// build settings or Coop's own inputs changed. emisar's loop was refused three times naming nothing,
// while the files that changed were ones its Dockerfile never copies (2026-11-05).
func TestAProjectBuildRefusalNamesWhatChanged(t *testing.T) {
	newClient := func(x *approvedRebuildFixture) {
		x.upgradeClient("sha256:"+strings.Repeat("4", 64), "sha256:"+strings.Repeat("5", 64))
	}
	for name, test := range map[string]struct {
		prepare  func(*approvedRebuildFixture)
		change   func(*approvedRebuildFixture)
		approved bool
		want     string
	}{
		"a copied file": {
			change: func(x *approvedRebuildFixture) { x.write("main.go", "package main // edited by an agent\n") },
			want:   "; files that changed: main.go",
		},
		"a file the Dockerfile never copies": {
			prepare: func(x *approvedRebuildFixture) { x.write("portal/lib/app.ex", "defmodule App do end\n") },
			change:  func(x *approvedRebuildFixture) { x.write("portal/lib/app.ex", "defmodule App do def x, do: 1 end\n") },
			want:    "; files that changed: portal/lib/app.ex",
		},
		"new and removed files": {
			prepare: func(x *approvedRebuildFixture) { x.write("old.txt", "x\n") },
			change: func(x *approvedRebuildFixture) {
				if err := os.Remove(filepath.Join(x.repo, "old.txt")); err != nil {
					x.t.Fatal(err)
				}
				x.write("tools/new/a.sh", "true\n")
			},
			want: "; files that changed: old.txt, tools/new/a.sh",
		},
		"only a build setting": {
			change: func(x *approvedRebuildFixture) { x.t.Setenv("SOURCE_DATE_EPOCH", "1") },
			want:   "; the files are the approved ones, so the Docker daemon, the Dockerfile in use or a build setting changed",
		},
		"only Coop's client image": {
			change:   newClient,
			approved: true,
			want:     "; this project's files and settings are still the approved ones; only Coop's side changed, such as its client image",
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := newApprovedRebuildFixture(t)
			if test.prepare != nil {
				test.prepare(x)
			}
			x.explicitBuild()
			test.change(x)
			required := x.miss()
			if required.Approved != test.approved || !strings.HasSuffix(required.Reason, test.want) {
				t.Fatalf("refusal = %+v, want approved %v and a reason ending %q", required, test.approved, test.want)
			}
			if test.approved {
				return
			}
			newClient(x)
			var automatic *ProjectBuildRequired
			if err := x.automaticBuild(); !errors.As(err, &automatic) || !strings.HasSuffix(automatic.Reason, test.want) {
				t.Fatalf("automatic build refusal = %v, want a reason ending %q", err, test.want)
			}
		})
	}
}

// A refusal a run prints ends on a whole sentence, and the base it asks for builds without Docker's
// InvalidDefaultArgInFrom warning, the way the scaffold writes it (QA, 2026-11-06).
func TestProjectBuildRefusalsReadWhole(t *testing.T) {
	required := (&ProjectBuildRequired{Dockerfile: ".agent/Dockerfile", Reason: "no explicit project build is recorded for this client image"}).Error()
	if want := "this project's .agent/Dockerfile is not ready: no explicit project build is recorded for this client image. " +
		"Review the Dockerfile and copied build files, then run 'coop build --egress filtered' on the host. " +
		"Project builds use ordinary networking, not the run's network restrictions."; required != want {
		t.Errorf("not-ready refusal = %q, want %q", required, want)
	}
	if got := notDerived(".agent/Dockerfile").Error(); !strings.Contains(got, "start it with `ARG COOP_BASE_IMAGE=coop-box` and `FROM ${COOP_BASE_IMAGE}`") {
		t.Errorf("not-derived refusal = %q, want the scaffold's ARG default", got)
	}
}

// A build Coop refuses leaves no image under the project's name, nor one on disk that nothing else
// holds, and a launch on the same files says why, with the fix, instead of sending the person back to a
// build that fails the same way (QA B-31, 2026-11-06). A build that passes takes the tag and supersedes
// the refusal.
func TestARefusedProjectBuildLeavesNoImageAndTheRunSaysWhy(t *testing.T) {
	pinHome(t)
	for name, test := range map[string]struct {
		refuse func(*fixtureImage, agents.ClientClosure)
		want   string
	}{
		"not built on the client image": {
			refuse: func(i *fixtureImage, _ agents.ClientClosure) {
				i.layers = []string{"sha256:" + strings.Repeat("f", 64)}
			},
			want: "this project's .agent/Dockerfile did not build on coop's client image — start it with `ARG COOP_BASE_IMAGE=coop-box` and `FROM ${COOP_BASE_IMAGE}`, or run with --egress open",
		},
		"replaces a pinned client": {
			refuse: func(i *fixtureImage, closure agents.ClientClosure) {
				file := i.files[closure.Clients[0].Launcher()]
				file.SHA256 = strings.Repeat("9", 64)
				i.files[closure.Clients[0].Launcher()] = file
			},
			want: "changes claude's cli client at ",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, d, repo, write := projectPreparationFixture(t)
			closure := derivedImageFixture(t, d)
			candidate := fixtureCandidate()
			tag := filteredProjectTag(repo, candidate.ClientImage)
			good := "sha256:" + strings.Repeat("3", 64)
			d.mu.Lock()
			image := d.images[fixtureBuiltImage]
			image.id = good
			d.images[good] = image
			d.mu.Unlock()
			tamperBuiltImage(d, func(i *fixtureImage) { test.refuse(i, closure) })

			dir := t.TempDir()
			calls, produced := filepath.Join(dir, "calls"), filepath.Join(dir, "produced")
			writeRepoFile(t, produced, fixtureBuiltImage+"\n")
			script := filepath.Join(dir, "docker")
			writeRepoFile(t, script, "#!/bin/sh\necho \"$@\" >> "+strconv.Quote(calls)+"\n"+
				"case \"$*\" in "+strconv.Quote(tagCount(fixtureBuiltImage))+") echo 0; exit 0 ;; esac\n"+
				"while [ $# -gt 0 ]; do\n  if [ \"$1\" = --iidfile ]; then cat "+strconv.Quote(produced)+" > \"$2\"; fi\n  shift\ndone\n")
			if err := os.Chmod(script, 0o755); err != nil {
				t.Fatal(err)
			}
			rt, cfg := frozenProjectRuntime(t, script), &config.Config{BoxHome: t.TempDir()}
			build := func() error {
				_, err := buildFilteredProject(t.Context(), rt, cfg, d, f.store, repo, candidate, io.Discard)
				return err
			}
			readiness := func() (*ProjectBuildRequired, error) {
				var required *ProjectBuildRequired
				err := checkFilteredProjectReadiness(t.Context(), d, f.store, RunSpec{Repo: repo}, candidate.Runtime)
				if !errors.As(err, &required) {
					return nil, err
				}
				return required, err
			}

			if err := build(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("refused build = %v, want %q", err, test.want)
			}
			recorded := string(mustReadFile(t, calls))
			if strings.Contains(recorded, " -t ") {
				t.Errorf("the build named its image before the proofs:\n%s", recorded)
			}
			if _, _, err := d.Image(t.Context(), tag); !errors.Is(err, runtime.ErrDockerImageMissing) {
				t.Errorf("the refused image took the project's tag %s", tag)
			}
			if !strings.Contains(recorded, "image rm "+fixtureBuiltImage+"\n") {
				t.Errorf("the refused image stayed on disk:\n%s", recorded)
			}

			required, err := readiness()
			if required == nil || !required.Refused || required.Approved || !strings.Contains(required.Reason, test.want) {
				t.Fatalf("readiness on the refused files = %v, want the refusal", err)
			}
			if got := err.Error(); !strings.HasPrefix(got, "Coop refused the last build of this project's box: ") ||
				!strings.HasSuffix(got, ". After you fix it, run 'coop build --egress filtered' on the host.") {
				t.Errorf("run refusal = %q", got)
			}

			write("main.go", "package main // fixed\n")
			if required, err := readiness(); required == nil || required.Refused || !strings.Contains(required.Reason, "no explicit project build") {
				t.Fatalf("readiness on other files = %v, want the plain miss", err)
			}
			writeRepoFile(t, produced, good+"\n")
			if err := build(); err != nil {
				t.Fatal(err)
			}
			if id, _, err := d.Image(t.Context(), tag); err != nil || id != good {
				t.Errorf("the tag names %q (%v) after a build that passed, want %s", id, err, good)
			}
			if err := checkFilteredProjectReadiness(t.Context(), d, f.store, RunSpec{Repo: repo}, candidate.Runtime); err != nil {
				t.Fatalf("readiness after a build that passed = %v", err)
			}
			write("main.go", "package main\n")
			if required, err := readiness(); required == nil || required.Refused || !strings.Contains(required.Reason, "changed since the explicit build") {
				t.Fatalf("readiness back on the refused files = %v, want the change since the last build", err)
			}
			if n := strings.Count(string(mustReadFile(t, calls)), "image rm "); n != 1 {
				t.Errorf("removed %d images, want only the refused one", n)
			}
		})
	}
}

// coop build says when the approval it records covers far more than the image reads, and how to narrow
// it. Every such file voids the approval when it changes, which is how emisar's loop came to be refused
// for edits its box never reads (2026-11-05). A context narrowed to what the Dockerfile copies hears
// nothing.
func TestAProjectBuildWarnsOfFilesItsDockerfileNeverCopies(t *testing.T) {
	x := newApprovedRebuildFixture(t)
	for i := range broadContextFiles {
		x.write(filepath.Join("portal", "lib", "module_"+strconv.Itoa(i)+".ex"), "defmodule M do end\n")
	}
	var errOut strings.Builder
	if _, err := buildFilteredProject(t.Context(), x.rt, x.cfg, x.d, x.f.store, x.repo, x.candidate, &errOut); err != nil {
		t.Fatal(err)
	}
	// The hundred modules and .gitignore; main.go is copied, and the Dockerfile is the build's own.
	const want = "The build context holds 101 files that .agent/Dockerfile never copies, and a change to any of them asks for another build. List only what it copies in .agent/Dockerfile.dockerignore.\n"
	if !strings.Contains(errOut.String(), want) {
		t.Fatalf("coop build said:\n%s\nwant:\n%s", errOut.String(), want)
	}
	x.write(".agent/Dockerfile.dockerignore", "portal/\n")
	errOut.Reset()
	if _, err := buildFilteredProject(t.Context(), x.rt, x.cfg, x.d, x.f.store, x.repo, x.candidate, &errOut); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(errOut.String(), "never copies") {
		t.Fatalf("a narrowed context was still called broad:\n%s", errOut.String())
	}
}

// Only what a Dockerfile reads from the context counts as copied: a path, a folder holding it or a
// pattern. Another stage's files and a URL read none, and a source Coop cannot spell out counts as
// everything, so the count never runs high.
func TestUncopiedContextFilesReadsWhatTheDockerfileCopies(t *testing.T) {
	entries := []contextEntry{{rel: "lib", mode: fs.ModeDir}}
	for _, rel := range []string{"main.go", "lib/a.ex", "libs/b.ex", "docs/c.md", ".agent/Dockerfile", ".dockerignore"} {
		entries = append(entries, contextEntry{rel: rel, mode: 0o644})
	}
	for name, test := range map[string]struct {
		dockerfile string
		want       int
	}{
		"one file":                     {"FROM x\nCOPY main.go /app/\n", 3},
		"a folder":                     {"FROM x\nCOPY lib /app/lib\n", 3},
		"a pattern":                    {"FROM x\nCOPY [\"lib*\", \"/app/\"]\n", 2},
		"everything":                   {"FROM x\nCOPY . /app\n", 0},
		"another stage and a URL":      {"FROM x\nCOPY --from=build /out /app\nADD https://example.com/a /a\n", 4},
		"a source from a build arg":    {"FROM x\nARG SRC\nCOPY $SRC /app\n", 0},
		"a continued, flagged copy":    {"FROM x\nCOPY --chown=node \\\n  main.go docs /app/\n", 2},
		"a heredoc body is not a copy": {"FROM x\nRUN <<EOF\nCOPY . /app\nEOF\n", 4},
	} {
		if got := uncopiedContextFiles([]byte(test.dockerfile), entries, ".agent/Dockerfile"); got != test.want {
			t.Errorf("%s: %d uncopied files, want %d", name, got, test.want)
		}
	}
}

// What a thread asks for: add a network rule, approve it and carry on. With no ignore file, whatever
// the Dockerfile copies, the person's build stands, and the next box runs the image they reviewed. No
// snapshot holds the edit, so `COPY . .` could not bake it in either.
func TestANetworkRuleEditAsksForNoProjectBuild(t *testing.T) {
	for name, dockerfile := range map[string]string{
		"copying everything": "ARG COOP_BASE_IMAGE\nFROM ${COOP_BASE_IMAGE}\nCOPY . /opt/app/\n",
		"copying one file":   reusableDockerfile,
	} {
		t.Run(name, func(t *testing.T) {
			x := newApprovedRebuildFixture(t)
			x.write(".agent/Dockerfile", dockerfile)
			x.write(".agent/project.yaml", "box:\n  egress: filtered\n")
			x.explicitBuild()
			builds := x.builds()

			x.write(".agent/project.yaml", "box:\n  egress: filtered\n  egress_rules:\n    - to: {domain: \"docs.example.com\"}\n      protocol: tls\n      ports: [443]\n")
			x.write(networkstate.ProjectApprovalMarker, "what coop approve leaves\n")
			if err := x.readiness(); err != nil {
				t.Fatalf("the edit asked for a build: %v", err)
			}
			image, _, err := filteredProjectImage(t.Context(), x.rt, x.cfg, x.d, x.f.store, RunSpec{Repo: x.repo, Quiet: true}, x.candidate)
			if err != nil || image != fixtureBuiltImage {
				t.Fatalf("the next box runs %q (%v), want the build the person approved, %q", image, err, fixtureBuiltImage)
			}
			if x.builds() != builds {
				t.Fatalf("the edit ran the builder: %d builds, want %d", x.builds(), builds)
			}

			x.upgradeClient("sha256:"+strings.Repeat("4", 64), "sha256:"+strings.Repeat("5", 64))
			if !x.miss().Approved {
				t.Fatal("the edit took back the approval of the build files")
			}
			prepared, err := snapshotProjectBuild(t.Context(), x.f.store.Path(), x.repo)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			for _, left := range []string{".agent/project.yaml", networkstate.ProjectApprovalMarker} {
				if _, err := os.Lstat(filepath.Join(prepared.Directory(), filepath.FromSlash(left))); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("a build could copy %s: %v", left, err)
				}
			}
			for _, kept := range []string{".agent/Dockerfile", "main.go"} {
				if _, err := os.Lstat(filepath.Join(prepared.Directory(), filepath.FromSlash(kept))); err != nil {
					t.Errorf("a build lost %s: %v", kept, err)
				}
			}
		})
	}
}

// A Dockerfile that really wants the project's settings hears so before anything runs, on every way
// to a build. Docker alone would say a file the person can see was not found.
func TestABuildThatCopiesTheProjectSettingsIsRefused(t *testing.T) {
	x := newApprovedRebuildFixture(t)
	x.write(".agent/project.yaml", "box:\n  egress: filtered\n")
	x.write(".agent/Dockerfile", reusableDockerfile+"COPY .agent/project.yaml /opt/project.yaml\n")
	daemon := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(daemon, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, planned := PlanBuild(runtime.Runtime{Name: daemon}, x.cfg, x.repo, false)
	_, prepared := PrepareFilteredProjectBuild(t.Context(), x.cfg, RunSpec{Repo: x.repo})
	for name, err := range map[string]error{
		"coop build and coop update": planned,
		"Build and continue":         prepared,
		"an automatic rebuild":       x.automaticBuild(),
	} {
		var stage *StageError
		if err == nil || !strings.Contains(err.Error(), ".agent/Dockerfile copies .agent/project.yaml") {
			t.Errorf("%s: %v, want the reason", name, err)
		}
		if errors.As(err, &stage) {
			t.Errorf("%s: the person would be told to fix a path or permissions: %v", name, err)
		}
	}
	if x.builds() != 0 {
		t.Fatalf("the builder ran %d times", x.builds())
	}
}

func TestRefuseCopiedProjectFileReadsWhatDockerReads(t *testing.T) {
	for _, dockerfile := range []string{
		"COPY .agent/project.yaml /opt/\n",
		"copy ./.agent/project.yaml /opt/\n",
		"ADD /.agent/project.yaml /opt/\n",
		"COPY --chown=1000:1000 --link .agent/project.yaml /opt/\n",
		"COPY main.go .agent/project.yaml /opt/\n",
		"COPY [\".agent/project.yaml\", \"/opt/a b/\"]\n",
		"COPY --link [\".agent/project.yaml\",\"/opt/\"]\n",
		"COPY main.go \\\n  # why it is here\n\n  .agent/project.yaml \\\n  /opt/\n",
		"COPY .agent/project.yaml /opt/\r\n",
		"COPY .agent/project.yaml /opt/",
		"COPY .agent/*.yaml /opt/\n",
		"COPY .agent/project.yaml* /opt/\n",
		"FROM scratch AS tools\nCOPY .agent/project.yaml /opt/\nFROM tools\n",
		// A heredoc ends, and what follows is an instruction again.
		"RUN cat <<EOF\nhello\nEOF\nCOPY .agent/project.yaml /opt/\n",
		"RUN cat <<EOF\r\nhello\r\nEOF\r\nCOPY .agent/project.yaml /opt/\r\n",
		"RUN cat <<-EOF\n\thello\n\tEOF\nCOPY .agent/project.yaml /opt/\n",
		"RUN cat <<A <<B\none\nA\ntwo\nB\nCOPY .agent/project.yaml /opt/\n",
		"RUN cat <<EOF \\\n  > /tmp/inner\nhello\nEOF\nCOPY .agent/project.yaml /opt/\n",
		"COPY <<EOF .agent/project.yaml /opt/\nhello\nEOF\n",
		// None of these opens one, as Docker reads them, so the next line is an instruction.
		"RUN cat<<EOF\nCOPY .agent/project.yaml /opt/\n",
		"RUN cat <<<EOF\nCOPY .agent/project.yaml /opt/\n",
		"RUN echo \"<<EOF\"\nCOPY .agent/project.yaml /opt/\n",
		"RUN echo '<<EOF'\nCOPY .agent/project.yaml /opt/\n",
		"RUN [\"cat\", \"<<EOF\"]\nCOPY .agent/project.yaml /opt/\n",
		"ENV NOTE <<EOF\nCOPY .agent/project.yaml /opt/\n",
		// Only the line holding exactly its name ends a body.
		"RUN cat <<EOF\n EOF\nEOF2\nEOF\nCOPY .agent/project.yaml /opt/\n",
	} {
		if refuseCopiedProjectFile([]byte(dockerfile), ".agent/Dockerfile") == nil {
			t.Errorf("not refused: %q", dockerfile)
		}
	}
	for _, dockerfile := range []string{
		"",
		"COPY\n",
		"COPY . /opt/app/\n",
		"COPY ./ /opt/app/\n",
		"COPY .agent /opt/\n",
		"COPY .agent/ /opt/\n",
		"COPY .agent/compose.yml /opt/\n",
		"COPY web/.agent/project.yaml /opt/\n",
		"COPY --from=tools .agent/project.yaml /opt/\n",
		"COPY --link --from=tools /src/.agent/project.yaml /opt/\n",
		"# COPY .agent/project.yaml /opt/\n",
		"COPY main.go .agent/project.yaml\n",
		"COPY [\"main.go\", \".agent/project.yaml\"]\n",
		"COPY main.go /home/node/.agent/project.yaml\n",
		"RUN echo COPY .agent/project.yaml /opt/\n",
		"ONBUILD COPY .agent/project.yaml /opt/\n",
		// A heredoc's body is text, and the image never holds the file.
		"RUN cat > /tmp/inner <<EOF\nCOPY .agent/project.yaml /opt/\nEOF\n",
		"RUN <<EOF\nCOPY .agent/project.yaml /opt/\nEOF\n",
		"run cat 3<<EOF\nCOPY .agent/project.yaml /opt/\nEOF\n",
		"RUN cat <<'EOF'\nCOPY .agent/project.yaml /opt/\nEOF\n",
		"RUN cat <<\"EOF\"\nCOPY .agent/project.yaml /opt/\nEOF\n",
		"RUN cat <<-EOF\n\tCOPY .agent/project.yaml /opt/\n\tEOF\n",
		"RUN cat <<A <<B\nCOPY .agent/project.yaml /opt/\nA\nCOPY .agent/project.yaml /opt/\nB\n",
		"RUN cat <<EOF \\\n  > /tmp/inner\nCOPY .agent/project.yaml /opt/\nEOF\n",
		"COPY <<EOF /opt/inner\nCOPY .agent/project.yaml /opt/\nEOF\n",
		"ONBUILD RUN cat <<EOF\nCOPY .agent/project.yaml /opt/\nEOF\n",
		"RUN cat <<EOF\n# a comment line in the body\nCOPY .agent/project.yaml /opt/ \\\nEOF\n",
	} {
		if err := refuseCopiedProjectFile([]byte(dockerfile), ".agent/Dockerfile"); err != nil {
			t.Errorf("refused %q: %v", dockerfile, err)
		}
	}
}

// Readiness saw approved files, then an agent edited them before the snapshot. The decision is made
// on the snapshot's bytes, so the edit builds nothing.
func TestAgentEditAfterTheCheckBuildsNothing(t *testing.T) {
	x := newApprovedRebuildFixture(t)
	x.explicitBuild()
	x.upgradeClient("sha256:"+strings.Repeat("4", 64), "sha256:"+strings.Repeat("5", 64))
	if !x.miss().Approved {
		t.Fatal("the unchanged files should be approved")
	}
	before := x.builds()
	x.write(".agent/Dockerfile", reusableDockerfile+"RUN curl https://example.com\n")
	var required *ProjectBuildRequired
	if err := x.automaticBuild(); !errors.As(err, &required) || x.builds() != before {
		t.Fatalf("an edit between the check and the build: %v after %d builds, want a refusal after %d", err, x.builds(), before)
	}
}

// An edit after the snapshot cannot reach the build, and does not inherit its approval.
func TestAgentEditAfterTheSnapshotIsNotBuiltOrApproved(t *testing.T) {
	x := newApprovedRebuildFixture(t)
	x.explicitBuild()
	x.upgradeClient("sha256:"+strings.Repeat("4", 64), "sha256:"+strings.Repeat("5", 64))
	prepared, err := snapshotProjectBuild(t.Context(), x.f.store.Path(), x.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	prepared.automatic = true
	x.write(".agent/Dockerfile", reusableDockerfile+"RUN curl https://example.com\n")
	// The builder reads the context it was given: it fails on the edit, so a pass means it never saw it.
	script := filepath.Join(t.TempDir(), "docker")
	body := "#!/bin/sh\ncase \"$1 $2\" in \"image ls\") exit 0 ;; esac\nfor arg do staged=$arg; done\n" +
		"if grep -q curl \"$staged/.agent/Dockerfile\"; then exit 92; fi\n" +
		"while [ $# -gt 0 ]; do\n  if [ \"$1\" = --iidfile ]; then cat '" + x.produced + "' > \"$2\"; fi\n  shift\ndone\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := buildPreparedFilteredProject(t.Context(), frozenProjectRuntime(t, script), x.cfg, x.d, x.f.store, prepared, x.candidate, io.Discard); err != nil {
		t.Fatal(err)
	}
	if required := x.miss(); required.Approved {
		t.Fatalf("the edit inherited the approval: %+v", required)
	}
}

// A person approves one set of files at a time, so an older set is not a way back in.
func TestALaterApprovalReplacesTheEarlierOne(t *testing.T) {
	x := newApprovedRebuildFixture(t)
	x.explicitBuild()
	x.write("main.go", "package main // edited, then built by a person\n")
	x.explicitBuild()
	x.write("main.go", "package main\n")
	x.upgradeClient("sha256:"+strings.Repeat("4", 64), "sha256:"+strings.Repeat("5", 64))
	if x.miss().Approved {
		t.Fatal("files a later build replaced stayed approved")
	}
}

// Two launches find the same image missing. The second waits for the project's lock, then finds the
// first one's image: it builds nothing and stages nothing.
func TestSecondAutomaticBuildReusesTheFirst(t *testing.T) {
	x := newApprovedRebuildFixture(t)
	x.explicitBuild()
	x.upgradeClient("sha256:"+strings.Repeat("4", 64), "sha256:"+strings.Repeat("5", 64))
	if err := x.automaticBuild(); err != nil {
		t.Fatal(err)
	}
	// A new snapshot reaps one nobody holds, so this one tells whether a launch staged anything.
	marker, lease, err := newProjectReviewSnapshot(t.Context(), x.f.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	before := x.builds()
	change, err := rebuildApprovedLocked(t.Context(), x.rt, x.cfg, x.d, x.f.store, x.repo, x.candidate, nil, io.Discard)
	if err != nil || x.builds() != before || change != (ImageChange{}) {
		t.Fatalf("the second launch: %+v, %v after %d builds, want none after %d", change, err, x.builds(), before)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the second launch staged a snapshot to find the image already built: %v", err)
	}

	// A launch that does have to build does stage one, which is what makes the marker a proof.
	x.upgradeClient("sha256:"+strings.Repeat("6", 64), "sha256:"+strings.Repeat("7", 64))
	if err := x.automaticBuild(); err != nil || x.builds() != before+1 {
		t.Fatalf("a real rebuild: %v after %d builds, want %d", err, x.builds(), before+1)
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a real rebuild left the abandoned snapshot, so the marker proves nothing: %v", err)
	}
}

// A launch that finds another build of its project running waits with no copy of the checkout on
// disk, however many windows are waiting beside it.
func TestALaunchWaitingForTheBuildLockHoldsNoSnapshot(t *testing.T) {
	x := newApprovedRebuildFixture(t)
	x.explicitBuild()
	x.upgradeClient("sha256:"+strings.Repeat("4", 64), "sha256:"+strings.Repeat("5", 64))
	root, err := NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	canonical, err := canonicalProjectDir(x.repo)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err = store.LockProjectBuild(t.Context(), canonical, nil, func() error {
		said := make(chan struct{})
		result := make(chan error, 1)
		go func() {
			_, err := rebuildApprovedProject(ctx, x.rt, x.cfg, x.repo, nil, io.Discard, onFirstWrite(func() { close(said) }))
			result <- err
		}()
		select {
		case <-said:
		case err := <-result:
			t.Fatalf("the launch did not wait for the lock: %v", err)
		}
		entries, err := os.ReadDir(store.Path())
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), projectReviewPrefix) {
				t.Errorf("a waiting launch holds a snapshot: %s", entry.Name())
			}
		}
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled wait returned %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// onFirstWrite is a writer that says when something was first written to it.
type onFirstWrite func()

func (f onFirstWrite) Write(p []byte) (int, error) {
	f()
	return len(p), nil
}

func TestAdmissionRebuildsOnlyApprovedFilesAndOnlyWhenAllowed(t *testing.T) {
	approved := &ProjectBuildRequired{Dockerfile: ".agent/Dockerfile", Reason: "stale", Approved: true}
	waits := &ProjectBuildRequired{Dockerfile: ".agent/Dockerfile", Reason: "changed"}
	rebuild := NetworkAdmission{ProjectBuild: RebuildApproved}
	for name, test := range map[string]struct {
		options  NetworkAdmission
		first    error
		rebuild  error
		rebuilds int
		admits   int
		want     error
	}{
		"ready":                     {options: rebuild, admits: 1},
		"approved files":            {options: rebuild, first: approved, rebuilds: 1, admits: 2},
		"other files":               {options: rebuild, first: waits, admits: 1, want: waits},
		"any other failure":         {options: rebuild, first: io.ErrUnexpectedEOF, admits: 1, want: io.ErrUnexpectedEOF},
		"a caller that did not ask": {first: approved, admits: 1, want: approved},
		"a caller that refuses":     {options: NetworkAdmission{ProjectBuild: RefuseProjectBuild}, first: approved, admits: 1, want: approved},
		"an editor connecting":      {options: NetworkAdmission{ProjectBuild: DeferProjectBuild}, first: approved, admits: 1, want: approved},
		"an editor re-admitting":    {options: NetworkAdmission{ExistingQualification: true, ProjectBuild: RebuildApproved}, first: approved, admits: 1, want: approved},
		"a failed rebuild":          {options: rebuild, first: approved, rebuild: io.ErrClosedPipe, rebuilds: 1, admits: 1, want: io.ErrClosedPipe},
	} {
		t.Run(name, func(t *testing.T) {
			admits, rebuilds := 0, 0
			_, err := admitRebuildingApproved(test.options, func() (*CapturedEgress, error) {
				admits++
				if admits == 1 {
					return nil, test.first
				}
				return nil, nil
			}, func(dfRel string) error {
				rebuilds++
				if dfRel != ".agent/Dockerfile" {
					t.Errorf("rebuild of %q", dfRel)
				}
				return test.rebuild
			})
			if !errors.Is(err, test.want) || admits != test.admits || rebuilds != test.rebuilds {
				t.Fatalf("err %v, %d admits, %d rebuilds; want %v, %d, %d", err, admits, rebuilds, test.want, test.admits, test.rebuilds)
			}
		})
	}
}

// A box that only answers an editor's handshake runs Coop's locked image, however the project's own
// build stands. It is the only filtered box that needs no project build, and no turn runs in it.
func TestHandshakeBoxRunsTheLockedImage(t *testing.T) {
	x := newApprovedRebuildFixture(t)
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	f := &filteredExecution{store: x.f.store, docker: x.d}
	if err := f.selectImage(t.Context(), recorderRuntime(t, recorder), x.cfg, RunSpec{Repo: x.repo, HandshakeOnly: true, Quiet: true}, x.candidate, true); err != nil {
		t.Fatal(err)
	}
	if f.image != x.candidate.ClientImage {
		t.Fatalf("the handshake box runs %q", f.image)
	}
	if err := f.selectImage(t.Context(), recorderRuntime(t, recorder), x.cfg, RunSpec{Repo: x.repo, Quiet: true}, x.candidate, true); err == nil {
		t.Fatal("a real box started with no project build")
	}
}

// A project file an agent broke stops a real box, and must not stop the box that says so: it gets past
// the file to the next check, which here is the capture this test leaves out.
func TestHandshakeBoxReadsNoProjectFile(t *testing.T) {
	pinHome(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("box: [broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "filtered"}
	rt := runtime.Runtime{Name: "must-not-execute"}
	spec := RunSpec{Image: "i", Repo: repo, Cmd: []string{"true"}, Batch: true, Quiet: true}
	if _, err := Run(cfg, rt, spec); err == nil || strings.Contains(err.Error(), "host policy capture") {
		t.Fatalf("a real box with a broken project file = %v", err)
	}
	spec.HandshakeOnly = true
	if _, err := Run(cfg, rt, spec); err == nil || !strings.Contains(err.Error(), "host policy capture") {
		t.Fatalf("a handshake box with a broken project file = %v", err)
	}
}

// Nothing runs in a handshake box to use a port, join a service network or change the project, and a
// real box is untouched.
func TestHandshakeBoxPublishesNothing(t *testing.T) {
	real := RunSpec{Network: true, Serve: true, servePorts: []int{4000}}
	if got := real.forHandshake(); !got.Network || !got.Serve || len(got.servePorts) != 1 || got.RepoReadOnly {
		t.Fatalf("a real box lost its ports or services, or its project: %+v", got)
	}
	handshake := real
	handshake.HandshakeOnly = true
	if got := handshake.forHandshake(); got.Network || got.Serve || got.servePorts != nil || !got.RepoReadOnly {
		t.Fatalf("a handshake box kept its ports or services, or could write the project: %+v", got)
	}
}

// The editor's early answer matches the refusal the launch would give.
func TestEditorServicesReadiness(t *testing.T) {
	services := egress.Snapshot{Grants: []egress.Grant{{Rule: egress.Rule{To: egress.Destination{Service: "db"}, Protocol: "tcp", Ports: []int{5432}}}}}
	for name, test := range map[string]struct {
		yaml    string
		compose bool
		policy  egress.Snapshot
		network bool
		refuses bool
	}{
		"no services":               {yaml: "", network: true},
		"services and no rules":     {yaml: "", compose: true, network: true, refuses: true},
		"services turned off":       {yaml: "box:\n  network: false\n", compose: true, network: true},
		"session without a network": {yaml: "", compose: true},
		"services with rules":       {yaml: "", compose: true, network: true, policy: services},
	} {
		t.Run(name, func(t *testing.T) {
			pinHome(t)
			repo, write := gitProject(t, reusableDockerfile)
			if test.yaml != "" {
				write(".agent/project.yaml", test.yaml)
			}
			if test.compose {
				write(".agent/compose.yml", "services:\n  db:\n    image: postgres\n    ports: [\"5432\"]\n")
			}
			p, err := project.Load(repo)
			if err != nil {
				t.Fatal(err)
			}
			err = editorServicesReadiness(&config.Config{}, RunSpec{Repo: repo, Agent: "claude", Network: test.network}, p, test.policy)
			var refused *ServicesNeedRules
			if test.refuses != errors.As(err, &refused) || (!test.refuses && err != nil) {
				t.Fatalf("readiness = %v, want refusal %v", err, test.refuses)
			}
			if test.refuses && (!strings.Contains(err.Error(), "coop approve") || !strings.Contains(err.Error(), "box.network: false")) {
				t.Fatalf("the refusal names no remedy: %v", err)
			}
		})
	}
}

// An editor connects first: admission skips the build question and goes on to qualify the host, so a
// box can start. Any other launch still refuses before that work.
func TestFilteredAdmissionDefersTheProjectBuildForAnEditor(t *testing.T) {
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
	options := NetworkAdmission{InvocationMode: &mode, ExistingQualification: true}
	options.ProjectBuild = DeferProjectBuild
	if _, err := AdmitNetwork(cfg.Clone(), runtime.Runtime{Name: script}, RunSpec{Repo: canonical}, options); !errors.Is(err, ErrNetworkSetupRequired) {
		t.Fatalf("a deferred admission stopped at %v, want it to reach the host's setup", err)
	}
	options.ProjectBuild = RefuseProjectBuild
	var required *ProjectBuildRequired
	if _, err := AdmitNetwork(cfg.Clone(), runtime.Runtime{Name: script}, RunSpec{Repo: canonical}, options); !errors.As(err, &required) {
		t.Fatalf("an ordinary admission = %v, want the build refusal", err)
	}
}

// Buildx reads file-backed controls from its environment. A person who starts a build answers for
// theirs; a rebuild nobody started must not follow one to a file an agent can edit.
func TestAutomaticRebuildGetsAFiniteEnvironment(t *testing.T) {
	x := newApprovedRebuildFixture(t)
	seen := filepath.Join(t.TempDir(), "environment")
	script := filepath.Join(t.TempDir(), "docker")
	body := "#!/bin/sh\ncase \"$1 $2\" in \"image ls\") exit 0 ;; esac\nenv > '" + seen + "'\n" +
		"while [ $# -gt 0 ]; do\n  if [ \"$1\" = --iidfile ]; then cat '" + x.produced + "' > \"$2\"; fi\n  shift\ndone\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	x.rt = frozenProjectRuntime(t, script)
	t.Setenv("EXPERIMENTAL_BUILDKIT_SOURCE_POLICY", filepath.Join(x.repo, "policy.json"))
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")
	x.explicitBuild()
	if explicit := string(mustReadFile(t, seen)); !strings.Contains(explicit, "EXPERIMENTAL_BUILDKIT_SOURCE_POLICY=") {
		t.Fatalf("a person's own build lost their environment: %s", explicit)
	}
	x.upgradeClient("sha256:"+strings.Repeat("4", 64), "sha256:"+strings.Repeat("5", 64))
	if err := x.automaticBuild(); err != nil {
		t.Fatal(err)
	}
	automatic := string(mustReadFile(t, seen))
	if strings.Contains(automatic, "EXPERIMENTAL_BUILDKIT_SOURCE_POLICY") {
		t.Fatalf("an automatic rebuild followed a file-backed control: %s", automatic)
	}
	if !strings.Contains(automatic, "SOURCE_DATE_EPOCH=1700000000") || !strings.Contains(automatic, "PATH=") {
		t.Fatalf("an automatic rebuild lost what its approval covers or what it needs to run: %s", automatic)
	}
}

// An agent chooses every name staged. On a volume that folds case, its link A is also the path to a/x,
// so staging must never follow a link out of the snapshot, nor overwrite what it already wrote.
func TestStagingStaysInsideTheSnapshot(t *testing.T) {
	outside := t.TempDir()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := stageEntry(root, contextEntry{rel: "link", mode: fs.ModeSymlink | 0o777, target: outside}, nil); err != nil {
		t.Fatal(err)
	}
	if err := stageEntry(root, contextEntry{rel: "link/planted", mode: 0o644}, strings.NewReader("planted")); err == nil {
		t.Error("staging wrote through a link")
	}
	if err := stageEntry(root, contextEntry{rel: "link/sub", mode: fs.ModeDir | 0o755}, nil); err == nil {
		t.Error("staging made a directory through a link")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("staging reached outside the snapshot: %v, %v", entries, err)
	}
	if err := stageEntry(root, contextEntry{rel: "file", mode: 0o644}, strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	if err := stageEntry(root, contextEntry{rel: "file", mode: 0o644}, strings.NewReader("second")); err == nil {
		t.Error("a colliding name overwrote a staged file")
	}
	if data, err := os.ReadFile(filepath.Join(dir, "file")); err != nil || string(data) != "first" {
		t.Fatalf("staged file = %q, %v", data, err)
	}
}

// Two checkouts with one folder name are two projects: each keeps its own image and approval.
func TestSameNamedProjectsGetTheirOwnTags(t *testing.T) {
	first, second := filepath.Join(t.TempDir(), "app"), filepath.Join(t.TempDir(), "app")
	for _, repo := range []string{first, second} {
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if filteredProjectTag(first, fixtureLockedImage) == filteredProjectTag(second, fixtureLockedImage) {
		t.Fatal("two checkouts share a tag, so they would take turns rebuilding over each other")
	}
	tag := filteredProjectTag(first, fixtureLockedImage)
	if derivedImageProject(tag) == "" {
		t.Fatalf("reclaim no longer recognizes %q as a project image", tag)
	}
}

// A waiting editor session reads this every two seconds instead of the project's build files, so it
// has to change whenever a person may have settled the wait.
func TestEditorBoxStampChangesWhenAPersonMayHaveActed(t *testing.T) {
	x := newApprovedRebuildFixture(t)
	capture := &CapturedEgress{Store: x.f.store, Project: x.repo, Fingerprint: "policy"}
	spec := RunSpec{Repo: x.repo}
	stamp := EditorBoxStamp(capture, spec)
	if again := EditorBoxStamp(capture, spec); again != stamp {
		t.Fatalf("nothing changed, but the stamp did: %q, %q", stamp, again)
	}
	for name, change := range map[string]func(){
		"a project build finished": func() {
			if err := x.f.store.RecordProjectBuildReview(x.repo, strings.Repeat("a", 64), nil); err != nil {
				t.Fatal(err)
			}
		},
		"the project's configuration": func() { x.write(".agent/project.yaml", "box:\n  network: false\n") },
		"the Dockerfile":              func() { x.write(".agent/Dockerfile", reusableDockerfile+"RUN true\n") },
		"the approved policy":         func() { capture.Fingerprint = "newer" },
		"the service launch lock": func() {
			release, err := forkspace.LockServiceLaunch(t.Context(), x.repo, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(release)
		},
		"an approval, anywhere on this host": func() {
			review, err := x.f.store.ReviewApproval(x.repo, egress.Filtered, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := x.f.store.Approve(t.Context(), x.repo, egress.Filtered, nil, nil, nil, review.Digest); err != nil {
				t.Fatal(err)
			}
		},
	} {
		change()
		next := EditorBoxStamp(capture, spec)
		if next == stamp {
			t.Errorf("%s left the stamp at %q", name, stamp)
		}
		stamp = next
	}
}

// A capture that leaves out what its project asks for is a wait in itself, whatever else is ready.
func TestEditorBoxReadinessSaysACaptureOnlyHolds(t *testing.T) {
	x := newApprovedRebuildFixture(t)
	pending := &networkstate.PendingApproval{Reason: "this project asks for network access that has not been approved"}
	capture := &CapturedEgress{Store: x.f.store, Project: x.repo, Fingerprint: "policy", Pending: pending}
	if err := EditorBoxReadiness(t.Context(), x.cfg, x.rt, capture, RunSpec{Repo: x.repo}); !errors.Is(err, pending) {
		t.Fatalf("readiness = %v, want the pending request", err)
	}
	if !capture.HoldsOnly() {
		t.Fatal("a capture with a pending request lets a real box start")
	}
	var none *CapturedEgress
	if none.HoldsOnly() || (&CapturedEgress{}).HoldsOnly() {
		t.Fatal("an ordinary capture, or none at all, was said to only hold")
	}
}

// A file that does not load is the answer under any capture, and before anything else is asked: read
// as a file that asks for nothing, it would send a person off to build something it does not ask for.
// Once it loads, a capture admitted without it still says only a child that holds prompts may run.
func TestEditorBoxReadinessSaysTheProjectFileIsBroken(t *testing.T) {
	x := newApprovedRebuildFixture(t)
	pending := &networkstate.PendingApproval{Reason: "this project asks for network access that has not been approved"}
	real := &CapturedEgress{Store: x.f.store, Project: x.repo, Fingerprint: "policy"}
	onBrokenFile := &CapturedEgress{Store: x.f.store, Project: x.repo, Fingerprint: "policy", BrokenFile: true}
	onPending := &CapturedEgress{Store: x.f.store, Project: x.repo, Fingerprint: "policy", Pending: pending}
	x.write(".agent/project.yaml", "box: [broken\n")
	for name, capture := range map[string]*CapturedEgress{"a real capture": real, "one admitted on a broken file": onBrokenFile, "one admitted on a request": onPending} {
		var broken *BrokenProjectFile
		if err := EditorBoxReadiness(t.Context(), x.cfg, x.rt, capture, RunSpec{Repo: x.repo}); !errors.As(err, &broken) || !strings.Contains(err.Error(), ".agent/project.yaml") {
			t.Fatalf("readiness under %s = %v, want the broken file", name, err)
		}
	}
	if !onBrokenFile.HoldsOnly() {
		t.Fatal("a capture admitted on a broken file lets a real box start")
	}
	x.write(".agent/project.yaml", "box:\n  network: false\n")
	if err := EditorBoxReadiness(t.Context(), x.cfg, x.rt, onBrokenFile, RunSpec{Repo: x.repo}); !errors.Is(err, ErrProjectNotAdmitted) {
		t.Fatalf("readiness once the file loads = %v, want the capture to go on holding until it is replaced", err)
	}
	if err := EditorBoxReadiness(t.Context(), x.cfg, x.rt, onPending, RunSpec{Repo: x.repo}); !errors.Is(err, pending) {
		t.Fatalf("readiness once the file loads = %v, want the pending request", err)
	}
}
