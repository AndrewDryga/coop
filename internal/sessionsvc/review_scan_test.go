package sessionsvc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
