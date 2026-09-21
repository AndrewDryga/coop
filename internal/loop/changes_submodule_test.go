package loop

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestLoopReviewIncludesCommittedSubmoduleUpdates(t *testing.T) {
	global := filepath.Join(t.TempDir(), "global")
	writeTaskFile(t, global, "[diff]\n\tsubmodule = diff\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Cleanup(forkspace.CloseGitViews)
	module, moduleGit := gitrepo.New(t)
	writeTaskFile(t, filepath.Join(module, ".gitattributes"), "*.go diff=audit\n")
	writeTaskFile(t, filepath.Join(module, "module.go"), "package example\nconst Version = 1\n")
	moduleGit("add", ".gitattributes", "module.go")
	moduleGit("commit", "-qm", "first version")
	first := gitOut(module, "rev-parse", "HEAD")
	writeTaskFile(t, filepath.Join(module, "module.go"), "package example\nconst Version = 2\n")
	moduleGit("add", "module.go")
	moduleGit("commit", "-qm", "second version")
	second := gitOut(module, "rev-parse", "HEAD")

	repo, git := gitrepo.New(t)
	const path = "deps/example"
	git("-c", "protocol.file.allow=always", "submodule", "add", "-q", module, path)
	git("-C", path, "checkout", "-q", first)
	git("add", ".gitmodules", path)
	git("commit", "-qm", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	git("-C", path, "checkout", "-q", second)
	git("add", path)
	git("commit", "-qm", "Update dependency\n\nCoop-Task: task-submodule")
	head := gitOut(repo, "rev-parse", "HEAD")
	marker := filepath.Join(t.TempDir(), "host-execution")
	t.Setenv("COOP_TEST_SUBMODULE_MARKER", marker)
	driver := filepath.Join(t.TempDir(), "external-diff.sh")
	writeTaskFile(t, driver, "#!/bin/sh\nprintf invoked > \"$COOP_TEST_SUBMODULE_MARKER\"\ncat \"$1\"\n")
	if err := os.Chmod(driver, 0o700); err != nil {
		t.Fatal(err)
	}
	git("-C", path, "config", "diff.audit.command", driver)

	if got := rangeFiles(repo, base+".."+head); !slices.Equal(got, []string{path}) {
		t.Errorf("changed paths = %q, want submodule %q", got, path)
	}
	changes := loopChanges(repo, base, head, nil)
	if !strings.Contains(changes.stat, path) {
		t.Errorf("review stat omitted submodule: %q", changes.stat)
	}
	if !slices.Equal(changes.subsystems, []string{"deps"}) {
		t.Errorf("affected areas omitted submodule: %q", changes.subsystems)
	}
	packet := reviewPacket(repo, nil, nil, changes)
	for _, want := range []string{"diff --git a/" + path, "-Subproject commit " + first, "+Subproject commit " + second} {
		if !strings.Contains(packet, want) {
			t.Errorf("review packet omitted %q:\n%s", want, packet)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("review ran an untrusted submodule driver on the host: %v", err)
	}

	// An inline diff really would run the driver; parent diff flags do not protect its child.
	args := append(append([]string{"-C", repo}, forkspace.GitHardening...),
		"show", "--submodule=diff", "--no-ext-diff", "--no-textconv", head, "--")
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("raw inline-diff control: %v: %s", err, out)
	}
	if content, err := os.ReadFile(marker); err != nil || string(content) != "invoked" {
		t.Fatalf("inline-diff control did not run the driver: %q, %v; output: %s", content, err, out)
	}
}
