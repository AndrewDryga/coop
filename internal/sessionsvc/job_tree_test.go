package sessionsvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func nestedSessionSource(t *testing.T, lfsPayload ...[]byte) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	leaf, git := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(leaf, "code"), []byte("complete nested code\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "code")
	if len(lfsPayload) != 0 {
		writeSessionLFSSource(t, leaf, lfsPayload[0])
		git("add", ".gitattributes", "asset.bin")
	}
	git("commit", "-qm", "leaf")
	for _, path := range []string{"nested space", "vendor/child"} {
		parent, parentGit := gitrepo.New(t)
		parentGit("-c", "protocol.file.allow=always", "submodule", "add", "--quiet", leaf, path)
		if len(lfsPayload) != 0 {
			writeSessionLFSSource(t, parent, lfsPayload[0])
			parentGit("add", ".gitattributes", "asset.bin")
		}
		parentGit("commit", "-qm", "parent")
		// Stage fixtures with independent child metadata, as the trusted worker does.
		if err := os.RemoveAll(filepath.Join(parent, path)); err != nil {
			t.Fatal(err)
		}
		parentGit("clone", "--quiet", "--no-local", leaf, path)
		if err := forkspace.HydrateLFS(context.Background(), filepath.Join(parent, path), gitOut(leaf, "rev-parse", "HEAD"), leaf); err != nil {
			t.Fatal(err)
		}
		if path == "vendor/child" {
			copyTestJobSubmodules(t, leaf, filepath.Join(parent, path), testJobSubmodules(t, leaf, gitOut(leaf, "rev-parse", "HEAD")))
		}
		leaf = parent
	}
	return leaf
}

func writeSessionLFSSource(t *testing.T, repository string, payload []byte) {
	t.Helper()
	oid := fmt.Sprintf("%x", sha256.Sum256(payload))
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, len(payload))
	sessionWorkspaceWrite(t, filepath.Join(repository, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	sessionWorkspaceWrite(t, filepath.Join(repository, "asset.bin"), pointer)
	object := filepath.Join(repository, ".git", "lfs", "objects", oid[:2], oid[2:4], oid)
	if err := os.MkdirAll(filepath.Dir(object), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, payload, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionLFSWorkSurvivesPrimaryCompanionAndReviewMaterialization(t *testing.T) {
	payload := bytes.Repeat([]byte("original\x00large-content\n"), 200_000)
	repository := nestedSessionSource(t, payload)
	revised := append(bytes.Clone(payload), []byte("model edit\n")...)
	gateCalls := 0
	fixture := newReviewTestService(t, repository, 1<<20, ReviewGateFunc(func(_ context.Context, _, workspace string) (ReviewGateResult, error) {
		gateCalls++
		for path, want := range map[string][]byte{"asset.bin": revised, "vendor/child/nested space/asset.bin": payload} {
			got, err := os.ReadFile(filepath.Join(workspace, path))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("review did not receive full LFS content at %s: %d bytes, %v", path, len(got), err)
			}
		}
		return ReviewGateResult{Configured: true, Passed: true}, nil
	}))
	defer fixture.Stop()
	fixture.companion(t, "reference", repository)
	created := createReviewSession(t, fixture, "lfs-workspaces")
	changes, err := fixture.GetChanges(context.Background(), created.ID)
	if err != nil || changes.PatchBytes != 0 {
		t.Fatalf("unchanged hydrated files appeared in the patch: %d bytes, %v", changes.PatchBytes, err)
	}
	for _, workspace := range []string{created.Workspace, created.Companions[0].Workspace} {
		for _, path := range []string{"", "vendor/child", "vendor/child/nested space"} {
			child := filepath.Join(workspace, path)
			got, err := os.ReadFile(filepath.Join(child, "asset.bin"))
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("incomplete LFS checkout %s: %d bytes, %v", child, len(got), err)
			}
			if err := forkspace.VerifyLFS(context.Background(), child, gitOut(child, "rev-parse", "HEAD")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := fixture.CreateRemoteSession(context.Background(), "create-lfs-workspaces", fixture.request(t, "lfs-workspaces")); err != nil {
		t.Fatalf("LFS replay: %v", err)
	}
	// This is the model-side Git operation. Host code must neither need nor trust
	// these local filters after the model has committed the new pointer/object.
	sessionWorkspaceGit(t, created.Workspace, "lfs", "install", "--local", "--skip-repo")
	if err := os.WriteFile(filepath.Join(created.Workspace, "asset.bin"), revised, 0644); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, created.Workspace, "add", "asset.bin")
	index, err := os.ReadFile(filepath.Join(created.Workspace, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	changes, err = fixture.GetChanges(context.Background(), created.ID)
	if err != nil || len(changes.Staged) != 1 || len(changes.Unstaged) != 0 || changes.PatchBytes > 1024 || !strings.Contains(changes.Patch, "size ") {
		t.Fatalf("staged LFS edit lost its pointer identity: %+v, %v", changes, err)
	}
	if after, err := os.ReadFile(filepath.Join(created.Workspace, ".git", "index")); err != nil || !bytes.Equal(index, after) {
		t.Fatal("inspecting an LFS edit rewrote the model's index")
	}
	sessionWorkspaceGit(t, created.Workspace, "commit", "-qm", "update LFS asset")
	marker := filepath.Join(t.TempDir(), "host-filter-ran")
	for _, name := range []string{"clean", "smudge", "process"} {
		sessionWorkspaceGit(t, created.Workspace, "config", "filter.lfs."+name, "touch '"+marker+"'; exit 1")
	}
	dossier, err := fixture.RunReview(context.Background(), "review-lfs-workspaces", RunReviewRequest{
		SessionID: created.ID, ExpectedRevision: created.Revision,
	})
	if err != nil || gateCalls != 1 || dossier.Gate != ReviewGatePassed || containsReviewReason(dossier.NotPublishableReasons, "gate_modified_candidate") {
		t.Fatalf("review changed LFS asset: %+v, %v; calls=%d", dossier, err, gateCalls)
	}
	if pathExists(marker) {
		t.Fatal("host executed the model's LFS filter")
	}
	for _, path := range []string{"", "vendor/child/nested space"} {
		companion := created.Companions[0]
		child := filepath.Join(companion.Workspace, path)
		pointer := sessionWorkspaceGit(t, child, "show", "HEAD:asset.bin") + "\n"
		sessionWorkspaceWrite(t, filepath.Join(child, "asset.bin"), pointer)
		if _, err := ensureSessionCompanion(fixture.stateRoot, created.ID, companion); err != nil {
			t.Fatalf("rehydrate a clean companion: %v", err)
		}
		if actual, err := os.ReadFile(filepath.Join(child, "asset.bin")); err != nil || !bytes.Equal(actual, payload) {
			t.Fatal("adopted an incomplete companion with unexpanded LFS pointers")
		}
	}
}

func TestSessionWorkspacesContainIndependentRecursiveSources(t *testing.T) {
	ctx := context.Background()
	repository := nestedSessionSource(t)
	fixture := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repository, nil)
	defer fixture.Stop()
	fixture.companion(t, "reference", repository)
	created, err := fixture.CreateRemoteSession(ctx, "create-nested", fixture.request(t, "test:nested"))
	if err != nil {
		t.Fatal(err)
	}
	for _, workspace := range []string{created.Workspace, created.Companions[0].Workspace} {
		for _, path := range []string{"vendor/child", "vendor/child/nested space"} {
			child := filepath.Join(workspace, path)
			if info, err := os.Lstat(filepath.Join(child, ".git")); err != nil || !info.IsDir() {
				t.Fatalf("missing independent metadata at %s: %v", child, err)
			}
			config, err := os.ReadFile(filepath.Join(child, ".git", "config"))
			if err != nil || strings.Contains(string(config), repository) || strings.Contains(string(config), "[remote ") {
				t.Fatalf("child leaked source configuration: %q, %v", config, err)
			}
		}
		data, err := os.ReadFile(filepath.Join(workspace, "vendor/child/nested space/code"))
		if err != nil || string(data) != "complete nested code\n" {
			t.Fatalf("incomplete working tree: %q, %v", data, err)
		}
	}
	if _, err := fixture.CreateRemoteSession(ctx, "create-nested", fixture.request(t, "test:nested")); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if _, err := captureSessionReviewSource(created.Repository, created.Workspace, created.ForkName); err != nil {
		t.Fatalf("complete source is reviewable: %v", err)
	}
	plan, err := planSessionWorkspaceDiscard(created.Repository, created.Workspace, false, false)
	if err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(created.Workspace, "vendor/child/nested space/code")
	if err := os.WriteFile(leaf, []byte("uncommitted nested work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureSessionReviewSource(created.Repository, created.Workspace, created.ForkName); err == nil {
		t.Fatal("review silently omitted nested work")
	}
	if _, err := fixture.GetChanges(ctx, created.ID); err == nil {
		t.Fatal("inspection silently omitted nested work")
	}
	if _, _, _, err := buildWorkspaceCheckpoint(context.Background(), t.TempDir(), created, CheckpointWorkspaceRequest{}, "nested-checkpoint", time.Now()); err == nil || !strings.Contains(err.Error(), "nested work needs separate custody") {
		t.Fatalf("checkpoint silently omitted nested work: %v", err)
	}
	if _, err := planSessionWorkspaceDiscard(created.Repository, created.Workspace, false, false); err == nil {
		t.Fatal("discard planning silently omitted nested work")
	}
	if err := discardSessionWorkspace(plan); err == nil {
		t.Fatal("stale discard deleted nested work")
	}
	if data, err := os.ReadFile(leaf); err != nil || string(data) != "uncommitted nested work\n" {
		t.Fatalf("nested work was lost: %q, %v", data, err)
	}
}

func TestReviewGateReceivesTheFullRecursiveTreeAndCannotSilentlyChangeIt(t *testing.T) {
	repository := nestedSessionSource(t)
	modifyChild := false
	calls := 0
	fixture := newReviewTestService(t, repository, 1<<20, ReviewGateFunc(func(_ context.Context, _, workspace string) (ReviewGateResult, error) {
		calls++
		leaf := filepath.Join(workspace, "vendor/child/nested space/code")
		if data, err := os.ReadFile(leaf); err != nil || string(data) != "complete nested code\n" {
			t.Fatalf("review gate received incomplete nested source: %q, %v", data, err)
		}
		if modifyChild {
			if err := os.WriteFile(leaf, []byte("gate changed child\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return ReviewGateResult{Configured: true, Passed: true}, nil
	}))
	defer fixture.Stop()
	created := createReviewSession(t, fixture, "nested-review")
	if err := os.WriteFile(filepath.Join(created.Workspace, "change"), []byte("parent change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, created.Workspace, "add", "change")
	sessionWorkspaceGit(t, created.Workspace, "commit", "-qm", "parent change")
	request := RunReviewRequest{SessionID: created.ID, ExpectedRevision: created.Revision}
	dossier, err := fixture.RunReview(context.Background(), "nested-review-clean", request)
	if err != nil || calls != 1 || dossier.Gate != ReviewGatePassed || containsReviewReason(dossier.NotPublishableReasons, "gate_modified_candidate") {
		t.Fatalf("review complete tree: %+v, %v; calls=%d", dossier, err, calls)
	}
	modifyChild = true
	dossier, err = fixture.RunReview(context.Background(), "nested-review-modified", request)
	if err != nil || dossier.Publishable || !containsReviewReason(dossier.NotPublishableReasons, "gate_modified_candidate") {
		t.Fatalf("gate's nested edits were omitted: %+v, %v", dossier, err)
	}
}

func TestCheckpointDoesNotDiscardCommittedNestedHistoryBehindAGitlink(t *testing.T) {
	repository := nestedSessionSource(t)
	fixture := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repository, nil)
	defer fixture.Stop()
	created, err := fixture.CreateRemoteSession(context.Background(), "nested-commits", fixture.request(t, "test:nested-commits"))
	if err != nil {
		t.Fatal(err)
	}
	created, err = fixture.EnsureWorkspaceTask(context.Background(), "nested-commits-task", EnsureWorkspaceTaskRequest{
		SessionID: created.ID, ExpectedRevision: created.Revision,
		Task: tasks.ControllerTaskDraft{OfferRef: "test:nested-commits", Title: "Preserve nested work", Prompt: "Keep child commits", SuccessChecks: []string{"child history retained"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(created.Workspace, "vendor/child")
	if err := os.WriteFile(filepath.Join(child, "new-code"), []byte("valuable child commit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, child, "config", "user.name", "Test")
	sessionWorkspaceGit(t, child, "config", "user.email", "test@example.invalid")
	sessionWorkspaceGit(t, child, "add", "new-code")
	sessionWorkspaceGit(t, child, "commit", "-qm", "child work")
	sessionWorkspaceGit(t, created.Workspace, "add", "vendor/child")
	sessionWorkspaceGit(t, created.Workspace, "commit", "-qm", "parent gitlink")
	request := CheckpointWorkspaceRequest{SessionID: created.ID, SessionRef: "session-work-1", ExpectedRevision: created.Revision, PlacementGeneration: 1, RepositoryRef: "test"}
	if _, _, _, err := buildWorkspaceCheckpoint(context.Background(), t.TempDir(), created, request, "nested-committed-checkpoint", time.Now()); err == nil || !strings.Contains(err.Error(), "nested work needs separate custody") {
		t.Fatalf("checkpoint did not refuse missing child history custody: %v", err)
	}
}

func TestNestedWorkCannotHideBehindIndexFlags(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			repository := nestedSessionSource(t)
			fixture := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repository, nil)
			defer fixture.Stop()
			created, err := fixture.CreateRemoteSession(context.Background(), "nested-index", fixture.request(t, "test:nested-index"))
			if err != nil {
				t.Fatal(err)
			}
			plan, err := planSessionWorkspaceDiscard(created.Repository, created.Workspace, false, false)
			if err != nil {
				t.Fatal(err)
			}
			child := filepath.Join(created.Workspace, "vendor/child/nested space")
			sessionWorkspaceGit(t, child, "update-index", flag, "code")
			if err := os.WriteFile(filepath.Join(child, "code"), []byte("hidden work\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if status := sessionWorkspaceGit(t, child, "status", "--porcelain"); status != "" {
				t.Fatalf("index flag positive control did not hide the edit: %s", status)
			}
			if _, err := captureSessionReviewSource(created.Repository, created.Workspace, created.ForkName); err == nil {
				t.Fatal("review trusted child index flags")
			}
			if _, _, _, err := buildWorkspaceCheckpoint(context.Background(), t.TempDir(), created, CheckpointWorkspaceRequest{}, "hidden-checkpoint", time.Now()); err == nil || !strings.Contains(err.Error(), "nested work needs separate custody") {
				t.Fatalf("checkpoint trusted child index flags: %v", err)
			}
			if _, err := planSessionWorkspaceDiscard(created.Repository, created.Workspace, false, false); err == nil {
				t.Fatal("discard planning trusted child index flags")
			}
			if err := discardSessionWorkspace(plan); err == nil {
				t.Fatal("stale discard trusted child index flags")
			}
		})
	}
}

func TestCompanionAdoptionRequiresCompleteNestedContent(t *testing.T) {
	repository := nestedSessionSource(t)
	fixture := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repository, nil)
	defer fixture.Stop()
	fixture.companion(t, "reference", repository)
	created, err := fixture.CreateRemoteSession(context.Background(), "missing-child", fixture.request(t, "test:missing-child"))
	if err != nil {
		t.Fatal(err)
	}
	companion := created.Companions[0]
	child := filepath.Join(companion.Workspace, "vendor/child")
	if err := os.RemoveAll(child); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSessionCompanion(fixture.stateRoot, created.ID, companion); err == nil {
		t.Fatal("adopted incomplete companion as a usable source")
	}
}

func TestSubmoduleCannotRedirectItsGitStorage(t *testing.T) {
	for _, part := range []string{"commondir", "objects"} {
		t.Run(part, func(t *testing.T) {
			repository := nestedSessionSource(t)
			child := filepath.Join(repository, "vendor/child/nested space")
			head := gitOut(child, "rev-parse", "HEAD")
			metadata := filepath.Join(child, ".git")
			switch part {
			case "commondir":
				if err := os.WriteFile(filepath.Join(metadata, part), []byte(metadata+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "objects":
				moved := filepath.Join(t.TempDir(), "objects")
				if err := os.Rename(filepath.Join(metadata, part), moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, filepath.Join(metadata, part)); err != nil {
					t.Fatal(err)
				}
			}
			if err := verifyPinnedSubmodule(context.Background(), child, head); err == nil {
				t.Fatal("accepted redirected child Git storage")
			}
		})
	}
}

func TestPinnedInspectionDoesNotRewriteTheAgentsIndex(t *testing.T) {
	repository := nestedSessionSource(t)
	child := filepath.Join(repository, "vendor/child/nested space")
	head := gitOut(child, "rev-parse", "HEAD")
	sessionWorkspaceGit(t, child, "update-index", "--assume-unchanged", "code")
	if err := os.WriteFile(filepath.Join(child, "staged"), []byte("staged work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, child, "add", "staged")
	before, err := os.ReadFile(filepath.Join(child, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPinnedSubmodule(context.Background(), child, head); err == nil {
		t.Fatal("inspection accepted staged child work")
	}
	after, err := os.ReadFile(filepath.Join(child, ".git", "index"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read-only inspection rewrote the agent's index")
	}
}
