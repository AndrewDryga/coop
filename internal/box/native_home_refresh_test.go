package box

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
)

// A published home is refreshed at every launch, under its lock and without being seeded again: a new
// home is only seeded, and two launches of one home never refresh it at the same time. A refresh that
// fails stops the launch and names the home.
func TestPublishedNativeHomeIsRefreshedAtEveryLaunchUnderItsLock(t *testing.T) {
	cfg, repo := &config.Config{ConfigDir: t.TempDir()}, t.TempDir()
	var seeds, refreshes, inside, overlaps atomic.Int32
	seed := func(string) error { seeds.Add(1); return nil }
	refresh := func(string) error {
		refreshes.Add(1)
		if inside.Add(1) > 1 {
			overlaps.Add(1)
		}
		time.Sleep(2 * time.Millisecond)
		inside.Add(-1)
		return nil
	}
	home, err := prepareNativeHome(context.Background(), cfg, "claude", "work", repo, false, seed, refresh)
	if err != nil || seeds.Load() != 1 || refreshes.Load() != 0 {
		t.Fatalf("a new home: %q, %v, seeded %d times, refreshed %d; want it seeded once and not refreshed", home, err, seeds.Load(), refreshes.Load())
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := prepareNativeHome(context.Background(), cfg, "claude", "work", repo, false, seed, refresh); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if seeds.Load() != 1 || refreshes.Load() != 8 || overlaps.Load() != 0 {
		t.Fatalf("eight launches of a published home seeded it %d times, refreshed it %d times and overlapped %d times; want 1, 8 and 0",
			seeds.Load(), refreshes.Load(), overlaps.Load())
	}
	_, err = prepareNativeHome(context.Background(), cfg, "claude", "work", repo, false, seed, func(string) error { return errors.New("broken .claude.json") })
	if err == nil || !strings.Contains(err.Error(), "broken .claude.json") || !strings.Contains(err.Error(), home) {
		t.Fatalf("a failed refresh = %v, want it to stop the launch and name the home %s", err, home)
	}
}

// A fork is a new folder in its project's shared Claude home. Claude dropped the bypass acceptance from
// .claude.json on its first start, and the folder was never trusted, so the second launch and every new
// fork met the bypass warning or the trust prompt, which exit by default. Each launch puts back what is
// missing and leaves the rest of the file as Claude wrote it, rewriting nothing when nothing is missing.
func TestANewForkTrustsItsFolderInItsProjectsClaudeHome(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	home, err := PrepareNativeHome(context.Background(), cfg, "claude", "work", repo, false)
	if err != nil {
		t.Fatal(err)
	}
	claudeJSON := filepath.Join(home, ".claude.json")
	readState := func() map[string]any {
		t.Helper()
		data, err := os.ReadFile(claudeJSON)
		if err != nil {
			t.Fatal(err)
		}
		var state map[string]any
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	// What Claude leaves after its first start: the acceptance it moved to its settings is gone.
	first, err := json.Marshal(map[string]any{
		"hasCompletedOnboarding": true, "migrationVersion": 14, "numStartups": 3,
		"projects": map[string]any{repo: map[string]any{"hasTrustDialogAccepted": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeJSON, first, 0o600); err != nil {
		t.Fatal(err)
	}

	workspace, _ := newForkWorkspace(t, repo, "qa1")
	again, err := PrepareNativeHome(context.Background(), cfg, "claude", "work", workspace, false)
	if err != nil || again != home {
		t.Fatalf("a fork's home = %q, %v; want its project's home %q", again, err, home)
	}
	state := readState()
	projects, _ := state["projects"].(map[string]any)
	fork, _ := projects[workspace].(map[string]any)
	parent, _ := projects[repo].(map[string]any)
	if state["bypassPermissionsModeAccepted"] != true || fork["hasTrustDialogAccepted"] != true || parent["hasTrustDialogAccepted"] != true {
		t.Errorf("the second launch is not pre-answered: %v", state)
	}
	if state["numStartups"] != float64(3) || state["migrationVersion"] != float64(14) {
		t.Errorf("the refresh changed what Claude wrote: %v", state)
	}

	before, err := os.Stat(claudeJSON)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareNativeHome(context.Background(), cfg, "claude", "work", workspace, false); err != nil {
		t.Fatal(err)
	}
	if after, err := os.Stat(claudeJSON); err != nil || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("a launch with nothing missing rewrote .claude.json (%v)", err)
	}
	// Claude drops it again at its next start: the next launch, even from a second fork, restores it.
	if err := os.WriteFile(claudeJSON, first, 0o600); err != nil {
		t.Fatal(err)
	}
	other, _ := newForkWorkspace(t, repo, "qa2")
	if _, err := PrepareNativeHome(context.Background(), cfg, "claude", "work", other, false); err != nil {
		t.Fatal(err)
	}
	state = readState()
	projects, _ = state["projects"].(map[string]any)
	second, _ := projects[other].(map[string]any)
	if state["bypassPermissionsModeAccepted"] != true || second["hasTrustDialogAccepted"] != true {
		t.Errorf("a second new fork is not pre-answered: %v", state)
	}
}
