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
			// The literal is beyond both the old scanner's blob limit and the UI preview.
			payload := append(bytes.Repeat([]byte{0, 1, 2, 3}, 2<<20), []byte("\nghp_"+strings.Repeat("Q7r9", 10)+"\n")...)
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

func TestReviewScanUsesPinnedDeltaAndDoesNotBlockOrdinaryLargeFiles(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	parent := gitOut(repo, "rev-parse", "HEAD")
	for path, data := range map[string][]byte{
		"ordinary large.bin":         bytes.Repeat([]byte{0, 1, 2, 3}, 2<<20),
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
