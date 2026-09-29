package sessionsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

const testSessionStoreID = "store_test"

func sessionWorkspaceGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := sessionWorkspaceGitResult(dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(out)
}

func sessionWorkspaceGitResult(dir string, args ...string) (string, error) {
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(os.TempDir(), "coop-session-workspace-no-global"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(os.TempDir(), "coop-session-workspace-no-system"))
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func sessionWorkspaceWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sessionWorkspaceHasPath(changes []sessionWorkspaceChange, path, status string) bool {
	for _, change := range changes {
		if change.Path == path && (status == "" || change.Status == status) {
			return true
		}
	}
	return false
}

func TestSessionWorkspaceCreateCapturesExactParentHead(t *testing.T) {
	repo, git := gitrepo.New(t)
	sessionWorkspaceWrite(t, filepath.Join(repo, "base.txt"), "base\n")
	git("add", "base.txt")
	git("commit", "-qm", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	parentRefsBefore := sessionWorkspaceGit(t, repo, "for-each-ref", "--format=%(refname)=%(objectname)")

	created, err := createSessionWorkspace(repo, "remote-1")
	if err != nil {
		t.Fatal(err)
	}
	if created.Repo != repo || created.Name != "remote-1" || created.Path != forkspace.Workspace(repo, "remote-1") {
		t.Fatalf("created workspace identity = %+v", created)
	}
	if created.BaseCommit != base || created.ForkHead != base {
		t.Fatalf("created workspace commits = %+v, want base %s", created, base)
	}
	if created.Branch != "remote-1" || sessionWorkspaceGit(t, created.Path, "branch", "--show-current") != "remote-1" {
		t.Fatalf("created workspace branch = %q", created.Branch)
	}
	if got := sessionWorkspaceGit(t, repo, "rev-parse", "HEAD"); got != base {
		t.Fatalf("parent HEAD changed to %s, want %s", got, base)
	}
	if got := sessionWorkspaceGit(t, repo, "for-each-ref", "--format=%(refname)=%(objectname)"); got != parentRefsBefore {
		t.Fatalf("parent refs changed during workspace creation:\nbefore %s\nafter %s", parentRefsBefore, got)
	}
}

func TestSessionWorkspacePreservesPublishedGenerationAfterAmbiguousCreateError(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	failure := errors.New("synthetic post-publish sync failure")
	previous := ensureSessionGenerationLocked
	ensureSessionGenerationLocked = func(repo, name string) (forkspace.Identity, error) {
		identity, err := previous(repo, name)
		if err != nil {
			return identity, err
		}
		return forkspace.Identity{}, failure
	}
	t.Cleanup(func() { ensureSessionGenerationLocked = previous })

	_, err := ensureSessionWorkspaceContext(context.Background(), nil, repo, "ambiguous", base, testSessionStoreID, "ambiguous")
	if !errors.Is(err, failure) {
		t.Fatalf("create error = %v, want %v", err, failure)
	}
	workspace := forkspace.Workspace(repo, "ambiguous")
	if info, err := os.Lstat(workspace); err != nil || !info.IsDir() {
		t.Fatalf("ambiguous publication deleted its anchored workspace: %+v, %v", info, err)
	}
	published, present, err := forkspace.ReadGeneration(repo, "ambiguous")
	if err != nil || !present {
		t.Fatalf("published generation = %+v, present=%v, err=%v", published, present, err)
	}
	if err := forkspace.ValidateGenerationWorkspace(repo, published); err != nil {
		t.Fatalf("published generation no longer validates: %v", err)
	}
	if _, err := ensureSessionWorkspaceContext(context.Background(), nil, repo, "ambiguous", base, testSessionStoreID, "ambiguous"); !errors.Is(err, errSessionWorkspacePublicationPending) {
		t.Fatalf("valid but unsynced existing generation lost retryability: %v", err)
	}

	ensureSessionGenerationLocked = previous
	recovered, err := ensureSessionWorkspaceContext(context.Background(), nil, repo, "ambiguous", base, testSessionStoreID, "ambiguous")
	if err != nil || recovered.Fork != published {
		t.Fatalf("retry = %+v, %v; want published generation %+v", recovered, err, published)
	}
}

func TestSessionWorkspacePreservesGenerationWhenReservationPublicationFails(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	reserveFailure := errors.New("synthetic reservation failure")
	retireCalled := false
	previousReserve, previousRemove := reserveSessionWorkspaceLocked, removeSessionGenerationIfMatches
	reserveSessionWorkspaceLocked = func(string, forkspace.WorkspaceReservation) error { return reserveFailure }
	removeSessionGenerationIfMatches = func(string, forkspace.Identity) error {
		retireCalled = true
		return errors.New("generation retirement must not begin")
	}
	t.Cleanup(func() {
		reserveSessionWorkspaceLocked = previousReserve
		removeSessionGenerationIfMatches = previousRemove
	})

	_, err := ensureSessionWorkspaceContext(context.Background(), nil, repo, "reserve-rollback", base, testSessionStoreID, "remote_owner")
	if !errors.Is(err, reserveFailure) {
		t.Fatalf("create error = %v, want reservation failure", err)
	}
	if retireCalled {
		t.Fatal("reservation failure began generation retirement")
	}
	workspace := forkspace.Workspace(repo, "reserve-rollback")
	if info, err := os.Lstat(workspace); err != nil || !info.IsDir() {
		t.Fatalf("reservation failure deleted the workspace: %+v, %v", info, err)
	}
	published, present, err := forkspace.ReadGeneration(repo, "reserve-rollback")
	if err != nil || !present {
		t.Fatalf("preserved generation = %+v, present=%v, err=%v", published, present, err)
	}
	if err := forkspace.ValidateGenerationWorkspace(repo, published); err != nil {
		t.Fatalf("preserved generation no longer validates: %v", err)
	}

	reserveSessionWorkspaceLocked, removeSessionGenerationIfMatches = previousReserve, previousRemove
	recovered, err := ensureSessionWorkspaceContext(context.Background(), nil, repo, "reserve-rollback", base, testSessionStoreID, "remote_owner")
	if err != nil || recovered.Fork != published {
		t.Fatalf("retry = %+v, %v; want preserved generation %+v", recovered, err, published)
	}
}

func TestSessionWorkspacePreservesPublishedReservationAfterAmbiguousCreate(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	failure := errors.New("synthetic reservation publication ambiguity")
	previous := reserveSessionWorkspaceLocked
	reserveSessionWorkspaceLocked = func(repo string, reservation forkspace.WorkspaceReservation) error {
		if err := previous(repo, reservation); err != nil {
			return err
		}
		return failure
	}
	t.Cleanup(func() { reserveSessionWorkspaceLocked = previous })

	_, err := ensureSessionWorkspaceContext(context.Background(), nil, repo, "reservation-ambiguous", base, testSessionStoreID, "remote_owner")
	if !errors.Is(err, failure) {
		t.Fatalf("create error = %v, want %v", err, failure)
	}
	workspace := forkspace.Workspace(repo, "reservation-ambiguous")
	if info, err := os.Lstat(workspace); err != nil || !info.IsDir() {
		t.Fatalf("ambiguous reservation deleted its workspace: %+v, %v", info, err)
	}
	identity, present, err := forkspace.ReadGeneration(repo, "reservation-ambiguous")
	if err != nil || !present {
		t.Fatalf("generation after ambiguous reservation = %+v, present=%v err=%v", identity, present, err)
	}
	if reservation, reserved, err := forkspace.ReadWorkspaceReservation(repo, identity); err != nil || !reserved || reservation.OwnerID != "remote_owner" {
		t.Fatalf("published reservation = %+v, reserved=%v err=%v", reservation, reserved, err)
	}
	if _, err := ensureSessionWorkspaceContext(context.Background(), nil, repo, "reservation-ambiguous", base, testSessionStoreID, "remote_owner"); !errors.Is(err, errSessionWorkspacePublicationPending) {
		t.Fatalf("valid but unsynced existing reservation lost retryability: %v", err)
	}

	reserveSessionWorkspaceLocked = previous
	recovered, err := ensureSessionWorkspaceContext(context.Background(), nil, repo, "reservation-ambiguous", base, testSessionStoreID, "remote_owner")
	if err != nil || recovered.Fork != identity {
		t.Fatalf("retry = %+v, %v; want reserved generation %+v", recovered, err, identity)
	}
}

func TestExistingSessionWorkspacePermanentAuthorityErrorsAreNotPublicationPending(t *testing.T) {
	for _, cause := range []string{"broken generation anchor", "missing reservation after refusal"} {
		t.Run(cause, func(t *testing.T) {
			repo, git := gitrepo.New(t)
			git("commit", "-q", "--allow-empty", "-m", "base")
			base := gitOut(repo, "rev-parse", "HEAD")
			name, owner := "permanent-refusal", "remote_owner"
			created, err := ensureSessionWorkspaceContext(t.Context(), nil, repo, name, base, testSessionStoreID, owner)
			if err != nil {
				t.Fatal(err)
			}
			switch cause {
			case "broken generation anchor":
				anchor := filepath.Join(forkspace.StateDir(repo), "generation-"+string(created.Fork.Generation)+".anchor")
				if err := os.Remove(anchor); err != nil {
					t.Fatal(err)
				}
			case "missing reservation after refusal":
				unlock, err := forkspace.LockState(repo, name)
				if err != nil {
					t.Fatal(err)
				}
				reservation, reserved, readErr := forkspace.ReadWorkspaceReservation(repo, created.Fork)
				if readErr == nil && reserved {
					readErr = forkspace.RemoveWorkspaceReservationIfMatchesLocked(repo, reservation)
				}
				unlock()
				if readErr != nil || !reserved {
					t.Fatalf("remove fixture reservation: reserved=%t err=%v", reserved, readErr)
				}
				previous := reserveSessionWorkspaceLocked
				reserveSessionWorkspaceLocked = func(string, forkspace.WorkspaceReservation) error {
					return errors.New("reservation refused")
				}
				t.Cleanup(func() { reserveSessionWorkspaceLocked = previous })
			}
			_, err = ensureSessionWorkspaceContext(t.Context(), nil, repo, name, base, testSessionStoreID, owner)
			if err == nil || errors.Is(err, errSessionWorkspacePublicationPending) {
				t.Fatalf("permanent existing-workspace refusal = %v, want terminal error", err)
			}
			if info, statErr := os.Lstat(created.Path); statErr != nil || !info.IsDir() {
				t.Fatalf("existing workspace changed or removed: %+v, %v", info, statErr)
			}
		})
	}
}

func TestSessionWorkspaceCreateRemovesItsOwnersStaleReservationAfterRollbackCrash(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	owner := "remote_55555555555555555555555555555555"
	created, err := ensureSessionWorkspaceContext(context.Background(), nil, repo, "rollback-replay", base, testSessionStoreID, owner)
	if err != nil {
		t.Fatal(err)
	}
	old := created.Fork
	// Crash prefix from rollback: the workspace is already gone and generation retirement is
	// durable, but reservation retirement never ran.
	if err := forkspace.Destroy(repo, created.Name); err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, created.Name)
	if err != nil {
		t.Fatal(err)
	}
	err = forkspace.RemoveGenerationIfMatchesLocked(repo, old)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, reserved, err := forkspace.ReadWorkspaceReservation(repo, old); err != nil || !reserved {
		t.Fatalf("crash fixture reservation: reserved=%v err=%v", reserved, err)
	}

	recovered, err := ensureSessionWorkspaceContext(context.Background(), nil, repo, created.Name, base, testSessionStoreID, owner)
	if err != nil {
		t.Fatalf("create replay: %v", err)
	}
	if recovered.Fork == old {
		t.Fatal("create replay reused the retired generation")
	}
	records, problems := forkspace.WorkspaceReservations(repo)
	if len(problems) != 0 {
		t.Fatal(errors.Join(problems...))
	}
	matching := 0
	for _, record := range records {
		if record.Fork.Name == created.Name && record.OwnerID == owner {
			matching++
			if record.Fork != recovered.Fork {
				t.Fatalf("stale reservation survived replay: %+v", record)
			}
		}
	}
	if matching != 1 {
		t.Fatalf("matching reservations = %d, want exactly the replacement", matching)
	}
}

func TestSessionWorkspaceCreateConfirmsMissingReservationBeforeSetup(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	failure := errors.New("synthetic missing-reservation durability failure")
	previous := confirmSessionReservationState
	confirmSessionReservationState = func(string) error { return failure }
	t.Cleanup(func() { confirmSessionReservationState = previous })

	_, err := ensureSessionWorkspaceContext(
		context.Background(), nil, repo, "reservation-sync", base,
		testSessionStoreID,
		"remote_66666666666666666666666666666666",
	)
	if !errors.Is(err, failure) {
		t.Fatalf("create error = %v, want %v", err, failure)
	}
	if _, statErr := os.Lstat(forkspace.Workspace(repo, "reservation-sync")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("reservation durability failure still created a workspace: %v", statErr)
	}
}

func TestSessionWorkspaceRetryChecksOwnerBeforeHydration(t *testing.T) {
	for _, version := range []int{1, forkspace.WorkspaceReservationVersion} {
		t.Run(fmt.Sprintf("reservation-v%d", version), func(t *testing.T) {
			repo, git := gitrepo.New(t)
			git("commit", "-q", "--allow-empty", "-m", "base")
			base := gitOut(repo, "rev-parse", "HEAD")
			owner := "remote_retry"
			created, err := ensureSessionWorkspaceContext(t.Context(), nil, repo, "retry-owner", base, testSessionStoreID, owner)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(created.Path, "keep.txt")
			sessionWorkspaceWrite(t, marker, "keep\n")
			unlock, err := forkspace.LockState(repo, created.Name)
			if err != nil {
				t.Fatal(err)
			}
			current, reserved, err := forkspace.ReadWorkspaceReservation(repo, created.Fork)
			if err == nil && reserved {
				err = forkspace.RemoveWorkspaceReservationIfMatchesLocked(repo, current)
			}
			if err == nil {
				current.Version = version
				current.OwnerStoreID = "store_other"
				if version == 1 {
					current.OwnerStoreID = ""
					var body []byte
					body, err = json.Marshal(current)
					if err == nil {
						err = os.WriteFile(filepath.Join(forkspace.StateDir(repo), "reservations", created.Name+"."+string(created.Fork.Generation)+".json"), body, 0o600)
					}
				} else {
					err = forkspace.ReserveWorkspaceLocked(repo, current)
				}
			}
			unlock()
			if err != nil || !reserved {
				t.Fatalf("prepare foreign reservation: reserved=%t err=%v", reserved, err)
			}
			// If hydration runs first, this broken source HEAD produces a Git error
			// instead of the ownership refusal.
			git("symbolic-ref", "HEAD", "refs/heads/missing")
			if _, err := ensureSessionWorkspaceContext(t.Context(), nil, repo, created.Name, base, testSessionStoreID, owner); err == nil ||
				!strings.Contains(err.Error(), "another or unproven owner") {
				t.Fatalf("foreign reservation was checked after hydration: %v", err)
			}
			if body, err := os.ReadFile(marker); err != nil || string(body) != "keep\n" {
				t.Fatalf("foreign workspace changed: %q, %v", body, err)
			}
		})
	}
}

func TestSessionWorkspaceCreateRefusesExistingAndInvalidPartialWorkspace(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	partial := forkspace.Workspace(repo, "partial")
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(partial, "must-remain")
	sessionWorkspaceWrite(t, marker, "foreign\n")
	if _, err := createSessionWorkspace(repo, "partial"); err == nil {
		t.Fatal("existing partial workspace was accepted")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("existing partial workspace was changed: %v", err)
	}
	if _, err := createSessionWorkspace(repo, "bad..name"); err == nil {
		t.Fatal("invalid workspace name was accepted")
	}
	if pathExists(forkspace.Workspace(repo, "bad..name")) {
		t.Fatal("invalid workspace name left a partial workspace")
	}
}

func TestSessionWorkspaceInspectTypedChangesAndOddFilenames(t *testing.T) {
	repo, git := gitrepo.New(t)
	for name, body := range map[string]string{
		"committed.txt": "before committed\n",
		"staged.txt":    "before staged\n",
		"unstaged.txt":  "before unstaged\n",
	} {
		sessionWorkspaceWrite(t, filepath.Join(repo, name), body)
	}
	git("add", ".")
	git("commit", "-qm", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	created, err := createSessionWorkspace(repo, "changes")
	if err != nil {
		t.Fatal(err)
	}

	sessionWorkspaceWrite(t, filepath.Join(created.Path, "committed.txt"), "committed change\n")
	sessionWorkspaceGit(t, created.Path, "add", "committed.txt")
	sessionWorkspaceGit(t, created.Path, "commit", "-qm", "committed change")
	forkHead := gitOut(created.Path, "rev-parse", "HEAD")
	sessionWorkspaceWrite(t, filepath.Join(created.Path, "staged.txt"), "staged change\n")
	sessionWorkspaceGit(t, created.Path, "add", "staged.txt")
	sessionWorkspaceWrite(t, filepath.Join(created.Path, "unstaged.txt"), "unstaged change\n")
	oddName := "untracked file with spaces.txt"
	sessionWorkspaceWrite(t, filepath.Join(created.Path, oddName), "untracked contents must not enter patch\n")

	changes, err := inspectSessionChanges(repo, created.Path, base, sessionWorkspacePatchLimit)
	if err != nil {
		t.Fatal(err)
	}
	if changes.BaseCommit != base || changes.ForkHead != forkHead || changes.ParentHead != base {
		t.Fatalf("inspection identities = %+v", changes)
	}
	if !sessionWorkspaceHasPath(changes.Committed, "committed.txt", "M") {
		t.Fatalf("committed changes = %+v", changes.Committed)
	}
	if !sessionWorkspaceHasPath(changes.Staged, "staged.txt", "M") {
		t.Fatalf("staged changes = %+v", changes.Staged)
	}
	if !sessionWorkspaceHasPath(changes.Unstaged, "unstaged.txt", "M") {
		t.Fatalf("unstaged changes = %+v", changes.Unstaged)
	}
	if !sessionWorkspaceHasPath(changes.Untracked, oddName, "??") {
		t.Fatalf("untracked changes = %+v", changes.Untracked)
	}
	if len(changes.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts = %+v", changes.Conflicts)
	}
	if changes.ParentDivergence.Ahead != 1 || changes.ParentDivergence.Behind != 0 || changes.ParentDivergence.Diverged {
		t.Fatalf("parent divergence = %+v", changes.ParentDivergence)
	}
	if changes.Truncated || !strings.Contains(changes.Patch, "committed change") ||
		!strings.Contains(changes.Patch, "staged change") || !strings.Contains(changes.Patch, "unstaged change") {
		t.Fatalf("tracked patch = truncated=%v patch=%q", changes.Truncated, changes.Patch)
	}
	if strings.Contains(changes.Patch, oddName) || strings.Contains(changes.Patch, "untracked contents") {
		t.Fatal("untracked contents were embedded in tracked patch")
	}
	first, err := inspectSessionChangesPage(repo, created.Path, base, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	second, err := inspectSessionChangesPage(
		repo,
		created.Path,
		base,
		first.PatchNextOffset,
		64,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.PatchDigest == "" || first.PatchDigest != second.PatchDigest ||
		first.PatchBytes != second.PatchBytes || !first.PatchHasMore ||
		second.PatchOffset != first.PatchNextOffset ||
		string(first.Patch)+string(second.Patch) == "" {
		t.Fatalf("patch pages = first %+v second %+v", first, second)
	}

	sessionWorkspaceWrite(t, filepath.Join(repo, "parent-only.txt"), "parent advanced\n")
	git("add", "parent-only.txt")
	git("commit", "-qm", "parent advance")
	changes, err = inspectSessionChanges(repo, created.Path, base, sessionWorkspacePatchLimit)
	if err != nil {
		t.Fatalf("inspect after parent advance: %v", err)
	}
	if changes.ParentDivergence.Ahead != 1 || changes.ParentDivergence.Behind != 1 || !changes.ParentDivergence.Diverged {
		t.Fatalf("divergence after parent advance = %+v", changes.ParentDivergence)
	}
}

func TestSessionWorkspaceInspectTruncatesPatchAndSurfacesGitFailure(t *testing.T) {
	repo, git := gitrepo.New(t)
	sessionWorkspaceWrite(t, filepath.Join(repo, "large.txt"), "base\n")
	git("add", "large.txt")
	git("commit", "-qm", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	created, err := createSessionWorkspace(repo, "patch")
	if err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(created.Path, "large.txt"), strings.Repeat("large tracked change\n", 20))
	changes, err := inspectSessionChanges(repo, created.Path, base, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !changes.Truncated || len(changes.Patch) > 16 {
		t.Fatalf("truncated patch = %d bytes, truncated=%v", len(changes.Patch), changes.Truncated)
	}
	if _, err := inspectSessionChanges(repo, filepath.Join(t.TempDir(), "not-a-git-workspace"), base, 16); err == nil {
		t.Fatal("Git failure was reported as a clean result")
	}
}

func TestSessionWorkspaceDiscardClean(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	created, err := createSessionWorkspace(repo, "discard-clean")
	if err != nil {
		t.Fatal(err)
	}
	parentHead := gitOut(repo, "rev-parse", "HEAD")
	plan, err := planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Dirty || plan.Unmerged || plan.Running || plan.AcceptedDirty || plan.AcceptedUnmerged {
		t.Fatalf("clean discard plan = %+v", plan)
	}
	if err := discardSessionWorkspace(plan); err != nil {
		t.Fatal(err)
	}
	if pathExists(created.Path) {
		t.Fatal("clean discard left the workspace")
	}
	if got := sessionWorkspaceGit(t, repo, "rev-parse", "HEAD"); got != parentHead {
		t.Fatalf("clean discard changed parent HEAD to %s, want %s", got, parentHead)
	}
}

func TestSessionWorkspaceDiscardKeepsAuthorityWhenStageDurabilityIsUncertain(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	created, err := ensureSessionWorkspaceContext(
		context.Background(), nil, repo, "discard-stage-sync", base,
		testSessionStoreID,
		"remote_44444444444444444444444444444444",
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil || plan.Fork == nil || plan.Reservation == nil {
		t.Fatalf("discard plan = %+v, %v; want exact generation and reservation", plan, err)
	}

	previous := stageSessionWorkspaceDiscard
	t.Cleanup(func() { stageSessionWorkspaceDiscard = previous })
	failure := errors.New("synthetic post-rename durability failure")
	stageSessionWorkspaceDiscard = func(repo, name string, pinned os.FileInfo) (string, error) {
		staged, err := previous(repo, name, pinned)
		if err != nil {
			return staged, err
		}
		return staged, failure
	}

	if err := discardSessionWorkspace(plan); !errors.Is(err, failure) {
		t.Fatalf("first discard error = %v, want %v", err, failure)
	}
	if _, present, err := forkspace.ReadGeneration(repo, created.Name); err != nil || !present {
		t.Fatalf("generation retired after uncertain stage: present=%v err=%v", present, err)
	}
	if _, reserved, err := forkspace.ReadWorkspaceReservation(repo, *plan.Fork); err != nil || !reserved {
		t.Fatalf("reservation retired after uncertain stage: reserved=%v err=%v", reserved, err)
	}

	stageSessionWorkspaceDiscard = previous
	if err := discardSessionWorkspace(plan); err != nil {
		t.Fatalf("discard durability retry: %v", err)
	}
	if _, present, err := forkspace.ReadGeneration(repo, created.Name); err != nil || present {
		t.Fatalf("generation after retry: present=%v err=%v", present, err)
	}
	if _, reserved, err := forkspace.ReadWorkspaceReservation(repo, *plan.Fork); err != nil || reserved {
		t.Fatalf("reservation after retry: reserved=%v err=%v", reserved, err)
	}
}

func TestSessionWorkspaceDiscardReplaysEachAuthorityCleanupPrefix(t *testing.T) {
	for _, first := range []string{"reservation", "generation"} {
		t.Run(first, func(t *testing.T) {
			repo, git := gitrepo.New(t)
			git("commit", "-q", "--allow-empty", "-m", "base")
			base := gitOut(repo, "rev-parse", "HEAD")
			created, err := ensureSessionWorkspaceContext(
				context.Background(), nil, repo, "discard-prefix", base,
				testSessionStoreID,
				"remote_11111111111111111111111111111111",
			)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := planSessionWorkspaceDiscard(repo, created.Path, false, false)
			if err != nil || plan.Fork == nil || plan.Reservation == nil {
				t.Fatalf("discard plan = %+v, %v; want exact generation and reservation", plan, err)
			}
			// Both real cleanup orders reach this point only after the workspace was staged and
			// destroyed. Simulate a crash after the first authority record is retired.
			if err := forkspace.Destroy(repo, created.Name); err != nil {
				t.Fatal(err)
			}
			unlock, err := forkspace.LockState(repo, created.Name)
			if err != nil {
				t.Fatal(err)
			}
			switch first {
			case "reservation":
				err = forkspace.RemoveWorkspaceReservationIfMatchesLocked(repo, *plan.Reservation)
			case "generation":
				err = forkspace.RemoveGenerationIfMatchesLocked(repo, *plan.Fork)
			}
			unlock()
			if err != nil {
				t.Fatal(err)
			}

			if err := discardSessionWorkspace(plan); err != nil {
				t.Fatalf("replay after %s cleanup: %v", first, err)
			}
			if _, present, err := forkspace.ReadGeneration(repo, created.Name); err != nil || present {
				t.Fatalf("generation after replay: present=%v err=%v", present, err)
			}
			if _, reserved, err := forkspace.ReadWorkspaceReservation(repo, *plan.Fork); err != nil || reserved {
				t.Fatalf("reservation after replay: reserved=%v err=%v", reserved, err)
			}
		})
	}
}

func TestSessionWorkspaceDiscardRetryRepeatsMissingGenerationDurabilityBeforeReservation(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	created, err := ensureSessionWorkspaceContext(
		context.Background(), nil, repo, "discard-sync-retry", base,
		testSessionStoreID,
		"remote_22222222222222222222222222222222",
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil || plan.Fork == nil || plan.Reservation == nil {
		t.Fatalf("discard plan = %+v, %v; want exact generation and reservation", plan, err)
	}

	previous := removeSessionGenerationIfMatches
	t.Cleanup(func() { removeSessionGenerationIfMatches = previous })
	failure := errors.New("synthetic post-unlink generation sync failure")
	calls := 0
	removeSessionGenerationIfMatches = func(repo string, identity forkspace.Identity) error {
		calls++
		if calls == 2 {
			if _, reserved, err := forkspace.ReadWorkspaceReservation(repo, identity); err != nil || !reserved {
				t.Fatalf("generation durability retry ran after reservation removal: reserved=%v err=%v", reserved, err)
			}
		}
		if err := previous(repo, identity); err != nil {
			return err
		}
		if calls == 1 {
			return failure
		}
		return nil
	}

	if err := discardSessionWorkspace(plan); !errors.Is(err, failure) {
		t.Fatalf("first discard error = %v, want %v", err, failure)
	}
	if _, present, err := forkspace.ReadGeneration(repo, created.Name); err != nil || present {
		t.Fatalf("first discard generation: present=%v err=%v", present, err)
	}
	if _, reserved, err := forkspace.ReadWorkspaceReservation(repo, *plan.Fork); err != nil || !reserved {
		t.Fatalf("first discard reservation: reserved=%v err=%v", reserved, err)
	}
	if err := discardSessionWorkspace(plan); err != nil {
		t.Fatalf("discard retry: %v", err)
	}
	if calls != 2 {
		t.Fatalf("generation removal calls = %d, want retry of missing-record durability", calls)
	}
	if _, reserved, err := forkspace.ReadWorkspaceReservation(repo, *plan.Fork); err != nil || reserved {
		t.Fatalf("reservation after retry: reserved=%v err=%v", reserved, err)
	}
}

func TestSessionWorkspaceDiscardRetryRepeatsMissingReservationDurability(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	created, err := ensureSessionWorkspaceContext(
		context.Background(), nil, repo, "discard-reservation-sync", base,
		testSessionStoreID,
		"remote_33333333333333333333333333333333",
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil || plan.Fork == nil || plan.Reservation == nil {
		t.Fatalf("discard plan = %+v, %v; want exact generation and reservation", plan, err)
	}

	previous := removeSessionReservationIfMatches
	t.Cleanup(func() { removeSessionReservationIfMatches = previous })
	failure := errors.New("synthetic post-unlink reservation sync failure")
	calls := 0
	removeSessionReservationIfMatches = func(repo string, reservation forkspace.WorkspaceReservation) error {
		calls++
		if err := previous(repo, reservation); err != nil {
			return err
		}
		if calls == 1 {
			return failure
		}
		return nil
	}

	if err := discardSessionWorkspace(plan); !errors.Is(err, failure) {
		t.Fatalf("first discard error = %v, want %v", err, failure)
	}
	if _, reserved, err := forkspace.ReadWorkspaceReservation(repo, *plan.Fork); err != nil || reserved {
		t.Fatalf("first discard reservation: reserved=%v err=%v", reserved, err)
	}
	if err := discardSessionWorkspace(plan); err != nil {
		t.Fatalf("discard retry: %v", err)
	}
	if calls != 2 {
		t.Fatalf("reservation removal calls = %d, want retry of missing-record durability", calls)
	}
}

func TestSessionWorkspaceDiscardRefusesStaleHeadStatusReplacementAndRunning(t *testing.T) {
	repo, git := gitrepo.New(t)
	sessionWorkspaceWrite(t, filepath.Join(repo, "file.txt"), "base\n")
	git("add", "file.txt")
	git("commit", "-qm", "base")
	created, err := createSessionWorkspace(repo, "discard-stale")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, created.Path, "commit", "--allow-empty", "-qm", "head moved")
	if err := discardSessionWorkspace(plan); err == nil {
		t.Fatal("stale HEAD discard unexpectedly succeeded")
	}
	if !pathExists(created.Path) {
		t.Fatal("stale HEAD discard removed the workspace")
	}

	plan, err = planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(created.Path, "new status.txt"), "status changed\n")
	if err := discardSessionWorkspace(plan); err == nil {
		t.Fatal("stale status discard unexpectedly succeeded")
	}
	if !pathExists(created.Path) {
		t.Fatal("stale status discard removed the workspace")
	}
	os.Remove(filepath.Join(created.Path, "new status.txt"))

	plan, err = planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	replaced := created.Path + ".old"
	if err := os.Rename(created.Path, replaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(created.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := discardSessionWorkspace(plan); err == nil {
		t.Fatal("replaced workspace discard unexpectedly succeeded")
	}
	if !pathExists(created.Path) || !pathExists(replaced) {
		t.Fatal("replaced workspace discard removed a path")
	}
	if err := os.Remove(created.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replaced, created.Path); err != nil {
		t.Fatal(err)
	}

	plan, err = planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkspace.PidPath(repo, created.Name), []byte(forkspace.ReapPending), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := discardSessionWorkspace(plan); err == nil {
		t.Fatal("running workspace discard unexpectedly succeeded")
	}
	if !pathExists(created.Path) {
		t.Fatal("running workspace discard removed the workspace")
	}
	if err := os.Remove(forkspace.PidPath(repo, created.Name)); err != nil {
		t.Fatal(err)
	}
}

// A discard plan made before a reboot names the old device for the very same workspace. The
// authoritative fork generation is now a hardlink identity and does not record the allocator's
// device number. The discard must still go through; a workspace recreated at the path is still
// refused (TestSessionWorkspaceDiscardRefusesStaleHeadStatusReplacementAndRunning).
func TestSessionWorkspaceDiscardSurvivesADeviceRenumber(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	created, err := createSessionWorkspace(repo, "discard-rebooted")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	plan.WorkspaceIdentity.Device++
	if err := discardSessionWorkspace(plan); err != nil {
		t.Fatalf("a discard planned before a reboot was refused: %v", err)
	}
	if pathExists(created.Path) {
		t.Fatal("the discard left the workspace")
	}
}

func TestSessionWorkspaceDiscardRequiresExactDirtyAndUnmergedAcknowledgement(t *testing.T) {
	repo, git := gitrepo.New(t)
	sessionWorkspaceWrite(t, filepath.Join(repo, "conflict.txt"), "base\n")
	git("add", "conflict.txt")
	git("commit", "-qm", "base")
	parentBranch := gitOut(repo, "branch", "--show-current")
	created, err := createSessionWorkspace(repo, "discard-dirty")
	if err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(created.Path, "conflict.txt"), "fork\n")
	dirtyPlan, err := planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil || !dirtyPlan.Dirty || dirtyPlan.Unmerged {
		t.Fatalf("dirty discard plan = %+v, err=%v", dirtyPlan, err)
	}
	if err := discardSessionWorkspace(dirtyPlan); err == nil {
		t.Fatal("dirty discard without acknowledgement unexpectedly succeeded")
	}
	ackPlan, err := planSessionWorkspaceDiscard(repo, created.Path, true, false)
	if err != nil || !ackPlan.AcceptedDirty {
		t.Fatalf("dirty acknowledgement plan = %+v, err=%v", ackPlan, err)
	}
	if err := discardSessionWorkspace(ackPlan); err != nil {
		t.Fatal(err)
	}

	created, err = createSessionWorkspace(repo, "discard-unmerged")
	if err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(created.Path, "conflict.txt"), "fork\n")
	sessionWorkspaceGit(t, created.Path, "add", "conflict.txt")
	sessionWorkspaceGit(t, created.Path, "commit", "-qm", "fork change")
	sessionWorkspaceWrite(t, filepath.Join(repo, "conflict.txt"), "parent\n")
	git("add", "conflict.txt")
	git("commit", "-qm", "parent change")
	sessionWorkspaceGit(t, created.Path, "fetch", "origin")
	if _, err := sessionWorkspaceGitResult(created.Path, "merge", "--no-edit", "origin/"+parentBranch); err == nil {
		t.Fatal("expected merge conflict did not occur")
	}
	plan, err := planSessionWorkspaceDiscard(repo, created.Path, true, false)
	if err != nil || !plan.Dirty || !plan.Unmerged || plan.AcceptedUnmerged {
		t.Fatalf("unmerged discard plan = %+v, err=%v", plan, err)
	}
	if err := discardSessionWorkspace(plan); err == nil {
		t.Fatal("unmerged discard without acknowledgement unexpectedly succeeded")
	}
	plan, err = planSessionWorkspaceDiscard(repo, created.Path, true, true)
	if err != nil || !plan.AcceptedDirty || !plan.AcceptedUnmerged {
		t.Fatalf("unmerged acknowledgement plan = %+v, err=%v", plan, err)
	}
	if err := discardSessionWorkspace(plan); err != nil {
		t.Fatal(err)
	}
	if pathExists(created.Path) {
		t.Fatal("acknowledged unmerged discard left the workspace")
	}

	created, err = createSessionWorkspace(repo, "discard-clean-commit")
	if err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(created.Path, "committed-only.txt"), "valuable commit\n")
	sessionWorkspaceGit(t, created.Path, "add", "committed-only.txt")
	sessionWorkspaceGit(t, created.Path, "commit", "-qm", "valuable clean commit")
	plan, err = planSessionWorkspaceDiscard(repo, created.Path, false, false)
	if err != nil || plan.Dirty || !plan.Unmerged {
		t.Fatalf("clean committed discard plan = %+v, err=%v", plan, err)
	}
	if err := discardSessionWorkspace(plan); err == nil {
		t.Fatal("clean unlanded commit was discarded without acknowledgement")
	}
	plan, err = planSessionWorkspaceDiscard(repo, created.Path, false, true)
	if err != nil || !plan.AcceptedUnmerged {
		t.Fatalf("clean committed acknowledgement plan = %+v, err=%v", plan, err)
	}
	if err := discardSessionWorkspace(plan); err != nil {
		t.Fatal(err)
	}
}

func TestSessionWorkspaceDiscardPlansAMissingWorkspaceAsAbsent(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	missing := forkspace.Workspace(repo, "vanished")
	plan, err := planSessionWorkspaceDiscard(repo, missing, false, false)
	if err != nil {
		t.Fatalf("missing workspace plan = %v", err)
	}
	if plan.Repo != repo || plan.Name != "vanished" || plan.Workspace != missing {
		t.Fatalf("missing workspace plan identity = %+v", plan)
	}
	// Nothing exists, so nothing may be claimed as work worth protecting.
	if plan.Dirty || plan.Unmerged || plan.Running || plan.Branch != "" || plan.Head != "" {
		t.Fatalf("missing workspace plan invented work to protect: %+v", plan)
	}
	if plan.StatusDigest != sessionWorkspaceStatusDigest(nil) {
		t.Fatalf("missing workspace status digest = %q, want the empty-status digest", plan.StatusDigest)
	}
	if err := discardSessionWorkspace(plan); err != nil {
		t.Fatalf("missing workspace discard = %v", err)
	}

	// Absence is the only shortcut: a workspace that exists but cannot be
	// inspected may hold work, so planning it must keep failing loudly.
	if err := os.MkdirAll(forkspace.Home(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := forkspace.Workspace(repo, "broken")
	sessionWorkspaceWrite(t, broken, "not a directory\n")
	if _, err := planSessionWorkspaceDiscard(repo, broken, false, false); err == nil ||
		!strings.Contains(err.Error(), "pin session workspace") {
		t.Fatalf("uninspectable workspace plan = %v", err)
	}
}

func TestCreatedSessionWorkspaceCleanupConfirmsRemovalDurability(t *testing.T) {
	repo := t.TempDir()
	path := forkspace.Workspace(repo, "partial")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("original create failure")
	barrier := errors.New("parent directory sync failed")
	previous := confirmSessionDiscardState
	defer func() { confirmSessionDiscardState = previous }()
	called := false
	confirmSessionDiscardState = func(got string) error {
		called = true
		if got != repo {
			t.Errorf("synced %q, want %q", got, repo)
		}
		return barrier
	}
	err := removeCreatedSessionWorkspace(repo, path, cause)
	if !called || !errors.Is(err, cause) || !errors.Is(err, barrier) {
		t.Fatalf("cleanup = %v; want original failure and sync failure", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace remains after cleanup: %v", err)
	}
}
