package sessionsvc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestReviewScansTheChangedCandidateIncludingLargeLFSPayloads(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "no-global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "no-system"))
	for _, lfs := range []bool{false, true} {
		t.Run(map[bool]string{false: "git-blob", true: "lfs-object"}[lfs], func(t *testing.T) {
			repo, git := gitrepo.New(t)
			if lfs {
				writeSessionLFSSource(t, repo, []byte("original asset\n"))
				git("add", ".")
			}
			git("commit", "--allow-empty", "-qm", "base")
			service := newReviewTestService(t, repo, 1024, ReviewGateFunc(func(context.Context, ReviewGateRequest) (ReviewGateResult, error) {
				return ReviewGateResult{Configured: true, Passed: true}, nil
			}))
			defer service.Stop()
			sess := createReviewSession(t, service, "secret-scan")
			// The literal is beyond the old scanner's blob limit, the UI preview, and the 5 MiB of a
			// blob the scanner keeps in memory.
			payload := append(reviewBinaryPayload(5<<20), []byte("ghp_"+strings.Repeat("Q7r9", 10)+"\n")...)
			if lfs {
				writeSessionLFSSource(t, sess.Workspace, payload)
			} else if err := os.WriteFile(filepath.Join(sess.Workspace, "ordinary file\tname.bin"), payload, 0600); err != nil {
				t.Fatal(err)
			}
			sessionWorkspaceGit(t, sess.Workspace, "add", ".")
			sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "candidate")
			dossier, err := service.RunReview(context.Background(), "scan-review", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
			if err != nil {
				t.Fatal(err)
			}
			if dossier.Publishable || len(dossier.PolicyFindings) == 0 || !strings.Contains(strings.Join(dossier.PolicyFindings, " "), "secret") {
				t.Fatalf("candidate credential was not refused: publishable=%v findings=%v", dossier.Publishable, dossier.PolicyFindings)
			}
		})
	}
}

// reviewBinaryPayload is binary data just past size bytes, in 4 KiB lines. One unbounded line takes
// the secret scanner's slow streaming engine, which internal/secretscan tests on its own; under the
// race detector it cost each review test here a quarter of a minute.
func reviewBinaryPayload(size int) []byte {
	line := append(bytes.Repeat([]byte{0, 1, 2, 3}, 1023), '\n')
	return bytes.Repeat(line, size/len(line)+1)
}

func TestWorkflowPublicationStillScansCredentialsAndLocalExecution(t *testing.T) {
	for _, tc := range []struct {
		name, path, contents, want string
	}{
		{"credential", ".github/workflows/ci.yml", "# ghp_" + strings.Repeat("Q7r9", 10), "possible secret in .github/workflows/ci.yml"},
		{"large-credential", ".github/workflows/ci.yml", string(reviewBinaryPayload(5<<20)) + "ghp_" + strings.Repeat("Q7r9", 10), "possible secret in .github/workflows/ci.yml"},
		{"host-hook", ".envrc", "echo host", "direnv"},
		{"nested-host-hook", ".github/workflows/.envrc", "echo host", "direnv"},
		{"hidden-policy", ".coopignore", "private/", "edits a .coopignore"},
		{"lifecycle", ".github/workflows/package.json", `{"scripts":{"postinstall":"node install.js"}}`, "adds a postinstall script"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, git := gitrepo.New(t)
			git("commit", "--allow-empty", "-qm", "base")
			parent := gitOut(repo, "rev-parse", "HEAD")
			for path, contents := range map[string]string{".github/workflows/replay.yml": "name: replay\n", tc.path: tc.contents} {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, path)), 0700); err != nil {
					t.Fatal(err)
				}
				sessionWorkspaceWrite(t, filepath.Join(repo, path), contents)
			}
			git("add", ".")
			git("commit", "-qm", "candidate")
			findings, err := scanReviewCandidate(t.Context(), repo, parent, gitOut(repo, "rev-parse", "HEAD"))
			joined := strings.Join(findings, "\n")
			if err != nil || !strings.Contains(joined, tc.want) || strings.Contains(joined, "CI runners") {
				t.Fatalf("findings = %q, %v; want %q without a blanket CI veto", findings, err, tc.want)
			}
		})
	}
}

func TestReviewScanUsesPinnedDeltaAndDoesNotBlockOrdinaryLargeFiles(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	parent := gitOut(repo, "rev-parse", "HEAD")
	for path, data := range map[string][]byte{
		"ordinary large.bin":         reviewBinaryPayload(5 << 20), // past the 5 MiB kept in memory
		".githooks/white space hook": []byte("echo automatic\n"),
		"package.json":               []byte(`{"scripts":{"postinstall":"node install.js"}}`),
		".env":                       []byte("ordinary data\n"),
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, path)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, path), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-qm", "candidate")
	candidate := gitOut(repo, "rev-parse", "HEAD")
	findings, err := scanReviewCandidate(context.Background(), repo, parent, candidate)
	if err != nil || len(findings) != 3 || strings.Contains(strings.Join(findings, " "), "ordinary large.bin") {
		t.Fatalf("wrong pinned delta findings: %v, %v", findings, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scanReviewCandidate(ctx, repo, parent, candidate); err == nil {
		t.Fatal("canceled scan was treated as safe")
	}
	if _, err := scanReviewCandidate(context.Background(), repo, strings.Repeat("f", 40), candidate); err == nil {
		t.Fatal("missing parent was treated as safe")
	}
}

// A candidate whose .coopignore coop cannot read leaves unknown which of its files are hidden, so
// the review says so once, naming it, which keeps the candidate from publishing; it does not call
// the files secret.
func TestReviewScanNamesAnUnreadableCoopignore(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	parent := gitOut(repo, "rev-parse", "HEAD")
	if err := os.Mkdir(filepath.Join(repo, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"config/a.go", "config/b.go"} {
		sessionWorkspaceWrite(t, filepath.Join(repo, path), "package config\n")
	}
	if err := os.Symlink("rules", filepath.Join(repo, ".coopignore")); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "candidate")
	findings, err := scanReviewCandidate(context.Background(), repo, parent, gitOut(repo, "rev-parse", "HEAD"))
	if want := "coop cannot tell what .coopignore hides, because it is a link"; err != nil || len(findings) != 1 || findings[0] != want {
		t.Fatalf("findings = %q, %v; want only %q", findings, err, want)
	}
}

// The review names a change to a .coopignore, which decides what every later box is shown, whether
// it drops a rule or adds one.
func TestReviewScanNamesAChangedCoopignore(t *testing.T) {
	for _, rules := range []string{"notes/\n", "secrets/\nnotes/\nmore/\n"} {
		repo, git := gitrepo.New(t)
		sessionWorkspaceWrite(t, filepath.Join(repo, ".coopignore"), "secrets/\nnotes/\n")
		git("add", ".")
		git("commit", "-qm", "base")
		parent := gitOut(repo, "rev-parse", "HEAD")
		sessionWorkspaceWrite(t, filepath.Join(repo, ".coopignore"), rules)
		git("commit", "-qam", "candidate")
		findings, err := scanReviewCandidate(context.Background(), repo, parent, gitOut(repo, "rev-parse", "HEAD"))
		if want := ".coopignore — the change edits a .coopignore, which decides what boxes are shown"; err != nil || !slices.Contains(findings, want) {
			t.Fatalf("findings = %q, %v; want %q", findings, err, want)
		}
	}
}

// A .COOPIGNORE is the folder's rules on a case-insensitive Mac disk, so the review names it too.
func TestReviewScanNamesACoopignoreInAnotherCase(t *testing.T) {
	repo, git := gitrepo.New(t)
	sessionWorkspaceWrite(t, filepath.Join(repo, "README.md"), "base\n")
	git("add", ".")
	git("commit", "-qm", "base")
	parent := gitOut(repo, "rev-parse", "HEAD")
	if err := os.Mkdir(filepath.Join(repo, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(repo, "web", ".COOPIGNORE"), "build/\n")
	git("add", ".")
	git("commit", "-qm", "candidate")
	findings, err := scanReviewCandidate(context.Background(), repo, parent, gitOut(repo, "rev-parse", "HEAD"))
	if want := "web/.COOPIGNORE — the change edits a .coopignore, which decides what boxes are shown"; err != nil || !slices.Contains(findings, want) {
		t.Fatalf("findings = %q, %v; want %q", findings, err, want)
	}
}

// A file the readable rules hide is still named when another .coopignore above it cannot be read:
// a rule in the root .coopignore, its own name or a folder's name above it. So the review never
// reports only the .coopignore for a candidate that adds a db.prod.json or a .env; the file the
// readable rules leave visible gets the one finding that names the unreadable .coopignore.
func TestReviewScanNamesASecretUnderAnUnreadableCoopignore(t *testing.T) {
	repo, git := gitrepo.New(t)
	sessionWorkspaceWrite(t, filepath.Join(repo, ".coopignore"), "*.prod.json\n")
	git("add", ".coopignore")
	git("commit", "-qm", "base")
	parent := gitOut(repo, "rev-parse", "HEAD")
	// A secret folder hides what it holds whatever the file is called, so those files count too.
	secrets := []string{"config/sub/.aws/config", "config/sub/.env", "config/sub/db.prod.json", "config/sub/secrets/token.txt"}
	for _, path := range secrets {
		if err := os.MkdirAll(filepath.Join(repo, filepath.Dir(filepath.FromSlash(path))), 0700); err != nil {
			t.Fatal(err)
		}
		sessionWorkspaceWrite(t, filepath.Join(repo, filepath.FromSlash(path)), "TOKEN=x\n")
	}
	sessionWorkspaceWrite(t, filepath.Join(repo, "config", "sub", "app.yml"), "name: app\n")
	if err := os.Symlink("rules", filepath.Join(repo, "config", ".coopignore")); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "candidate")
	findings, err := scanReviewCandidate(context.Background(), repo, parent, gitOut(repo, "rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	var named []string
	unreadable := 0
	for _, finding := range findings {
		if path, ok := strings.CutPrefix(finding, "secret-like file: "); ok {
			named = append(named, path)
		}
		if finding == "coop cannot tell what config/.coopignore hides, because it is a link" {
			unreadable++
		}
	}
	slices.Sort(named)
	if !slices.Equal(named, secrets) || unreadable != 1 {
		t.Errorf("findings = %q; want %q named and one finding for config/.coopignore", findings, secrets)
	}
}

func TestReviewScanChecksLFSActivatedByRemovingAnAttributeOverride(t *testing.T) {
	repo, git := gitrepo.New(t)
	writeSessionLFSSource(t, repo, []byte("ghp_"+strings.Repeat("Q7r9", 10)+"\n"))
	if err := os.Mkdir(filepath.Join(repo, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(repo, "asset.bin"), filepath.Join(repo, "nested/asset.bin")); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(repo, "nested/.gitattributes"), "*.bin -filter\n")
	sessionWorkspaceWrite(t, filepath.Join(repo, "package.json"), `{}`)
	git("add", ".")
	git("commit", "-qm", "ordinary pointer text")
	parent := gitOut(repo, "rev-parse", "HEAD")
	git("rm", "nested/.gitattributes")
	sessionWorkspaceWrite(t, filepath.Join(repo, "package.json"), "")
	git("add", ".")
	git("commit", "-qm", "activate LFS without editing its pointer")
	findings, err := scanReviewCandidate(context.Background(), repo, parent, gitOut(repo, "rev-parse", "HEAD"))
	if err != nil || len(findings) != 1 || !strings.Contains(findings[0], "possible secret in nested/asset.bin") {
		t.Fatalf("newly activated payload escaped scan, or empty JSON was falsely flagged: %v, %v", findings, err)
	}
}

// Another spelling of .gitattributes is the file Git reads once a Mac checks the candidate out, so
// removing an override spelled .GITATTRIBUTES rescans what it may have switched on, as removing
// .gitattributes does; another spelling of a name a .coopignore hides, or of a hook, is named too.
func TestReviewScanJudgesEverySpelling(t *testing.T) {
	repo, git := gitrepo.New(t)
	writeSessionLFSSource(t, repo, []byte("ghp_"+strings.Repeat("Q7r9", 10)+"\n"))
	if err := os.Mkdir(filepath.Join(repo, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(repo, "asset.bin"), filepath.Join(repo, "nested/asset.bin")); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(repo, "nested/.GITATTRIBUTES"), "*.bin -filter\n")
	sessionWorkspaceWrite(t, filepath.Join(repo, ".coopignore"), "private.yml\n")
	git("add", ".")
	git("commit", "-qm", "an override in another spelling")
	parent := gitOut(repo, "rev-parse", "HEAD")
	git("rm", "-q", "nested/.GITATTRIBUTES")
	if err := os.Mkdir(filepath.Join(repo, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(repo, "config/PRIVATE.yml"), "password: inert\n")
	if err := os.Mkdir(filepath.Join(repo, ".GITHOOKS"), 0700); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(repo, ".GITHOOKS/pre-commit"), "#!/bin/sh\n")
	git("add", ".")
	git("commit", "-qm", "candidate")
	findings, err := scanReviewCandidate(context.Background(), repo, parent, gitOut(repo, "rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"possible secret in nested/asset.bin", "secret-like file: config/PRIVATE.yml", ".GITHOOKS/pre-commit — Runs during Git operations."} {
		if !slices.ContainsFunc(findings, func(finding string) bool { return strings.Contains(finding, want) }) {
			t.Errorf("findings = %q; want one naming %q", findings, want)
		}
	}
}
