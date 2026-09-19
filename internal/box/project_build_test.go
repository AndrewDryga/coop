package box

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/runtime"
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

// Only a Dockerfile whose inputs are its context and the base it is given may reuse a build; any
// other source Docker could resolve differently next time, and anything unrecognised, builds every
// launch as before.
func TestDockerfileReuseNeedsNoInputBeyondTheContextAndTheBase(t *testing.T) {
	for name, dockerfile := range map[string]string{
		"the base":            reusableDockerfile,
		"lowercase and CRLF":  "arg COOP_BASE_IMAGE\r\nfrom $COOP_BASE_IMAGE\r\nrun echo ok\r\n",
		"stages":              "FROM ${COOP_BASE_IMAGE} AS tools\nRUN make\nFROM tools AS final\nCOPY --from=tools /out /out\nCOPY --from=0 /x /y\n",
		"a continued RUN":     "FROM ${COOP_BASE_IMAGE}\nRUN apt-get update \\\n # a comment Docker drops\n && apt-get install -y jq\n",
		"a comment after":     "FROM ${COOP_BASE_IMAGE}\n# see https://example.com/?a=b\nCOPY --chown=node:node . /src\n",
		"a JSON-form command": "FROM ${COOP_BASE_IMAGE}\nCOPY [\"a b\", \"/c\"]\nCMD [\"sh\"]\n",
		// A backslash before trailing spaces still continues, as BuildKit reads it: the FROM below is
		// only an argument of the RUN.
		"a spaced backslash continues": "FROM ${COOP_BASE_IMAGE}\nRUN echo \\ \nFROM node:24\n",
	} {
		if !dockerfileReusable([]byte(dockerfile)) {
			t.Errorf("%s: a Dockerfile of the context and the base was refused", name)
		}
	}
	for name, dockerfile := range map[string]string{
		"no FROM":             "RUN echo\n",
		"another image":       "FROM node:24\n",
		"a platform flag":     "FROM --platform=linux/amd64 ${COOP_BASE_IMAGE}\n",
		"a later stage name":  "FROM later\nFROM ${COOP_BASE_IMAGE} AS later\n",
		"a syntax directive":  "# syntax=docker/dockerfile:1\nFROM ${COOP_BASE_IMAGE}\n",
		"an escape directive": "# escape=`\nFROM ${COOP_BASE_IMAGE}\n",
		"a directive and BOM": "\ufeff# syntax=docker/dockerfile:1\nFROM ${COOP_BASE_IMAGE}\n",
		"a heredoc":           "FROM ${COOP_BASE_IMAGE}\nRUN <<EOF\necho hi\nEOF\n",
		"ADD":                 "FROM ${COOP_BASE_IMAGE}\nADD https://example.com/tool.tgz /opt/\n",
		"COPY from an image":  "FROM ${COOP_BASE_IMAGE}\nCOPY --from=alpine:3 /bin/busybox /bin/\n",
		"COPY from later":     "FROM ${COOP_BASE_IMAGE}\nCOPY --from=1 /x /y\nFROM ${COOP_BASE_IMAGE}\n",
		"a bare --from":       "FROM ${COOP_BASE_IMAGE}\nCOPY --from alpine:3 /x /y\n",
		"a RUN mount":         "FROM ${COOP_BASE_IMAGE}\nRUN --mount=type=cache,target=/root/.cache pip install x\n",
		"a RUN network flag":  "FROM ${COOP_BASE_IMAGE}\nRUN --network=host curl example.com\n",
		"ONBUILD":             "FROM ${COOP_BASE_IMAGE}\nONBUILD ADD . /app\n",
		"an unknown keyword":  "FROM ${COOP_BASE_IMAGE}\nFETCH example.com\n",
		"a lone backslash":    "FROM ${COOP_BASE_IMAGE}\n\\\n",
		// Each of these is an instruction Docker reads differently from a line-by-line reading.
		"an escaped backslash ends the line": "FROM ${COOP_BASE_IMAGE}\nRUN echo done\\\\\nCOPY --from=alpine:3 /bin/busybox /bb\n",
		"a flag continued mid-word":          "FROM ${COOP_BASE_IMAGE}\nCOPY --fro\\\nm=alpine:3 /bin/busybox /bb\n",
		"a stage continued into an image":    "FROM ${COOP_BASE_IMAGE} AS tools\nRUN make\nCOPY --from=tools\\\n:latest /out /out\n",
		"a quoted flag":                      "FROM ${COOP_BASE_IMAGE}\nCOPY --\"from\"=alpine:3 /x /y\n",
		"an escaped flag":                    "FROM ${COOP_BASE_IMAGE}\nCOPY --fr\\om=alpine:3 /x /y\n",
		"a carriage return between flags":    "FROM ${COOP_BASE_IMAGE}\nCOPY --chown=1\r--from=alpine:3 /a /b\n",
		"a COPY Docker splits at a byte":     "FROM ${COOP_BASE_IMAGE}\nCOPY --chown=à--from=alpine:3 /a /b\n",
		"invalid UTF-8 before a flag":        "FROM ${COOP_BASE_IMAGE}\nRUN \xa0--mount=type=bind,from=alpine,target=/m true\n",
		"a doubled carriage return":          "FROM ${COOP_BASE_IMAGE}\nRUN echo \\\r\r\nCOPY --from=alpine:3 /x /y\n",
	} {
		if dockerfileReusable([]byte(dockerfile)) {
			t.Errorf("%s: a Dockerfile with inputs outside its context was reused", name)
		}
	}
}

// Docker reads the Dockerfile and its ignore files through a link, straight out of the context: a
// build is reusable only when each is a regular file the digest covered.
func TestReusableProjectBuildNeedsRegularDockerfileAndIgnoreFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".agent", "Dockerfile"), []byte(reusableDockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	regular := []contextEntry{{rel: ".agent", mode: fs.ModeDir}, {rel: filepath.Join(".agent", "Dockerfile"), mode: 0o644}}
	if !reusableProjectBuild(dir, regular, ".agent/Dockerfile") {
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
		if reusableProjectBuild(dir, entries, ".agent/Dockerfile") {
			t.Errorf("%s: a build that read outside its digest was reusable", name)
		}
	}
}

// A filtered launch builds the project's image once and reuses that exact image while nothing the
// build reads changes — and builds again the moment something does, when the image it remembered is
// gone, when the Dockerfile has inputs the digest cannot see, or when there is no store to remember
// in. A cancelled launch builds nothing.
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

	// The runtime's build writes the id in produced to its --iidfile and counts itself.
	scratch := t.TempDir()
	produced, count := filepath.Join(scratch, "produced"), filepath.Join(scratch, "builds")
	setProduced := func(image string) {
		t.Helper()
		if err := os.WriteFile(produced, []byte(image+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setProduced(fixtureBuiltImage)
	script := filepath.Join(scratch, "docker")
	body := "#!/bin/sh\nprintf 'build\\n' >> '" + count + "'\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = --iidfile ]; then cat '" + produced + "' > \"$2\"; fi\n  shift\ndone\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	builds := func() int {
		data, _ := os.ReadFile(count)
		return strings.Count(string(data), "build\n")
	}
	launch := func(ctx context.Context, withStore bool) (string, error) {
		store := f.store
		if !withStore {
			store = nil
		}
		return filteredProjectImage(ctx, runtime.Runtime{Name: script}, d, store, RunSpec{Repo: repo, Quiet: true}, fixtureCandidate())
	}
	expect := func(step string, withStore bool, image string, wantBuilds int) {
		t.Helper()
		got, err := launch(context.Background(), withStore)
		if err != nil || got != image || builds() != wantBuilds {
			t.Fatalf("%s: image %q (%v) after %d builds, want %q after %d", step, got, err, builds(), image, wantBuilds)
		}
	}

	expect("the first launch", true, fixtureBuiltImage, 1)
	expect("an unchanged tree", true, fixtureBuiltImage, 1)
	write("main.go", "package main // edited\n")
	expect("an edited file", true, fixtureBuiltImage, 2)
	expect("the edited tree again", true, fixtureBuiltImage, 2)
	write("build/out.bin", "generated\n")
	expect("new ignored output", true, fixtureBuiltImage, 2)
	expect("no store to remember in", false, fixtureBuiltImage, 3)

	// The image it remembered is gone: it builds, and runs what that build made.
	rebuilt := "sha256:" + strings.Repeat("3", 64)
	d.mu.Lock()
	d.images[rebuilt] = fixtureImage{id: rebuilt, layers: d.images[fixtureBuiltImage].layers, files: d.images[fixtureBuiltImage].files, tree: d.images[fixtureBuiltImage].tree}
	delete(d.images, fixtureBuiltImage)
	d.mu.Unlock()
	setProduced(rebuilt)
	expect("a remembered image that is gone", true, rebuilt, 4)
	expect("the rebuilt image", true, rebuilt, 4)

	// A Dockerfile with an input the digest cannot see builds every launch.
	write(".agent/Dockerfile", reusableDockerfile+"ADD https://example.com/tool.tgz /opt/\n")
	expect("an ADD from the network", true, rebuilt, 5)
	expect("the same ADD again", true, rebuilt, 6)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := launch(cancelled, true); !errors.Is(err, context.Canceled) || builds() != 6 {
		t.Fatalf("a cancelled launch: %v after %d builds", err, builds())
	}
}
