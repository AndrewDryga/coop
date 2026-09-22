package box

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// gitProject is a Git repository with a box Dockerfile, under a Git config that is only the test's
// own: ignoredBuildPaths reads the ambient one, and a developer's excludes must not change what is
// selected.
func gitProject(t *testing.T, dockerfile string) (string, func(rel, body string)) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", "build/\n")
	write(".agent/Dockerfile", dockerfile)
	write("main.go", "package main\n")
	cmd := exec.Command("git", "-C", repo, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return repo, write
}

const reusableDockerfile = "ARG COOP_BASE_IMAGE\nFROM ${COOP_BASE_IMAGE}\nUSER root\nRUN echo layer > /opt/marker\nUSER node\nCOPY main.go /opt/main.go\n"

func TestFilteredProjectImageRequiresExplicitBuild(t *testing.T) {
	f, d := filteredFixture(t)
	derivedImageFixture(t, d)
	definition, _, _, err := lockedImageDefinition(agents.ClientPlatform{OS: "linux", Architecture: "arm64", Libc: "glibc"})
	if err != nil {
		t.Fatal(err)
	}
	d.images[definition.Tag] = d.images[fixtureLockedImage]
	repo, _ := gitProject(t, reusableDockerfile)
	marker := filepath.Join(t.TempDir(), "builder-ran")
	script := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, err = filteredProjectImage(context.Background(), runtime.Runtime{Name: script}, &config.Config{}, d, f.store,
		RunSpec{Repo: repo, Quiet: true}, fixtureCandidate())
	if _, statErr := os.Stat(marker); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("a restricted launch executed repository build instructions: %v (launch: %v)", statErr, err)
	}
	if err == nil || !strings.Contains(err.Error(), "coop build --egress filtered") {
		t.Fatalf("missing explicit build recovery: %v", err)
	}
}

func projectContextDigest(t *testing.T, repo string) string {
	t.Helper()
	entries, err := buildContextSelection(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := contextDigest(context.Background(), repo, entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// The digest follows everything a build could read from the tree — contents, modes, links, new
// authored files, empty directories, ignore rules — and nothing it cannot: a touched file, ignored
// output and a shadowed secret leave it as it was.
func TestContextDigestFollowsWhatTheBuildReads(t *testing.T) {
	repo, write := gitProject(t, reusableDockerfile)
	write(".env", "SECRET=1\n")
	if err := os.Symlink("main.go", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	digest := projectContextDigest(t, repo)
	for _, step := range []struct {
		name    string
		change  func()
		changes bool
	}{
		{"a touched file", func() {
			if err := os.Chtimes(filepath.Join(repo, "main.go"), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"ignored build output", func() { write("build/out.bin", "generated\n") }, false},
		{"a shadowed secret", func() { write(".env", "SECRET=2\n") }, false},
		{"a file's bytes", func() { write("main.go", "package main // edited\n") }, true},
		{"a file's mode", func() {
			if err := os.Chmod(filepath.Join(repo, "main.go"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"an authored untracked file", func() { write("notes.txt", "draft\n") }, true},
		{"a link's target", func() {
			if err := os.Remove(filepath.Join(repo, "link")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("notes.txt", filepath.Join(repo, "link")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"an empty directory", func() {
			if err := os.Mkdir(filepath.Join(repo, "empty"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"an ignore rule", func() { write(".gitignore", "build/\nnotes.txt\n") }, true},
	} {
		step.change()
		next := projectContextDigest(t, repo)
		if (next != digest) != step.changes {
			t.Errorf("%s: digest changed = %v, want %v", step.name, next != digest, step.changes)
		}
		digest = next
	}
}

// A staged context is exactly what its digest says, read once: the same digest as the check that
// reads without copying, and each file with the repository's own mode — not the one the process
// umask would have given the copy.
func TestStagedContextIsWhatItsDigestSays(t *testing.T) {
	repo, write := gitProject(t, reusableDockerfile)
	write("shared.txt", "group-writable\n")
	if err := os.Chmod(filepath.Join(repo, "shared.txt"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside", filepath.Join(repo, "dangling")); err != nil {
		t.Fatal(err)
	}
	entries, err := buildContextSelection(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	checked, err := contextDigest(context.Background(), repo, entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir, staged, cleanup, err := stageContextEntries(context.Background(), repo, entries)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if staged != checked {
		t.Fatalf("the staged context hashed %s, the check %s", staged, checked)
	}
	if info, err := os.Lstat(filepath.Join(dir, "shared.txt")); err != nil || info.Mode().Perm() != 0o666 {
		t.Fatalf("staged mode = %v (%v), want the repository's 0666", info.Mode().Perm(), err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "dangling")); err != nil || target != "../outside" {
		t.Fatalf("a link was not staged as itself: %q (%v)", target, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := contextDigest(cancelled, repo, entries, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled read went on: %v", err)
	}
}

// A file deleted between the selection and its read is left out, as a walk a moment later would
// have left it: an editor or an agent saving while a box starts must not fail the launch.
func TestContextLeavesOutAFileDeletedSinceSelection(t *testing.T) {
	repo, write := gitProject(t, reusableDockerfile)
	write("scratch.txt", "soon gone\n")
	entries, err := buildContextSelection(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "scratch.txt")); err != nil {
		t.Fatal(err)
	}
	want := projectContextDigest(t, repo)
	if got, err := contextDigest(context.Background(), repo, entries, nil); err != nil || got != want {
		t.Fatalf("a deleted file: digest %s (%v), want the tree without it %s", got, err, want)
	}
	dir, staged, cleanup, err := stageContextEntries(context.Background(), repo, entries)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, statErr := os.Lstat(filepath.Join(dir, "scratch.txt")); staged != want || !os.IsNotExist(statErr) {
		t.Fatalf("staged %s with the deleted file present = %v, want %s without it", staged, statErr == nil, want)
	}
}

// An agent can rewrite the checkout while a launch reads it. A directory swapped for a link after the
// selection cannot make the read pick up a file the selection never judged — ignored output here —
// nor leave the repository: the first is left out, the second refuses the read.
func TestContextReadsOnlyTheFilesTheSelectionJudged(t *testing.T) {
	repo, write := gitProject(t, reusableDockerfile)
	write("src/a.txt", "selected\n")
	write("build/a.txt", "ignored output\n")
	entries, err := buildContextSelection(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(repo, "src"), filepath.Join(repo, "src.old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("build", filepath.Join(repo, "src")); err != nil {
		t.Fatal(err)
	}
	dir, _, cleanup, err := stageContextEntries(context.Background(), repo, entries)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if data, err := os.ReadFile(filepath.Join(dir, "src", "a.txt")); err == nil {
		t.Fatalf("a directory swapped for a link staged %q", data)
	}
	outside := t.TempDir()
	writeCopyFixture(t, filepath.Join(outside, "a.txt"), "host file\n")
	if err := os.Remove(filepath.Join(repo, "src")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "src")); err != nil {
		t.Fatal(err)
	}
	if _, err := contextDigest(context.Background(), repo, entries, nil); err == nil {
		t.Fatal("a link out of the repository was read through")
	}
}

// Docker reads the Dockerfile and its ignore files through a link, straight out of the context: a
// build can be approved only when each is a regular file the digest covered.
func TestProjectBuildNeedsRegularDockerfileAndIgnoreFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".agent", "Dockerfile"), []byte(reusableDockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	regular := []contextEntry{{rel: ".agent", mode: fs.ModeDir}, {rel: filepath.Join(".agent", "Dockerfile"), mode: 0o644}}
	if err := validateProjectBuildFiles(dir, regular, ".agent/Dockerfile"); err != nil {
		t.Fatal("a regular Dockerfile of the context and the base was refused")
	}
	for name, entries := range map[string][]contextEntry{
		"a linked Dockerfile": {{rel: filepath.Join(".agent", "Dockerfile"), mode: fs.ModeSymlink, target: "/elsewhere"}},
		"no Dockerfile":       {{rel: ".agent", mode: fs.ModeDir}},
		"a linked .dockerignore": append(regular[:2:2],
			contextEntry{rel: ".dockerignore", mode: fs.ModeSymlink, target: "/elsewhere"}),
		"a linked Dockerfile.dockerignore": append(regular[:2:2],
			contextEntry{rel: filepath.Join(".agent", "Dockerfile.dockerignore"), mode: fs.ModeSymlink, target: "/elsewhere"}),
	} {
		if err := validateProjectBuildFiles(dir, entries, ".agent/Dockerfile"); err == nil {
			t.Errorf("%s: a build that read outside its digest was reusable", name)
		}
	}
}

func TestFilteredProjectBuildCancellationCleansStaging(t *testing.T) {
	f, d := filteredFixture(t)
	derivedImageFixture(t, d)
	definition, _, _, err := lockedImageDefinition(agents.ClientPlatform{OS: "linux", Architecture: "arm64", Libc: "glibc"})
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.images[definition.Tag] = d.images[fixtureLockedImage]
	d.mu.Unlock()
	repo, _ := gitProject(t, reusableDockerfile)
	root := t.TempDir()
	ready := filepath.Join(root, "ready")
	script := filepath.Join(root, "docker")
	body := "#!/bin/sh\nfor arg do staged=$arg; done\nsleep 60 & helper=$!\n" +
		"trap 'kill \"$helper\" 2>/dev/null; wait \"$helper\"; exit 143' TERM\n" +
		"printf '%s\\n%s\\n%s\\n' \"$$\" \"$helper\" \"$staged\" > " + strconv.Quote(ready) + "\nwait \"$helper\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
	finished := make(chan struct{})
	var buildErr error
	go func() {
		_, _, buildErr = buildFilteredProject(ctx, runtime.Runtime{Name: script}, &config.Config{BoxHome: root}, d, f.store, repo, fixtureCandidate(), io.Discard)
		close(finished)
	}()
	defer func() { cancel(); <-finished }()
	var fields []string
	wait.For(t, "the staged project build", func() bool {
		data, err := os.ReadFile(ready)
		fields = strings.Split(strings.TrimSpace(string(data)), "\n")
		return err == nil && len(fields) == 3
	})
	cancel()
	<-finished
	if buildErr == nil {
		t.Fatal("a cancelled build reported success")
	}
	for _, field := range fields[:2] {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 {
			t.Fatalf("invalid fixture process: %q", field)
		}
		if err := syscall.Kill(pid, 0); err == nil {
			t.Errorf("build process %d survived cancellation", pid)
		}
	}
	if _, err := os.Stat(fields[2]); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("staged build input survived cancellation: %v", err)
	}
	records, err := filepath.Glob(filepath.Join(f.store.Path(), "projectbuild-*.json"))
	if err != nil || len(records) != 0 {
		t.Fatalf("cancelled build published approval: %v / %v", records, err)
	}
}

// Only an explicit build executes instructions. Every reuse proves the exact built image; changed
// inputs, lost images and missing host authority stop rather than restoring build networking.
func TestFilteredProjectImageReusesAnUnchangedBuild(t *testing.T) {
	f, d := filteredFixture(t)
	derivedImageFixture(t, d)
	definition, _, _, err := lockedImageDefinition(agents.ClientPlatform{OS: "linux", Architecture: "arm64", Libc: "glibc"})
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.images[definition.Tag] = d.images[fixtureLockedImage]
	d.mu.Unlock()
	repo, write := gitProject(t, reusableDockerfile)

	// The runtime's build writes the id in produced to its --iidfile and counts itself, and every
	// invocation is recorded: a launch that REUSED a remembered image must not scan for images to
	// reclaim, and this is where that stays true.
	scratch := t.TempDir()
	reuseCfg := &config.Config{BoxHome: t.TempDir()}
	produced, count := filepath.Join(scratch, "produced"), filepath.Join(scratch, "builds")
	setProduced := func(image string) {
		t.Helper()
		if err := os.WriteFile(produced, []byte(image+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setProduced(fixtureBuiltImage)
	script := filepath.Join(scratch, "docker")
	calls := filepath.Join(scratch, "calls")
	body := "#!/bin/sh\necho \"$@\" >> '" + calls + "'\ncase \"$1 $2\" in \"image ls\") exit 0 ;; esac\n" +
		"printf 'build\\n' >> '" + count + "'\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = --iidfile ]; then cat '" + produced + "' > \"$2\"; fi\n  shift\ndone\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	builds := func() int {
		data, _ := os.ReadFile(count)
		return strings.Count(string(data), "build\n")
	}
	scans := func() int {
		data, _ := os.ReadFile(calls)
		return strings.Count(string(data), "image ls")
	}
	launch := func(ctx context.Context, withStore bool) (string, error) {
		store := f.store
		if !withStore {
			store = nil
		}
		image, _, err := filteredProjectImage(ctx, runtime.Runtime{Name: script}, reuseCfg, d, store, RunSpec{Repo: repo, Quiet: true}, fixtureCandidate())
		return image, err
	}
	expect := func(step string, withStore bool, image string, wantBuilds int) {
		t.Helper()
		got, err := launch(context.Background(), withStore)
		if err != nil || got != image || builds() != wantBuilds {
			t.Fatalf("%s: image %q (%v) after %d builds, want %q after %d", step, got, err, builds(), image, wantBuilds)
		}
		// One scan per build and none per reuse: a launch that changed nothing must not go looking
		// for images to reclaim.
		if scans() != wantBuilds {
			t.Fatalf("%s: %d reclaim scans after %d builds", step, scans(), wantBuilds)
		}
	}

	deny := func(step string, withStore bool, wantBuilds int) {
		t.Helper()
		got, err := launch(context.Background(), withStore)
		if err == nil || got != "" || !strings.Contains(err.Error(), "coop build --egress filtered") || builds() != wantBuilds {
			t.Fatalf("%s: image %q, error %v after %d builds, want denial after %d", step, got, err, builds(), wantBuilds)
		}
	}
	build := func() {
		t.Helper()
		if _, _, err := buildFilteredProject(context.Background(), runtime.Runtime{Name: script}, reuseCfg, d, f.store, repo, fixtureCandidate(), io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	deny("the first launch", true, 0)
	build()
	expect("an unchanged tree", true, fixtureBuiltImage, 1)
	write("main.go", "package main // edited\n")
	deny("an edited copied file", true, 1)
	build()
	expect("the explicitly rebuilt tree", true, fixtureBuiltImage, 2)
	write("build/out.bin", "generated\n")
	expect("new ignored output", true, fixtureBuiltImage, 2)
	deny("no host authority", false, 2)
	for _, change := range []func(){
		func() {
			if err := os.Chmod(filepath.Join(repo, "main.go"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		func() { write(".dockerignore", "notes.txt\n") },
		func() { write(".agent/Dockerfile.dockerignore", "other.txt\n") },
	} {
		before := builds()
		change()
		deny("changed build input", true, before)
		build()
		expect("explicit recovery", true, fixtureBuiltImage, before+1)
	}

	rebuilt := "sha256:" + strings.Repeat("3", 64)
	d.mu.Lock()
	d.images[rebuilt] = fixtureImage{id: rebuilt, layers: d.images[fixtureBuiltImage].layers, files: d.images[fixtureBuiltImage].files, tree: d.images[fixtureBuiltImage].tree}
	delete(d.images, fixtureBuiltImage)
	d.mu.Unlock()
	setProduced(rebuilt)
	before := builds()
	deny("an approved image that disappeared", true, before)
	build()
	expect("the rebuilt image", true, rebuilt, before+1)

	// External inputs need no cache-purity parser: only the host's explicit build fetches them.
	write(".agent/Dockerfile", "# syntax=docker/dockerfile:1\n"+reusableDockerfile+"ADD https://example.com/tool.tgz /opt/\nRUN --mount=type=cache,target=/tmp/cache true\n")
	before = builds()
	deny("new Dockerfile instructions", true, before)
	build()
	expect("an explicitly built complex Dockerfile", true, rebuilt, before+1)
	expect("no automatic external fetch on reuse", true, rebuilt, before+1)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	before = builds()
	if _, err := launch(cancelled, true); !errors.Is(err, context.Canceled) || builds() != before {
		t.Fatalf("a cancelled launch: %v after %d builds", err, builds())
	}

	// A build may finish while the checkout changes. Only the staged snapshot was approved.
	body += "printf 'edited during build\\n' > '" + filepath.Join(repo, "main.go") + "'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	build()
	deny("checkout edited during the explicit build", true, before+1)

	// A lost approval store must not print success for an unusable image.
	f.store.Close()
	if _, _, err := buildFilteredProject(context.Background(), runtime.Runtime{Name: script}, reuseCfg, d, f.store, repo, fixtureCandidate(), io.Discard); err == nil || !strings.Contains(err.Error(), "approval could not be saved") {
		t.Fatalf("approval publication failure was hidden: %v", err)
	}
}
