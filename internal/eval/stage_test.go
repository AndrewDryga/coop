package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stagedAgentSuite(t *testing.T) *Suite {
	t.Helper()
	path := writeSuite(t, agentSuite, "files/input.txt", "verifiers/hello/verify.sh", "verifiers/hello/expected.txt")
	manifest, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest = []byte(strings.Replace(string(manifest), "    verifier: ./verifiers/hello\n", "    files: ./files\n    verifier: ./verifiers/hello\n", 1))
	if err := os.WriteFile(path, manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	suite, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return suite
}

func TestStageSuiteFreezesInputAndVerifierBytes(t *testing.T) {
	source := stagedAgentSuite(t)
	first, err := StageSuite(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(first.Path)
	if err != nil || reloaded.Cases[0].Timeout != first.Cases[0].Timeout {
		t.Fatalf("retained suite cannot be reloaded: %+v, %v", reloaded, err)
	}
	firstFP := WorkloadFingerprint(first)
	if first.ContentDigest == "" || firstFP == WorkloadFingerprint(source) {
		t.Fatal("staged content did not enter the workload fingerprint")
	}
	input := filepath.Join(first.Dir, first.Cases[0].Files, "input.txt")
	verifier := filepath.Join(first.Dir, first.Cases[0].Verifier, "expected.txt")
	if _, err := os.Stat(filepath.Join(first.Dir, first.Cases[0].Files, "expected.txt")); !os.IsNotExist(err) {
		t.Fatalf("hidden answer entered candidate files: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source.Dir, "files/input.txt"), []byte("changed input"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := StageSuite(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	if WorkloadFingerprint(second) == firstFP {
		t.Fatal("editing an input did not change the frozen workload")
	}
	if body, err := os.ReadFile(input); err != nil || string(body) != "x\n" {
		t.Fatalf("the old staged input changed with its source: %q, %v", body, err)
	}
	if err := os.WriteFile(filepath.Join(source.Dir, "verifiers/hello/expected.txt"), []byte("changed answer"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := StageSuite(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	if WorkloadFingerprint(third) == WorkloadFingerprint(second) {
		t.Fatal("editing a hidden verifier did not change the frozen workload")
	}
	if body, err := os.ReadFile(verifier); err != nil || string(body) != "x\n" {
		t.Fatalf("the old staged verifier changed with its source: %q, %v", body, err)
	}
	identical, err := StageSuite(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	if WorkloadFingerprint(identical) != WorkloadFingerprint(third) {
		t.Fatal("identical suite bytes produced different workloads")
	}
	if err := os.Chmod(filepath.Join(source.Dir, "files/input.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	modeChanged, err := StageSuite(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	if WorkloadFingerprint(modeChanged) == WorkloadFingerprint(third) {
		t.Fatal("an input mode change kept the old workload identity")
	}
	if err := os.WriteFile(filepath.Join(source.Dir, "files/added.txt"), []byte("new path"), 0o644); err != nil {
		t.Fatal(err)
	}
	namesChanged, err := StageSuite(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	if WorkloadFingerprint(namesChanged) == WorkloadFingerprint(modeChanged) {
		t.Fatal("an added input path kept the old workload identity")
	}
}

func TestStageSuiteAgentWithoutFilesGetsAnEmptyInput(t *testing.T) {
	path := writeSuite(t, agentSuite, "verifiers/hello/verify.sh", "reference-solution.txt")
	source, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := StageSuite(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(staged.Dir, staged.Cases[0].Files))
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty agent input includes suite material: %v, %v", entries, err)
	}
}

func TestStageSuiteLoopQueueChangesWorkloadButRecipeDoesNot(t *testing.T) {
	path := writeSuite(t, loopSuite, "loop.yaml", "fixtures/app/main.go", "queues/repo-evolution/00_todo/first/task.md", "verifiers/repo-evolution/verify.sh")
	source, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := StageSuite(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.Dir, "loop.yaml"), []byte("changed recipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	same, err := StageSuite(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	if WorkloadFingerprint(same) != WorkloadFingerprint(first) {
		t.Fatal("loop recipe is a configuration change, not a workload change")
	}
	if err := os.WriteFile(filepath.Join(source.Dir, "queues/repo-evolution/00_todo/first/task.md"), []byte("changed task"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := StageSuite(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	if WorkloadFingerprint(changed) == WorkloadFingerprint(first) {
		t.Fatal("an edited queue was pooled with the old workload")
	}
}

func TestStageSuiteRefusesMissingLinkedAndOversizedInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, source *Suite)
		want string
	}{
		{"missing", func(t *testing.T, s *Suite) {
			t.Helper()
			if err := os.RemoveAll(filepath.Join(s.Dir, "files")); err != nil {
				t.Fatal(err)
			}
		}, "no such file"},
		{"escaping link", func(t *testing.T, s *Suite) {
			t.Helper()
			if err := os.Symlink(t.TempDir(), filepath.Join(s.Dir, "files/escape")); err != nil {
				t.Fatal(err)
			}
		}, "absolute target"},
		{"relative verifier link", func(t *testing.T, s *Suite) {
			t.Helper()
			if err := os.Symlink("../verifiers/hello/expected.txt", filepath.Join(s.Dir, "files/escape")); err != nil {
				t.Fatal(err)
			}
		}, "does not resolve inside"},
		{"oversized", func(t *testing.T, s *Suite) {
			t.Helper()
			if err := os.Truncate(filepath.Join(s.Dir, "files/input.txt"), SnapshotLimit+1); err != nil {
				t.Fatal(err)
			}
		}, "signature limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := stagedAgentSuite(t)
			tc.edit(t, source)
			_, err := StageSuite(t.TempDir(), source)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("stage error = %v; want %q", err, tc.want)
			}
		})
	}
}

func TestStageSuitePreservesInternalRelativeLinks(t *testing.T) {
	source := stagedAgentSuite(t)
	if err := os.Mkdir(filepath.Join(source.Dir, "files", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../input.txt", filepath.Join(source.Dir, "files", "sub", "link")); err != nil {
		t.Fatal(err)
	}
	staged, err := StageSuite(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(staged.Dir, staged.Cases[0].Files, "sub", "link")
	if got, err := os.Readlink(link); err != nil || got != "../input.txt" {
		t.Fatalf("staged internal link = %q, %v", got, err)
	}
}

func TestStageSuiteRefusesCaseFoldedVerifierInsideCandidateInput(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Files"), 0o700); err != nil {
		t.Fatal(err)
	}
	upper, err := os.Stat(filepath.Join(root, "Files"))
	if err != nil {
		t.Fatal(err)
	}
	lower, err := os.Stat(filepath.Join(root, "files"))
	if err != nil || !os.SameFile(upper, lower) {
		t.Skip("filesystem is case-sensitive")
	}
	for _, path := range []string{"Files/hidden/answer", "other/readme", "verifiers/broad/verify.sh"} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := `version: 1
name: physical-overlap
runner: agent
cases:
  - id: broad
    instruction: inspect files
    files: ./Files
    verifier: ./verifiers/broad
    timeout: 1m
  - id: narrow
    instruction: inspect other files
    files: ./other
    verifier: ./files/hidden
    timeout: 1m
`
	path := filepath.Join(root, "suite.yaml")
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StageSuite(t.TempDir(), source); err == nil || !strings.Contains(err.Error(), "physically overlaps") {
		t.Fatalf("case-folded hidden grader was staged as candidate input: %v", err)
	}
}

func TestOpenedSuiteTreeStaysConfinedAfterSourcePathSwap(t *testing.T) {
	suiteDir := t.TempDir()
	inside := filepath.Join(suiteDir, "files")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inside, "visible.txt"), []byte("okay"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "private.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := os.OpenRoot(suiteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	opened, err := openSuiteTree(base, "files")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.root.Close()
	if err := os.Rename(inside, filepath.Join(suiteDir, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, inside); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "frozen")
	remaining := int64(SnapshotLimit)
	entries := stageMaxEntries
	if _, err := stageOpenedTree(opened.root, dest, &remaining, &entries); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "private.txt")); !os.IsNotExist(err) {
		t.Fatalf("swapped source exposed a private file: %v", err)
	}
	if err := opened.stillNamed(base); err == nil {
		t.Fatal("source path swap was not detected before accepting the freeze")
	}
}

func TestStageSuiteRejectsSuiteRootAndManifestReplacement(t *testing.T) {
	t.Run("suite root", func(t *testing.T) {
		source := stagedAgentSuite(t)
		outside := t.TempDir()
		for _, path := range []string{"files/private.txt", "verifiers/hello/verify.sh"} {
			full := filepath.Join(outside, path)
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Rename(source.Dir, source.Dir+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, source.Dir); err != nil {
			t.Fatal(err)
		}
		if _, err := StageSuite(t.TempDir(), source); err == nil || !strings.Contains(err.Error(), "directory changed since load") {
			t.Fatalf("replaced suite root was trusted: %v", err)
		}
	})
	t.Run("manifest bytes", func(t *testing.T) {
		source := stagedAgentSuite(t)
		if err := os.WriteFile(source.Path, []byte(strings.Replace(agentSuite, "print hello", "read secrets", 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := StageSuite(t.TempDir(), source); err == nil || !strings.Contains(err.Error(), "manifest changed since load") {
			t.Fatalf("edited manifest was trusted: %v", err)
		}
	})
}

func TestStageCopyHasOneByteBudgetAcrossTreesAndPreservesModes(t *testing.T) {
	source := stagedAgentSuite(t)
	if err := os.Mkdir(filepath.Join(source.Dir, "files", "readonly"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.Dir, "files", "readonly", "code.txt"), []byte("code"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source.Dir, "files", "readonly"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(source.Dir, "files", "readonly"), 0o700) })
	staged, err := StageSuite(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(staged.Dir, staged.Cases[0].Files, "readonly"), 0o700) })
	info, err := os.Stat(filepath.Join(staged.Dir, staged.Cases[0].Files, "readonly"))
	if err != nil || info.Mode().Perm() != 0o500 {
		t.Fatalf("read-only input directory mode changed: %v, %v", info, err)
	}
	if err := RemoveStagedSuite(staged.Dir); err != nil {
		t.Fatalf("remove read-only staged suite: %v", err)
	}
	if _, err := os.Stat(staged.Dir); !os.IsNotExist(err) {
		t.Fatalf("staged suite was not removed: %v", err)
	}
	base, err := os.OpenRoot(source.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	tree, err := openSuiteTree(base, "files")
	if err != nil {
		t.Fatal(err)
	}
	defer tree.root.Close()
	remaining := int64(10)
	entries := stageMaxEntries
	first := filepath.Join(t.TempDir(), "first")
	if _, err := stageOpenedTree(tree.root, first, &remaining, &entries); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(first, "readonly"), 0o700) })
	if _, err := stageOpenedTree(tree.root, filepath.Join(t.TempDir(), "second"), &remaining, &entries); err == nil || !strings.Contains(err.Error(), "staging limit") {
		t.Fatalf("second tree exceeded shared byte budget: %v", err)
	}
	remaining = int64(100)
	entries = 3 // exactly this tree's two files and one directory fit
	entryFirst := filepath.Join(t.TempDir(), "entries-first")
	if _, err := stageOpenedTree(tree.root, entryFirst, &remaining, &entries); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(entryFirst, "readonly"), 0o700) })
	if _, err := stageOpenedTree(tree.root, filepath.Join(t.TempDir(), "entries-second"), &remaining, &entries); err == nil || !strings.Contains(err.Error(), "entry staging limit") {
		t.Fatalf("second tree exceeded shared entry budget: %v", err)
	}
}

func TestStageSuiteDropsSpecialDirectoryModeWithoutChangingItsInputIdentity(t *testing.T) {
	source := stagedAgentSuite(t)
	dir := filepath.Join(source.Dir, "files", "sticky")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSticky == 0 {
		t.Skip("filesystem did not retain the sticky bit")
	}
	staged, err := StageSuite(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(filepath.Join(staged.Dir, staged.Cases[0].Files, "sticky"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSticky != 0 || info.Mode().Perm() != 0o755 {
		t.Fatalf("staged directory mode = %v, want ordinary 0755", info.Mode())
	}
}

func TestStageSuiteSkipsNestedGitMetadataButKeepsSubmoduleFiles(t *testing.T) {
	source := stagedAgentSuite(t)
	module := filepath.Join(source.Dir, "files", "vendor", "module")
	if err := os.MkdirAll(module, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		".git":    "gitdir: /private/source/history\n",
		"main.go": "package module\n",
	} {
		if err := os.WriteFile(filepath.Join(module, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	staged, err := StageSuite(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	module = filepath.Join(staged.Dir, staged.Cases[0].Files, "vendor", "module")
	if _, err := os.Stat(filepath.Join(module, ".git")); !os.IsNotExist(err) {
		t.Fatalf("nested Git metadata entered candidate input: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(module, "main.go")); err != nil || string(body) != "package module\n" {
		t.Fatalf("submodule working file was lost: %q, %v", body, err)
	}
	if err := os.WriteFile(filepath.Join(source.Dir, "files", ".git"), []byte("gitdir: /private/source/history\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := StageSuite(t.TempDir(), source); err == nil || !strings.Contains(err.Error(), "contains .git") {
		t.Fatalf("top-level checkout metadata was accepted: %v", err)
	}
}
