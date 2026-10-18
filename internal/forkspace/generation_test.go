package forkspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/fsidentity"
	"github.com/AndrewDryga/coop/internal/testutil/starttoken"
)

func makeGenerationWorkspace(t *testing.T, repo, name string) {
	t.Helper()
	if err := os.MkdirAll(Workspace(repo, name), 0o755); err != nil {
		t.Fatal(err)
	}
}

func ensureTestGeneration(t *testing.T, repo, name string) Identity {
	t.Helper()
	unlock, err := LockState(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	identity, err := EnsureGenerationLocked(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestForkGenerationIsStableAndFencesWorkspaceReplacement(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "perf")
	first := ensureTestGeneration(t, repo, "perf")
	second := ensureTestGeneration(t, repo, "perf")
	if first != second {
		t.Fatalf("resume generation changed: %+v -> %+v", first, second)
	}
	if err := ValidateGenerationWorkspace(repo, first); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(Workspace(repo, "perf")); err != nil {
		t.Fatal(err)
	}
	makeGenerationWorkspace(t, repo, "perf")
	if err := ValidateGenerationWorkspace(repo, first); err == nil {
		t.Fatal("replacement workspace inherited the old generation")
	}

	unlock, err := LockState(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveGenerationIfMatchesLocked(repo, first); err != nil {
		unlock()
		t.Fatal(err)
	}
	replacement, err := EnsureGenerationLocked(repo, "perf")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Generation == first.Generation {
		t.Fatal("reused fork name received the same generation")
	}
}

// A fork is bound to its record by a hard link from its folder, which cannot cross disks, so a fork
// whose folder is on another disk than Coop's private state is refused, naming both folders, and
// nothing is recorded.
func TestForkGenerationRefusesAProjectOnAnotherDisk(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "perf")
	before := sameDisk
	sameDisk = func(string, string) (bool, error) { return false, nil }
	t.Cleanup(func() { sameDisk = before })
	unlock, err := LockState(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = EnsureGenerationLocked(repo, "perf")
	if err == nil || !strings.Contains(err.Error(), Workspace(repo, "perf")) || !strings.Contains(err.Error(), StateDir(repo)) || !strings.Contains(err.Error(), "one disk") {
		t.Fatalf("fork on another disk = %v; want a refusal naming %s and %s", err, Workspace(repo, "perf"), StateDir(repo))
	}
	if _, err := os.Lstat(GenerationPath(repo, "perf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused fork left a generation record: %v", err)
	}
}

func TestForkGenerationRefusesNameWhilePriorSessionWorkspaceIsStaged(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "remote")
	identity := ensureTestGeneration(t, repo, "remote")
	record := WorkspaceReservation{
		Version: workspaceReservationVersion, Fork: identity,
		Kind: WorkspaceReservationRemoteSession, OwnerStoreID: testReservationStoreID, OwnerID: "remote_staged", CreatedAt: time.Now().UTC(),
	}
	handle, info, err := Pin(Workspace(repo, "remote"))
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	unlock, err := LockState(repo, "remote")
	if err != nil {
		t.Fatal(err)
	}
	if err := ReserveWorkspaceLocked(repo, record); err != nil {
		unlock()
		t.Fatal(err)
	}
	if _, err := StageWorkspaceDiscardLocked(repo, "remote", info); err != nil {
		unlock()
		t.Fatal(err)
	}
	if err := RemoveGenerationIfMatchesLocked(repo, identity); err != nil {
		unlock()
		t.Fatal(err)
	}
	// Simulate a caller that bypassed Setup and recreated the public directory. The authority
	// boundary must still reject it while the prior session's staged bytes/name tombstone remain.
	makeGenerationWorkspace(t, repo, "remote")
	_, ensureErr := EnsureGenerationLocked(repo, "remote")
	unlock()
	if ensureErr == nil || !strings.Contains(ensureErr.Error(), "still being discarded") {
		t.Fatalf("replacement generation error = %v, want staged-discard refusal", ensureErr)
	}
	if _, present, err := ReadGeneration(repo, "remote"); err != nil || present {
		t.Fatalf("replacement generation after refusal: present=%v err=%v", present, err)
	}
}

func writeLegacyGenerationRecord(t *testing.T, repo, name string, version int) generationRecord {
	t.Helper()
	device, inode, err := workspaceGeneration(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	record := generationRecord{Version: version, Name: name, Generation: Generation("0123456789abcdef0123456789abcdef"),
		WorkspaceDevice: device, WorkspaceInode: inode, CreatedAt: time.Now().UTC()}
	if version == forkGenerationBirthVersion {
		record.WorkspaceBirthSec = 1
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeGenerationAtomic(repo, name, append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestForkGenerationDoesNotPersistReusableAllocatorIdentity(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "perf")
	identity := ensureTestGeneration(t, repo, "perf")
	record, err := readGenerationRecord(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Version != forkGenerationVersion || record.WorkspaceAnchor == "" ||
		record.WorkspaceDevice != 0 || record.WorkspaceInode != 0 ||
		record.WorkspaceBirthSec != 0 || record.WorkspaceBirthNsec != 0 {
		t.Fatalf("generation record retained reusable allocator identity: %+v", record)
	}
}

func TestForkGenerationAnchorPreservesHistoricallyValidLongNames(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	// This fits the historical atomic .generation-* writer but would overflow the v3 anchor if that
	// private filename repeated the human name.
	name := strings.Repeat("a", 230)
	if !ValidExistingName(name) {
		t.Fatal("long path-safe fork name was unexpectedly invalid")
	}
	makeGenerationWorkspace(t, repo, name)
	identity := ensureTestGeneration(t, repo, name)
	if err := ValidateGenerationWorkspace(repo, identity); err != nil {
		t.Fatalf("long-name generation was not usable: %v", err)
	}
	record, err := readGenerationRecord(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.WorkspaceAnchor) >= len(name) || strings.Contains(record.WorkspaceAnchor, name) {
		t.Fatalf("private anchor still embeds the human fork name: %q", record.WorkspaceAnchor)
	}
}

func TestForkGenerationMarkerStaysOutOfGitStatus(t *testing.T) {
	repo := committedSetupRepo(t)
	ws, err := Setup(repo, "clean")
	if err != nil {
		t.Fatal(err)
	}
	ensureTestGeneration(t, repo, "clean")
	status, err := gitOutputContext(context.Background(), ws, "status", "--porcelain")
	if err != nil || status != "" {
		t.Fatalf("anchored fork status = %q, %v; want clean", status, err)
	}
}

func TestForkGenerationNormalizesInterruptedSetupExclusions(t *testing.T) {
	repo := committedSetupRepo(t)
	name := "interrupted"
	ws := Workspace(repo, name)
	if err := os.MkdirAll(Home(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := GitClone(repo, ws); err != nil {
		t.Fatal(err)
	}
	if err := gitCheckoutNewBranchContext(context.Background(), ws, name); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".coop"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".coop", "partial"), []byte("state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ensureTestGeneration(t, repo, name)
	ensureTestGeneration(t, repo, name) // idempotent retry must not duplicate either rule
	status, err := gitOutputContext(context.Background(), ws, "status", "--porcelain")
	if err != nil || status != "" {
		t.Fatalf("adopted interrupted workspace status = %q, %v; want clean", status, err)
	}
	data, err := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{".coop/", "/" + GenerationMarkerName} {
		count := 0
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == pattern {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("exclude pattern %q count = %d, want 1:\n%s", pattern, count, data)
		}
	}
}

func TestForkGenerationMigratesOnlyStoppedSemanticallyVerifiedLegacyRecords(t *testing.T) {
	for _, version := range []int{forkGenerationLegacyVersion, forkGenerationBirthVersion} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			repo := committedSetupRepo(t)
			if _, err := Setup(repo, "legacy"); err != nil {
				t.Fatal(err)
			}
			legacy := writeLegacyGenerationRecord(t, repo, "legacy", version)
			if err := WriteWorkerState(repo, "legacy", WorkerState{Pid: 2147483646, Token: starttoken.Stale()}); err != nil {
				t.Fatal(err)
			}
			unlock, err := LockState(repo, "legacy")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := EnsureGenerationLocked(repo, "legacy"); err == nil {
				unlock()
				t.Fatal("active legacy generation was silently migrated")
			}
			if err := os.Remove(PidPath(repo, "legacy")); err != nil {
				unlock()
				t.Fatal(err)
			}
			identity, err := EnsureGenerationLocked(repo, "legacy")
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			if identity.Generation != legacy.Generation {
				t.Fatalf("migration changed logical generation: %s -> %s", legacy.Generation, identity.Generation)
			}
			record, err := readGenerationRecord(repo, "legacy")
			if err != nil || record.Version != forkGenerationVersion || record.WorkspaceAnchor == "" {
				t.Fatalf("migrated generation = %+v, %v", record, err)
			}
			if err := ValidateGenerationWorkspace(repo, identity); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestForkGenerationRefusesLegacyRepositorySemanticMismatch(t *testing.T) {
	repo := committedSetupRepo(t)
	ws, err := Setup(repo, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	legacy := writeLegacyGenerationRecord(t, repo, "legacy", forkGenerationLegacyVersion)
	gitIn(t, ws, "config", "remote.origin.url", filepath.Join(t.TempDir(), "other"))
	unlock, err := LockState(repo, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	_, migrateErr := EnsureGenerationLocked(repo, "legacy")
	unlock()
	if migrateErr == nil {
		t.Fatal("legacy generation with a different origin was migrated")
	}
	record, err := readGenerationRecord(repo, "legacy")
	if err != nil || record.Version != forkGenerationLegacyVersion || record.Generation != legacy.Generation {
		t.Fatalf("failed migration changed legacy authority: %+v, %v", record, err)
	}
}

func TestLegacyGenerationMigrationRetriesPrivateOnlyAnchor(t *testing.T) {
	repo := committedSetupRepo(t)
	if _, err := Setup(repo, "legacy"); err != nil {
		t.Fatal(err)
	}
	legacy := writeLegacyGenerationRecord(t, repo, "legacy", forkGenerationLegacyVersion)
	if err := EnsureStateDir(repo); err != nil {
		t.Fatal(err)
	}
	state, err := os.OpenRoot(StateDir(repo))
	if err != nil {
		t.Fatal(err)
	}
	anchored := generationRecord{Version: forkGenerationVersion, Name: legacy.Name,
		Generation: legacy.Generation, WorkspaceAnchor: forkGenerationAnchorName(legacy.Name, legacy.Generation)}
	binding := forkGenerationBinding(repo, anchored, state)
	marker, err := fsidentity.Create(binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := marker.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(Workspace(repo, "legacy"), GenerationMarkerName)); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockState(repo, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	identity, migrateErr := EnsureGenerationLocked(repo, "legacy")
	unlock()
	if migrateErr != nil || identity.Generation != legacy.Generation {
		t.Fatalf("private-only interrupted migration = %+v, %v", identity, migrateErr)
	}
	if err := ValidateGenerationWorkspace(repo, identity); err != nil {
		t.Fatal(err)
	}
}

func TestForkGenerationMigrationRefusesPoisonedCommonGitDirectory(t *testing.T) {
	repo := committedSetupRepo(t)
	ws, err := Setup(repo, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	legacy := writeLegacyGenerationRecord(t, repo, "legacy", forkGenerationLegacyVersion)
	victim := filepath.Join(t.TempDir(), "victim")
	gitIn(t, filepath.Dir(victim), "clone", "-q", repo, victim)
	gitIn(t, victim, "branch", "legacy")
	victimExclude := filepath.Join(victim, ".git", "info", "exclude")
	before, err := os.ReadFile(victimExclude)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git", "commondir"), []byte(filepath.Join(victim, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The poisoned view still satisfies the old semantic checks: its HEAD names the expected
	// branch and the sibling clone names the canonical project as its origin.
	if branch, err := gitOutputContext(context.Background(), ws, "symbolic-ref", "--quiet", "--short", "HEAD"); err != nil || branch != "legacy" {
		t.Fatalf("positive control branch = %q, %v", branch, err)
	}
	if origin, ok, err := gitConfigContext(context.Background(), ws, "remote.origin.url"); err != nil || !ok || filepath.Clean(origin) != filepath.Clean(repo) {
		t.Fatalf("positive control origin = %q, ok=%v, err=%v", origin, ok, err)
	}

	unlock, err := LockState(repo, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	_, migrateErr := EnsureGenerationLocked(repo, "legacy")
	unlock()
	if migrateErr == nil {
		t.Fatal("legacy migration followed a repository-controlled common Git directory")
	}
	after, err := os.ReadFile(victimExclude)
	if err != nil || string(after) != string(before) {
		t.Fatalf("legacy migration changed sibling exclude: %q, %v", after, err)
	}
	record, err := readGenerationRecord(repo, "legacy")
	if err != nil || record.Version != forkGenerationLegacyVersion || record.Generation != legacy.Generation {
		t.Fatalf("failed migration changed legacy authority: %+v, %v", record, err)
	}
}

func TestForkGenerationRecoversAnchorPublishedBeforeRecord(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "recover")
	if err := EnsureStateDir(repo); err != nil {
		t.Fatal(err)
	}
	state, err := os.OpenRoot(StateDir(repo))
	if err != nil {
		t.Fatal(err)
	}
	generation := Generation("fedcba9876543210fedcba9876543210")
	record := generationRecord{Version: forkGenerationVersion, Name: "recover", Generation: generation,
		WorkspaceAnchor: forkGenerationAnchorName("recover", generation), CreatedAt: time.Now().UTC()}
	root, err := fsidentity.Create(forkGenerationBinding(repo, record, state))
	if err != nil {
		_ = state.Close()
		t.Fatal(err)
	}
	_ = root.Close()
	_ = state.Close()
	identity := ensureTestGeneration(t, repo, "recover")
	if identity.Generation != generation {
		t.Fatalf("recovery minted %s, want published %s", identity.Generation, generation)
	}
	if err := ValidateGenerationWorkspace(repo, identity); err != nil {
		t.Fatal(err)
	}
}

func TestForkGenerationRemovalKeepsRecordUntilAnchorRetires(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "retire")
	identity := ensureTestGeneration(t, repo, "retire")
	record, err := readGenerationRecord(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(Workspace(repo, identity.Name)); err != nil {
		t.Fatal(err)
	}
	anchor := filepath.Join(StateDir(repo), record.WorkspaceAnchor)
	if err := os.Chmod(anchor, 0o644); err != nil {
		t.Fatal(err)
	}

	unlock, err := LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	removeErr := RemoveGenerationIfMatchesLocked(repo, identity)
	unlock()
	if removeErr == nil {
		t.Fatal("generation removal succeeded before its private anchor could retire")
	}
	if current, present, err := ReadGeneration(repo, identity.Name); err != nil || !present || current != identity {
		t.Fatalf("failed anchor retirement lost retry record: %+v, present=%v, err=%v", current, present, err)
	}

	if err := os.Chmod(anchor, 0o600); err != nil {
		t.Fatal(err)
	}
	unlock, err = LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	err = RemoveGenerationIfMatchesLocked(repo, identity)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, present, err := ReadGeneration(repo, identity.Name); err != nil || present {
		t.Fatalf("retry left generation record: present=%v, err=%v", present, err)
	}
	if _, err := os.Lstat(anchor); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry left private anchor: %v", err)
	}
}

func TestResolveProjectBindingRequiresExactGenerationWorkspace(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "perf")
	identity := ensureTestGeneration(t, repo, "perf")
	workspace := Workspace(repo, "perf")
	authority, bound, err := ResolveProjectBinding(workspace)
	if err != nil || authority != repo || bound == nil || *bound != identity {
		t.Fatalf("binding = %q %+v, %v", authority, bound, err)
	}
	plain := filepath.Join(t.TempDir(), "ordinary")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if authority, bound, err := ResolveProjectBinding(plain); err != nil || authority != plain || bound != nil {
		t.Fatalf("ordinary binding = %q %+v, %v", authority, bound, err)
	}
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	makeGenerationWorkspace(t, repo, "perf")
	if _, _, err := ResolveProjectBinding(workspace); err == nil {
		t.Fatal("replacement workspace inherited canonical binding")
	}
}

func TestForkGenerationAdoptsOnlyStoppedLegacyWorkspace(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "legacy")
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteWorkerState(repo, "legacy", WorkerState{Pid: 2147483646, Token: starttoken.Stale()}); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockState(repo, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureGenerationLocked(repo, "legacy"); err == nil {
		unlock()
		t.Fatal("legacy worker state was silently adopted")
	}
	if err := os.Remove(PidPath(repo, "legacy")); err != nil {
		unlock()
		t.Fatal(err)
	}
	if _, err := EnsureGenerationLocked(repo, "legacy"); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
}

func TestForkGenerationRecordFailsClosed(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "bad")
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(GenerationPath(repo, "bad"), []byte(`{"version":2,"name":"bad"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadGeneration(repo, "bad"); err == nil {
		t.Fatal("unsupported generation record was treated as absent")
	}
	if err := os.Remove(GenerationPath(repo, "bad")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", GenerationPath(repo, "bad")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadGeneration(repo, "bad"); err == nil {
		t.Fatal("symlinked generation record was accepted")
	}
}

func TestForkGenerationPublicationNeverReplacesExistingAuthority(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	path := GenerationPath(repo, "race")
	want := []byte("existing authority\n")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeGenerationAtomic(repo, "race", []byte("replacement\n")); err == nil {
		t.Fatal("generation publication replaced an existing authority path")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(want) {
		t.Fatalf("existing generation changed: %q, %v", got, err)
	}
}

func TestForkGenerationPublicationLeavesOneLinkAndNoTemporaryAuthority(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := writeGenerationAtomic(repo, "clean", []byte("authority\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(GenerationPath(repo, "clean"))
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		t.Fatalf("published authority link count = %v", stat)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("published authority mode = %04o, want 0600", got)
	}
	matches, err := filepath.Glob(filepath.Join(StateDir(repo), ".clean.generation-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary generation authorities remain: %v", matches)
	}
}

// A build before this one kept fork records in <repo>-forks/.coop. The first use moves them, once,
// by one rename, and the fork still validates: its anchor and marker are still one inode. A .coop
// planted afterwards is never read, and a link in its place is refused, not followed.
func TestForkRecordsMoveOnceFromTheForksFolder(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "perf")
	identity := ensureTestGeneration(t, repo, "perf")
	legacy := filepath.Join(Home(repo), ".coop")
	if err := os.Rename(recordsPath(repo), legacy); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGenerationWorkspace(repo, identity); err != nil {
		t.Fatalf("the moved records lost the fork: %v", err)
	}
	if _, err := os.Lstat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old records folder is still there: %v", err)
	}
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "planted.pid"), []byte("owner-v2\npid=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	names, err := LifecycleNames(repo)
	if err != nil || slices.Contains(names, "planted") {
		t.Fatalf("lifecycle names = %v, %v; a .coop planted after the move was read", names, err)
	}

	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(Home(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(Home(other), ".coop")); err != nil {
		t.Fatal(err)
	}
	if err := EnsureStateDir(other); err == nil || !strings.Contains(err.Error(), "not a real folder") {
		t.Fatalf("a linked old records folder = %v; want it refused", err)
	}
}
