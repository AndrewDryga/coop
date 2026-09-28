package tasks

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// A no-change completion is the one path that skips review, so its guard has to turn on CONTENT.
// Every case here starts from checkout state a task is ALLOWED to inherit, edits it in a way that
// leaves `git status --porcelain` byte-identical, and requires the completion to be refused anyway.
// The final case is the other half of the claim: inherited work that is genuinely untouched must
// still complete, or the guard would have been "fixed" by refusing everything.
func TestNoChangeCompletionRejectsSameStatusEdits(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	for _, test := range []struct {
		name    string
		inherit func(t *testing.T, repo string, git func(...string))
		edit    func(t *testing.T, repo string, git func(...string))
		refuse  bool
	}{{
		name: "rewriting an inherited unstaged change",
		inherit: func(t *testing.T, repo string, _ func(...string)) {
			writeTaskFile(t, filepath.Join(repo, "source"), "inherited edit\n")
		},
		edit: func(t *testing.T, repo string, _ func(...string)) {
			writeTaskFile(t, filepath.Join(repo, "source"), "smuggled edit\n")
		},
		refuse: true,
	}, {
		name: "rewriting inherited staged content",
		inherit: func(t *testing.T, repo string, git func(...string)) {
			writeTaskFile(t, filepath.Join(repo, "source"), "inherited edit\n")
			git("add", "source")
		},
		edit: func(t *testing.T, repo string, git func(...string)) {
			writeTaskFile(t, filepath.Join(repo, "source"), "smuggled edit\n")
			git("add", "source")
		},
		refuse: true,
	}, {
		name: "rewriting an inherited untracked file",
		inherit: func(t *testing.T, repo string, _ func(...string)) {
			writeTaskFile(t, filepath.Join(repo, "scratch.go"), "package scratch\n")
		},
		edit: func(t *testing.T, repo string, _ func(...string)) {
			writeTaskFile(t, filepath.Join(repo, "scratch.go"), "package scratch // and more\n")
		},
		refuse: true,
	}, {
		// A binary edit renders as the content-free "Binary files differ" without --binary, which
		// would let a changed archive or image through while every label stayed put.
		name: "rewriting inherited binary content",
		inherit: func(t *testing.T, repo string, _ func(...string)) {
			if err := os.WriteFile(filepath.Join(repo, "source"), []byte{0x00, 0x01, 0x02, 0x00}, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		edit: func(t *testing.T, repo string, _ func(...string)) {
			if err := os.WriteFile(filepath.Join(repo, "source"), []byte{0x00, 0x09, 0x09, 0x00}, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		refuse: true,
	}, {
		name:    "hidden tracked edit",
		inherit: func(*testing.T, string, func(...string)) {},
		edit: func(t *testing.T, repo string, git func(...string)) {
			git("update-index", "--assume-unchanged", "source")
			writeTaskFile(t, filepath.Join(repo, "source"), "hidden change\n")
		},
		refuse: true,
	}, {
		name: "inherited skip-worktree content",
		inherit: func(t *testing.T, repo string, git func(...string)) {
			git("update-index", "--skip-worktree", "source")
		},
		edit: func(t *testing.T, repo string, _ func(...string)) {
			writeTaskFile(t, filepath.Join(repo, "source"), "hidden change\n")
		},
		refuse: true,
	}, {
		name: "leading whitespace in untracked path",
		inherit: func(t *testing.T, repo string, _ func(...string)) {
			writeTaskFile(t, filepath.Join(repo, " leading.go"), "package before\n")
		},
		edit: func(t *testing.T, repo string, _ func(...string)) {
			writeTaskFile(t, filepath.Join(repo, " leading.go"), "package after\n")
		},
		refuse: true,
	}, {
		name: "untracked executable mode",
		inherit: func(t *testing.T, repo string, _ func(...string)) {
			writeTaskFile(t, filepath.Join(repo, "script"), "#!/bin/sh\nexit 0\n")
		},
		edit: func(t *testing.T, repo string, _ func(...string)) {
			if err := os.Chmod(filepath.Join(repo, "script"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		refuse: true,
	}, {
		name: "trailing whitespace in tracked content",
		inherit: func(t *testing.T, repo string, _ func(...string)) {
			writeTaskFile(t, filepath.Join(repo, "source"), "changed\n")
		},
		edit: func(t *testing.T, repo string, _ func(...string)) {
			writeTaskFile(t, filepath.Join(repo, "source"), "changed \n")
		},
		refuse: true,
	}, {
		name: "inherited work left alone",
		inherit: func(t *testing.T, repo string, git func(...string)) {
			writeTaskFile(t, filepath.Join(repo, "source"), "inherited edit\n")
			writeTaskFile(t, filepath.Join(repo, "scratch.go"), "package scratch\n")
			git("add", "source")
		},
		edit:   func(*testing.T, string, func(...string)) {},
		refuse: false,
	}} {
		t.Run(test.name, func(t *testing.T) {
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, "source"), "original\n")
			git("add", "source")
			git("commit", "-m", "base")
			head := gitOut(repo, "rev-parse", "HEAD")

			test.inherit(t, repo, git)
			baseline, err := CheckoutFingerprint(repo)
			if err != nil {
				t.Fatal(err)
			}
			before := gitOut(repo, "status", "--porcelain", "--untracked-files=all")

			test.edit(t, repo, git)
			if after := gitOut(repo, "status", "--porcelain", "--untracked-files=all"); after != before {
				t.Fatalf("the fixture changed the status listing, so it does not test what it claims:\n"+
					"before: %q\nafter:  %q", before, after)
			}

			err = NoChangeCompletionAllowed(repo, head, head, "task", baseline)
			if test.refuse && err == nil {
				t.Fatal("a same-status content edit was accepted as a no-change completion")
			}
			if !test.refuse && err != nil {
				t.Fatalf("untouched inherited work was refused: %v", err)
			}
		})
	}
}

func TestCheckoutFingerprintIndexAndInheritedState(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	for _, scenario := range []string{"intent to add", "hidden flags", "ignored output", "tracked ignored file", "ancestor symlink"} {
		t.Run(scenario, func(t *testing.T) {
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, "dir", "source"), "original\n")
			writeTaskFile(t, filepath.Join(repo, ".gitignore"), "output/\n")
			git("add", ".")
			git("commit", "-qm", "base")
			outside := t.TempDir()
			switch scenario {
			case "intent to add":
				writeTaskFile(t, filepath.Join(repo, "empty"), "")
				git("add", "-N", "empty")
			case "hidden flags":
				git("update-index", "--assume-unchanged", ".gitignore")
				git("update-index", "--skip-worktree", "dir/source")
			case "ignored output", "tracked ignored file":
				writeTaskFile(t, filepath.Join(repo, "output", "file"), "before\n")
				if scenario == "tracked ignored file" {
					git("add", "-f", "output/file")
				}
			case "ancestor symlink":
				if err := os.Rename(filepath.Join(repo, "dir"), filepath.Join(repo, "moved")); err != nil {
					t.Fatal(err)
				}
				writeTaskFile(t, filepath.Join(outside, "source"), "outside\n")
				if err := os.Symlink(outside, filepath.Join(repo, "dir")); err != nil {
					t.Fatal(err)
				}
			}
			before, err := CheckoutFingerprint(repo)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "intent to add":
				git("add", "empty")
			case "hidden flags":
				writeTaskFile(t, filepath.Join(repo, "dir", "source"), "changed behind skip-worktree\n")
			case "ignored output", "tracked ignored file":
				writeTaskFile(t, filepath.Join(repo, "output", "file"), "changed\n")
			case "ancestor symlink":
				writeTaskFile(t, filepath.Join(outside, "source"), "changed outside\n")
			}
			after, err := CheckoutFingerprint(repo)
			if err != nil {
				t.Fatal(err)
			}
			wantChange := scenario == "intent to add" || scenario == "hidden flags" || scenario == "tracked ignored file"
			if (before != after) != wantChange {
				t.Fatalf("fingerprint changed = %v, want %v", before != after, wantChange)
			}
		})
	}
}

func TestCheckoutFingerprintNestedRepositories(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	for _, scenario := range []string{"unborn untracked", "gitlink content", "gitlink index", "gitlink HEAD", "empty gitlink", "populated without metadata"} {
		t.Run(scenario, func(t *testing.T) {
			repo, git := gitrepo.New(t)
			git("commit", "--allow-empty", "-qm", "base")
			git("init", "-q", "nested")
			git("-C", "nested", "config", "user.name", "Test")
			git("-C", "nested", "config", "user.email", "test@example.invalid")
			if scenario != "unborn untracked" {
				writeTaskFile(t, filepath.Join(repo, "nested", "source"), "base\n")
				git("-C", "nested", "add", "source")
				git("-C", "nested", "commit", "-qm", "child")
				git("add", "nested")
				git("commit", "-qm", "gitlink")
			}
			if scenario == "empty gitlink" || scenario == "populated without metadata" {
				// Preserve the fixture repository elsewhere instead of destructive cleanup.
				if err := os.Rename(filepath.Join(repo, "nested"), filepath.Join(t.TempDir(), "saved")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(repo, "nested"), 0o755); err != nil {
					t.Fatal(err)
				}
				if scenario == "populated without metadata" {
					writeTaskFile(t, filepath.Join(repo, "nested", "source"), "unexamined\n")
					if _, err := CheckoutFingerprint(repo); err == nil {
						t.Fatal("populated gitlink without metadata was treated as unchanged")
					}
					return
				}
			}
			if scenario == "gitlink content" || scenario == "gitlink index" {
				writeTaskFile(t, filepath.Join(repo, "nested", "source"), "inherited\n")
			}
			before, err := CheckoutFingerprint(repo)
			if err != nil {
				t.Fatal(err)
			}
			unchanged, err := CheckoutFingerprint(repo)
			if err != nil || unchanged != before {
				t.Fatalf("unchanged nested checkout refused: %v", err)
			}
			switch scenario {
			case "unborn untracked", "gitlink content":
				writeTaskFile(t, filepath.Join(repo, "nested", "source"), "new bytes\n")
			case "gitlink index":
				git("-C", "nested", "add", "source")
			case "gitlink HEAD":
				git("-C", "nested", "commit", "--allow-empty", "-qm", "next")
			}
			after, err := CheckoutFingerprint(repo)
			if err != nil {
				t.Fatal(err)
			}
			if (before != after) != (scenario != "empty gitlink") {
				t.Fatalf("unexpected nested checkout comparison: changed=%v", before != after)
			}
		})
	}
}

func TestCheckoutFingerprintRefusesFileReplacement(t *testing.T) {
	for _, scenario := range []string{"symlink", "fifo", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "source")
			writeTaskFile(t, path, "before\n")
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			before, err := root.Lstat("source")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "symlink":
				outside := filepath.Join(t.TempDir(), "outside")
				writeTaskFile(t, outside, "do not read\n")
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := checkoutEntryFingerprint(root, "source", before); err == nil {
				t.Fatal("file replacement during snapshot was accepted")
			}
		})
	}
}

func TestCheckoutFingerprintDoesNotRunSubmoduleDrivers(t *testing.T) {
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
	git("-C", path, "checkout", "-q", second)
	marker := filepath.Join(t.TempDir(), "host-execution")
	t.Setenv("COOP_TEST_SUBMODULE_MARKER", marker)
	driver := filepath.Join(t.TempDir(), "external-diff.sh")
	writeTaskFile(t, driver, "#!/bin/sh\nprintf invoked > \"$COOP_TEST_SUBMODULE_MARKER\"\ncat \"$1\"\n")
	if err := os.Chmod(driver, 0o700); err != nil {
		t.Fatal(err)
	}
	git("-C", path, "config", "diff.audit.command", driver)
	if _, err := CheckoutFingerprint(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("fingerprint ran an untrusted submodule driver on the host: %v", err)
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
	}
	// Prove the driver is reachable under the supported inline-submodule preference.
	args := append(append([]string{"-C", repo}, forkspace.GitHardening...),
		"diff", "--submodule=diff", "--no-ext-diff", "--no-textconv")
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("inline-diff control: %v: %s", err, out)
	}
	if content, err := os.ReadFile(marker); err != nil || string(content) != "invoked" {
		t.Fatalf("inline-diff control did not run the driver: %q, %v", content, err)
	}
}

// An untracked symlink is fingerprinted by where it points, never by what it points at: following
// it would read outside the repository, and an unrelated change out there would then look like a
// change inside the task.
func TestCheckoutFingerprintDoesNotFollowUntrackedSymlinks(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	writeTaskFile(t, filepath.Join(repo, "source"), "original\n")
	git("add", "source")
	git("commit", "-m", "base")

	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("host secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	before, err := CheckoutFingerprint(repo)
	if err != nil {
		t.Fatal(err)
	}
	// The host file changes; the checkout does not.
	if err := os.WriteFile(outside, []byte("host secret, edited elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := CheckoutFingerprint(repo)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("the fingerprint read through a symlink, so work outside the repository counts as a change inside it")
	}
	// Repointing the link IS a change in the checkout.
	if err := os.Remove(filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repo, "source"), filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	repointed, err := CheckoutFingerprint(repo)
	if err != nil {
		t.Fatal(err)
	}
	if repointed == after {
		t.Fatal("repointing an untracked symlink left the fingerprint unchanged")
	}
}
