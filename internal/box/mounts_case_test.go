package box

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// On a disk that ignores letter case, every spelling of a hidden file and of a .coopignore gets
// its own cover, and a name too long to cover refuses the launch unless its owner accepted that.
// On a disk that keeps case none of this applies, so the test asks the disk which it is.
func TestMountsCoverEverySpellingWhereTheDiskIgnoresCase(t *testing.T) {
	pinHome(t)
	repo := t.TempDir()
	for rel, body := range map[string]string{".env": "SECRET=inert\n", ".coopignore": "notes/\n", "src/app.go": "package app\n"} {
		writeRepoFile(t, filepath.Join(repo, rel), body)
	}
	folds := foldsCase(filepath.Join(repo, ".env"))
	mounts, err := ComputeMounts(repo, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	env, policy := find(mounts, "/workspace/.env"), find(mounts, "/workspace/.coopignore")
	if env == nil || policy == nil {
		t.Fatalf("no decoy or policy mount: %+v", mounts)
	}
	wantEnv, wantPolicy := 0, 0
	if folds {
		wantEnv, wantPolicy = 1<<3-1, 1<<10-1
	}
	if len(env.Spellings) != wantEnv || len(policy.Spellings) != wantPolicy {
		t.Fatalf("spellings: .env %d, .coopignore %d; want %d and %d on this disk (folds case: %v)",
			len(env.Spellings), len(policy.Spellings), wantEnv, wantPolicy, folds)
	}
	if got := ShadowCount(mounts); got != 1 {
		t.Fatalf("ShadowCount = %d, want 1: covers of other spellings are no extra hidden paths", got)
	}
	args := strings.Join(RenderMounts(mounts, "/decoy", "/decoys"), "\n")
	if folds && (!strings.Contains(args, "/decoy:/workspace/.ENV:ro") || !strings.Contains(args, "/decoy:/workspace/.eNv:ro")) {
		t.Fatalf("rendered mounts miss a spelling of .env:\n%s", args)
	}

	long := filepath.Join(repo, "config", "credentials.json")
	writeRepoFile(t, long, `{"token":"inert"}`)
	mounts, err = ComputeMounts(repo, "/workspace")
	var uncovered *UncoveredSpellings
	if folds != errors.As(err, &uncovered) {
		t.Fatalf("ComputeMounts with a long hidden name = %v; want a refusal exactly where the disk folds case (%v)", err, folds)
	}
	if !folds {
		return
	}
	if !slices.Equal(uncovered.Paths, []string{"config/credentials.json"}) || find(mounts, "/workspace/config/credentials.json") == nil {
		t.Fatalf("refusal = %+v, mounts %d; want the long name named and its decoy still planned", uncovered, len(mounts))
	}
	if _, exposed, err := launchMounts(&config.Config{}, repo, "/workspace"); err == nil || exposed != nil {
		t.Fatalf("launch without the setting = %v, %v; want the refusal", exposed, err)
	}
	if _, exposed, err := launchMounts(&config.Config{AllowCaseExposedSecrets: true}, repo, "/workspace"); err != nil || !slices.Equal(exposed, []string{"config/credentials.json"}) {
		t.Fatalf("launch with the setting = %v, %v; want it to start and name the file", exposed, err)
	}
}
