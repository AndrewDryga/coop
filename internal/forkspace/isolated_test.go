package forkspace

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsolatedHostGitRefusesRedirectBeforeParentViewRecovery(t *testing.T) {
	for _, access := range []string{"operational", "observation", "ref"} {
		t.Run(access, func(t *testing.T) {
			repo, ws, _ := isolatedFixture(t)
			if _, err := viewRun(t, repo, "status", "--porcelain"); err != nil {
				t.Fatal(err)
			}
			view, err := openGitView(t.Context(), repo)
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(filepath.Join(repo, ".git", "HEAD"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(view.dir, "HEAD"), []byte("ref: refs/heads/foreign\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			future := time.Now().Add(time.Hour)
			if err := os.Chtimes(filepath.Join(view.dir, "HEAD"), future, future); err != nil {
				t.Fatal(err)
			}
			metadata := filepath.Join(ws, ".git")
			if err := os.Rename(metadata, metadata+"-saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(metadata, []byte("gitdir: "+filepath.Join(repo, ".git")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch access {
			case "operational":
				_, err = GitCommand(t.Context(), ws, "rev-parse", "HEAD")
			case "observation":
				_, err = ObserveGit(t.Context(), ws, "rev-parse", "HEAD")
			case "ref":
				err = GitRefCommand(t.Context(), ws, "update-ref", "refs/heads/foreign", strings.Repeat("a", 40)).Run()
			}
			if err == nil {
				t.Fatal("host Git admitted redirected isolated metadata")
			}
			after, err := os.ReadFile(filepath.Join(repo, ".git", "HEAD"))
			if err != nil || !bytes.Equal(original, after) {
				t.Fatal("source redirect reconciled parent HEAD", err)
			}
		})
	}
}

func TestIsolatedHostGitRefusesMissingRootMetadata(t *testing.T) {
	repo, ws, _ := isolatedFixture(t)
	if err := os.Rename(filepath.Join(ws, ".git"), filepath.Join(ws, ".git-saved")); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(ws, "nested")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{ws, child} {
		if _, err := GitCommand(t.Context(), path, "rev-parse", "HEAD"); err == nil {
			t.Fatal("missing root metadata admitted host Git", path, repo)
		}
	}
}

func TestIsolatedDestroyPreservesUnownedParentReviewRef(t *testing.T) {
	repo, _, identity := isolatedFixture(t)
	gitIn(t, repo, "branch", "review/"+identity.Name)
	before, err := ObserveGit(t.Context(), repo, "rev-parse", "refs/heads/review/"+identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := Destroy(repo, identity.Name); err != nil {
		t.Fatal(err)
	}
	after, err := ObserveGit(t.Context(), repo, "rev-parse", "refs/heads/review/"+identity.Name)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("isolated destruction deleted an unrelated parent review ref", err)
	}
}

func isolatedFixture(t *testing.T) (string, string, Identity) {
	t.Helper()
	repo := committedSetupRepo(t)
	head, err := gitOutputContext(context.Background(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := SetupPinnedContext(context.Background(), repo, "isolated", head)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := LockState(repo, "isolated")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	identity, err := EnsureIsolatedGenerationLocked(repo, "isolated")
	if err != nil {
		t.Fatal(err)
	}
	return repo, ws, identity
}

func TestIsolatedGenerationPersistsAndRecoversItsBoundary(t *testing.T) {
	repo, _, identity := isolatedFixture(t)
	resumed := ensureTestGeneration(t, repo, identity.Name)
	if resumed != identity {
		t.Fatal("reopen changed identity")
	}
	if isolated, err := IsolatedGeneration(repo, resumed); err != nil || !isolated {
		t.Fatalf("reopen isolation = %v, %v", isolated, err)
	}
	if err := ValidateGenerationWorkspace(repo, identity); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(GenerationPath(repo, identity.Name)); err != nil {
		t.Fatal(err)
	}
	if recovered := ensureTestGeneration(t, repo, identity.Name); recovered != identity {
		t.Fatal("interrupted publication did not recover exact identity")
	}
	if isolated, err := IsolatedGeneration(repo, identity); err != nil || !isolated {
		t.Fatalf("recovery downgraded isolation = %v, %v", isolated, err)
	}
}

func TestIsolatedRecoveryRefusesRewrittenMarkerContract(t *testing.T) {
	for _, attack := range []string{"ordinary-mode", "creation-base"} {
		t.Run(attack, func(t *testing.T) {
			repo, ws, identity := isolatedFixture(t)
			body := forkGenerationAnchorBody(identity.Name, identity.Generation)
			if attack == "creation-base" {
				body = []byte("coop-fork-generation-v4-isolated\n" + identity.Name + "\n" + string(identity.Generation) + "\n" + strings.Repeat("a", 40) + "\n")
			}
			if err := os.WriteFile(filepath.Join(ws, GenerationMarkerName), body, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(GenerationPath(repo, identity.Name)); err != nil {
				t.Fatal(err)
			}
			unlock, err := LockState(repo, identity.Name)
			if err != nil {
				t.Fatal(err)
			}
			_, err = EnsureGenerationLocked(repo, identity.Name)
			unlock()
			if err == nil {
				t.Fatal("writable hardlink bytes selected a different recovery contract")
			}
			if _, present, err := ReadGeneration(repo, identity.Name); err != nil || present {
				t.Fatalf("refusal published authority: %v, %v", present, err)
			}
		})
	}
}

func TestIsolatedGenerationRefusesOrdinaryUpgradeAndRecordDowngrade(t *testing.T) {
	repo := committedSetupRepo(t)
	if _, err := Setup(repo, "ordinary"); err != nil {
		t.Fatal(err)
	}
	ordinary := ensureTestGeneration(t, repo, "ordinary")
	unlock, err := LockState(repo, ordinary.Name)
	if err != nil {
		t.Fatal(err)
	}
	_, err = EnsureIsolatedGenerationLocked(repo, ordinary.Name)
	unlock()
	if err == nil || !strings.Contains(err.Error(), "existing work is unchanged") {
		t.Fatalf("ordinary upgrade = %v", err)
	}
	repo, _, identity := isolatedFixture(t)
	record, err := readGenerationRecord(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	record.Version, record.Isolated = forkGenerationVersion, false
	record.CreationBase = ""
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(GenerationPath(repo, identity.Name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGenerationWorkspace(repo, identity); err == nil {
		t.Fatal("downgraded record accepted the isolated hardlink anchor")
	}
}

func TestIndependentGitRefusesRedirectorsAndSharedObjects(t *testing.T) {
	for _, attack := range []string{"commondir", "alternate", "symlink", "hardlink", "gitfile"} {
		t.Run(attack, func(t *testing.T) {
			repo, ws, _ := isolatedFixture(t)
			metadata := filepath.Join(ws, ".git")
			var err error
			switch attack {
			case "commondir":
				err = os.WriteFile(filepath.Join(metadata, "commondir"), []byte(filepath.Join(repo, ".git")), 0o600)
			case "alternate":
				err = os.MkdirAll(filepath.Join(metadata, "objects", "info"), 0o755)
				if err == nil {
					err = os.WriteFile(filepath.Join(metadata, "objects", "info", "alternates"), []byte(filepath.Join(repo, ".git", "objects")), 0o600)
				}
			case "symlink":
				err = os.Symlink(filepath.Join(repo, ".git", "objects"), filepath.Join(metadata, "objects", "redirect"))
			case "hardlink":
				err = os.Link(filepath.Join(repo, ".git", "HEAD"), filepath.Join(metadata, "foreign"))
			case "gitfile":
				err = os.Rename(metadata, metadata+"-saved")
				if err == nil {
					err = os.WriteFile(metadata, []byte("gitdir: "+filepath.Join(repo, ".git")+"\n"), 0o600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateIndependentGit(ws); err == nil {
				t.Fatal("redirect/shared metadata admitted")
			}
		})
	}
}
