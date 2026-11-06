package forkctl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ui"
)

// parentFingerprint is what a refused merge must leave exactly as it was: the branch tip, the files
// Git sees, and the index.
func parentFingerprint(t *testing.T, repo string) string {
	t.Helper()
	head, err := observed(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	status, err := observed(repo, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	return head + "\n" + status + "\n" + string(index)
}

// Every refusal of an isolated merge is one block: what is refused and why in plain words, then what
// to do. A refusal of the fork's own commits also says --force cannot change it. None shows a raw Git
// exit status or the words the code uses inside (land, custody, uninspectable blob).
func TestIsolatedRefusalsAreOneBlockThatNamesTheCauseAndTheAction(t *testing.T) {
	const forceLine = "--force cannot bypass this check."
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, repo, ws string)
		policy bool     // a refusal of the fork's commits, which --force never changes
		want   []string // each must be in the block
	}{
		{"changed submodule file", func(t *testing.T, _, ws string) {
			isolatedTestCommit(t, ws, ".gitmodules", "[submodule \"x\"]\n\tpath = x\n\turl = https://example.com/x\n")
		}, true, []string{`The fork changes ".gitmodules", which Coop will not merge`, "submodules or nested Git repositories", "your own checkout"}},
		{"git hook", func(t *testing.T, _, ws string) {
			isolatedTestCommit(t, ws, ".githooks/pre-commit", "#!/bin/sh\n")
		}, true, []string{`The fork changes ".githooks/pre-commit", which Coop will not merge`, "Runs during Git operations.", "your own checkout"}},
		{"editor task", func(t *testing.T, _, ws string) {
			isolatedTestCommit(t, ws, ".vscode/tasks.json", "{}\n")
		}, true, []string{`The fork changes ".vscode/tasks.json", which Coop will not merge`, "Can run commands in your VS Code session.", "your own checkout"}},
		{"install script", func(t *testing.T, _, ws string) {
			isolatedTestCommit(t, ws, "package.json", `{"scripts":{"postinstall":"echo planted"}}`)
		}, true, []string{`The fork adds or changes the postinstall script in "package.json"`, "npm runs it by itself", "your own checkout"}},
		{"unreadable package file", func(t *testing.T, _, ws string) {
			isolatedTestCommit(t, ws, "package.json", `{"scripts":`)
		}, true, []string{`Coop cannot read the scripts in "package.json"`, "not valid JSON", "Make it a regular, valid package.json"}},
		{"credential", func(t *testing.T, _, ws string) {
			isolatedTestCommit(t, ws, "ordinary.txt", "github_token = ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij\n")
		}, true, []string{`The fork adds what looks like a credential to "ordinary.txt"`, "Deleting it in a later commit does not help", "Rewrite the fork's commits so none adds it, then merge again."}},
		{"secret file name", func(t *testing.T, _, ws string) {
			isolatedTestCommit(t, ws, "credentials.json", "{}\n")
		}, true, []string{`The fork adds "credentials.json", a file Coop hides from boxes`, "Remove it from the fork's commits."}},
		{"large file", func(t *testing.T, _, ws string) {
			isolatedTestCommit(t, ws, "big.bin", strings.Repeat("x", (5<<20)+1))
		}, true, []string{`The fork adds "big.bin", a file over 5 MiB`, "5 MiB is the most it reads", "Keep it out of the fork's commits"}},
		{"project on a detached head", func(t *testing.T, repo, ws string) {
			isolatedTestCommit(t, ws, "new.txt", "new\n")
			git(t, repo, "checkout", "-q", "--detach")
		}, false, []string{"Your project is not on a branch", "HEAD is detached", "Check out that branch (git switch <branch>), then merge again."}},
		{"leftover index lock", func(t *testing.T, repo, ws string) {
			isolatedTestCommit(t, ws, "new.txt", "new\n")
			writeTaskFile(t, filepath.Join(repo, ".git", "index.lock"), "")
		}, false, []string{"Git has a lock file in your project", "whether a Git command is still changing your project",
			"If none is, the lock is left over and safe to remove.", "Check Git:", "pgrep -lx git", "Remove it:", "index.lock"}},
		{"file in the way", func(t *testing.T, repo, ws string) {
			isolatedTestCommit(t, ws, "base.txt", "from the fork\n")
			writeTaskFile(t, filepath.Join(repo, "base.txt"), "untracked\n")
		}, false, []string{"A file in your project is in the way", `"base.txt" already exists in your project and Git does not track it`, "Move or delete it, then merge again."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, ws, identity, c := isolatedTestFork(t)
			tc.setup(t, repo, ws)
			before := parentFingerprint(t, repo)

			result, err := c.mergeOne(repo, "", identity.Name, false)
			var block *ui.UsageError
			if err == nil || result.landed || !errors.As(err, &block) {
				t.Fatalf("the merge was not refused with a block: %+v, %v", result, err)
			}
			if block.ExitCode != 1 || len(block.Rows) == 0 {
				t.Errorf("the refusal exits %d with %d action rows, want exit 1 and an action", block.ExitCode, len(block.Rows))
			}
			plain := block.Render(ui.Palette{})
			for _, want := range tc.want {
				if !strings.Contains(plain, want) {
					t.Errorf("the refusal does not say %q:\n%s", want, plain)
				}
			}
			if got := strings.Contains(plain, forceLine); got != tc.policy {
				t.Errorf("the refusal says %q = %v, want %v:\n%s", forceLine, got, tc.policy, plain)
			}
			for _, inner := range []string{"exit status", "isolated land", "isolated publication", "custody", "uninspectable", "nested Git sources", "before review"} {
				if strings.Contains(plain, inner) {
					t.Errorf("the refusal shows %q, a word from inside the code:\n%s", inner, plain)
				}
			}
			if strings.Count(plain, "✗") != 1 {
				t.Errorf("the refusal is not one block:\n%s", plain)
			}
			if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
				t.Errorf("the refusal left a journal: %v, %v", pending, err)
			}
			if after := parentFingerprint(t, repo); after != before {
				t.Errorf("the refusal changed the parent")
			}
		})
	}
}

// A lock the parent has after a publication was interrupted keeps its journal, and the block says so,
// since the person must not reset the project.
func TestIsolatedLockRefusalAfterAnInterruptedPublicationSaysTheJournalIsKept(t *testing.T) {
	repo, ws, identity, c := isolatedTestFork(t)
	isolatedTestCommit(t, ws, "new.txt", "candidate\n")
	candidate, err := prepareIsolatedCandidate(repo, identity.Name, identity, false)
	if err != nil {
		t.Fatal(err)
	}
	intent := isolatedLandIntent{Version: 2, Fork: identity, SourceHead: candidate.sourceHead,
		SourceTree: candidate.sourceTree, Upstream: candidate.upstream, Parent: candidate.parent,
		PublicationHead: candidate.head, PublicationTree: candidate.tree,
		Custody: filepath.Base(candidate.dir), Phase: "started", CreatedAt: time.Now().UTC()}
	if err := writeIsolatedRecord(repo, landIntentPath(repo, identity), intent); err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(repo, ".git", "index.lock"), "left by the interrupted publication\n")

	_, err = c.mergeOne(repo, "", identity.Name, false)
	var block *ui.UsageError
	if !errors.As(err, &block) {
		t.Fatalf("the finish of an interrupted publication under a lock = %v, want a block", err)
	}
	plain := block.Render(ui.Palette{})
	for _, want := range []string{"Git has a lock file in your project", "Coop kept the journal of the interrupted merge. Do not reset your project."} {
		if !strings.Contains(plain, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, plain)
		}
	}
	if pending, err := ForkHasPendingLand(repo, identity); err != nil || !pending {
		t.Fatalf("the refusal dropped the journal: %v, %v", pending, err)
	}
}

// The ordinary fork's policy refusal is the same block, and --all keeps its pieces.
func TestOrdinaryForkPolicyRefusalIsOneBlockWithItsActions(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "risky")
	if err != nil {
		t.Fatal(err)
	}
	isolatedTestCommit(t, ws, ".envrc", "export X=1\n")
	c := &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}

	code, err := c.ForkMerge([]string{"risky", "--yes"})
	var block *ui.UsageError
	if code != 1 || !errors.As(err, &block) {
		t.Fatalf("merge of a fork that changes .envrc = (%d, %v), want exit 1 and a block", code, err)
	}
	plain := block.Render(ui.Palette{})
	for _, want := range []string{"Fork risky changes files Coop flags as risky", ".envrc — Runs when you enter the folder with direnv.",
		"Review it:", "coop fork review risky", "Merge anyway:", "coop fork merge risky --force"} {
		if !strings.Contains(plain, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "policy flagged") || strings.Contains(plain, "(use --force") {
		t.Errorf("the refusal kept the old wording:\n%s", plain)
	}
	if !pathExists(ws) {
		t.Fatal("the refused merge removed the fork")
	}

	stderr := captureStderr(t, func() { code, err = c.ForkMerge([]string{"--all", "--yes"}) })
	if code != 1 || err == nil {
		t.Fatalf("merge --all over that fork = (%d, %v), want exit 1", code, err)
	}
	for _, want := range []string{"Fork risky changes files Coop flags as risky", ".envrc — Runs when you enter the folder with direnv.",
		"Merged: nothing.", "Review it:", "Merge anyway:"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("merge --all does not say %q:\n%s", want, stderr)
		}
	}
}

// A review that keeps no copy still says where the copy it used was, in the fork records.
func TestIsolatedPlainReviewSaysWhereItsCopyIs(t *testing.T) {
	repo, ws, identity, c := isolatedTestFork(t)
	isolatedTestCommit(t, ws, "kept.txt", "kept\n")
	notes := captureStderr(t, func() {
		captureStdout(t, func() {
			if code, err := c.forkReviewIsolated(repo, ws, identity.Name, identity, true, false, false, false); code != 0 || err != nil {
				t.Fatalf("review --stat: %d, %v", code, err)
			}
		})
	})
	want := "The review uses a private copy of the committed files in " + forkspace.StateDir(repo) + string(filepath.Separator) +
		forkspace.IsolatedRecordPrefix("candidate", identity)
	if !strings.Contains(notes, want) {
		t.Errorf("review did not say where its copy was, want %q in:\n%s", want, notes)
	}
	if left := isolatedRecords(t, repo); len(left) != 0 {
		t.Errorf("a review with no --open kept its copy: %v", left)
	}
}

// `coop fork ls` marks an isolated fork, and its JSON has an isolated field for every fork.
func TestForkLsMarksAnIsolatedFork(t *testing.T) {
	repo, _, identity, c := isolatedTestFork(t)
	if _, err := forkspace.Setup(repo, "ordinary"); err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	_, err = forkspace.EnsureGenerationLocked(repo, "ordinary")
	unlock()
	if err != nil {
		t.Fatal(err)
	}

	text, _, isolated := listForkStatus(t, c, identity.Name)
	_, _, ordinary := listForkStatus(t, c, "ordinary")
	block := func(name string) string {
		_, after, _ := strings.Cut(text, "  "+name+" · ")
		before, _, _ := strings.Cut(after, "\n\n")
		return before
	}
	if !strings.Contains(block(identity.Name), "\n    Isolated: yes") {
		t.Errorf("fork ls does not mark the isolated fork:\n%s", text)
	}
	if strings.Contains(block("ordinary"), "Isolated") || strings.Count(text, "Isolated:") != 1 {
		t.Errorf("fork ls marks an ordinary fork as isolated:\n%s", text)
	}
	if !strings.Contains(isolated, `"isolated":true`) || !strings.Contains(ordinary, `"isolated":false`) {
		t.Errorf("fork ls --json isolated fields: %s and %s", isolated, ordinary)
	}
}
