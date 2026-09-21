package tasks

import (
	"os"
	"path/filepath"
	"testing"

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
