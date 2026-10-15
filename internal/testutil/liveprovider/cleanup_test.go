package liveprovider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

type cleanupClock struct{ now time.Time }

func (c *cleanupClock) Now() time.Time { return c.now }
func (c *cleanupClock) Sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.now = c.now.Add(duration)
	return nil
}

func TestCleanupSupervisorRemovesCIDFilesAndWaitsForLateLabels(t *testing.T) {
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cidDir := filepath.Join(layout.State, "cids")
	mustMkdir(t, cidDir)
	writeRawResult(t, filepath.Join(cidDir, "version.cid"), []byte("version_1\n"))
	writeRawResult(t, filepath.Join(cidDir, "prompt.cid"), []byte("prompt.2\n"))
	clock := &cleanupClock{now: time.Unix(1, 0)}
	var ids []string
	labelResults := []int{0, 1, 0, 0, 0}
	labelCalls := 0
	err = CleanupSupervisor(context.Background(), SupervisorCleanupSpec{
		Root: layout.Root, CIDDir: cidDir, Supervisor: "supervisor", LabelKey: "coop.supervisor",
		OperationTimeout: time.Second, QuietPeriod: 2 * time.Second, PollInterval: time.Second,
	}, SupervisorCleanupOps{
		RemoveContainer: func(_ context.Context, id string) error {
			ids = append(ids, id)
			return errors.New("already removed")
		},
		RemoveByLabel: func(_ context.Context, key, value string) (int, error) {
			if key != "coop.supervisor" || value != "supervisor" {
				t.Fatalf("label sweep = %s=%s", key, value)
			}
			result := labelResults[labelCalls]
			labelCalls++
			return result, nil
		},
		Now: clock.Now, Sleep: clock.Sleep,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"version_1", "prompt.2"}) {
		t.Errorf("cid removals = %v", ids)
	}
	if labelCalls != len(labelResults) {
		t.Errorf("label calls = %d, want %d", labelCalls, len(labelResults))
	}
}

func TestCleanupSupervisorAcceptsOnlyExplicitSafeCIDPhases(t *testing.T) {
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cidDir := filepath.Join(layout.State, "cids")
	mustMkdir(t, cidDir)
	writeRawResult(t, filepath.Join(cidDir, "version-claude.cid"), []byte("version_claude\n"))
	writeRawResult(t, filepath.Join(cidDir, "edge-claude-codex.cid"), []byte("edge_codex\n"))
	clock := &cleanupClock{now: time.Unix(1, 0)}
	var ids []string
	err = CleanupSupervisor(context.Background(), SupervisorCleanupSpec{
		Root: layout.Root, CIDDir: cidDir, Supervisor: "supervisor", LabelKey: "label",
		Phases:           []string{"version-claude", "edge-claude-codex"},
		OperationTimeout: time.Second, QuietPeriod: time.Second, PollInterval: time.Second,
	}, SupervisorCleanupOps{
		RemoveContainer: func(_ context.Context, id string) error {
			ids = append(ids, id)
			return nil
		},
		RemoveByLabel: func(context.Context, string, string) (int, error) { return 0, nil },
		Now:           clock.Now, Sleep: clock.Sleep,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"version_claude", "edge_codex"}) {
		t.Fatalf("custom cid removals = %v", ids)
	}

	for _, phases := range [][]string{
		{"version", "version"}, {"../prompt"}, {"Prompt"}, {"-prompt"}, {"prompt-"}, {""},
	} {
		labelCalls := 0
		if err := CleanupSupervisor(context.Background(), SupervisorCleanupSpec{
			Root: layout.Root, CIDDir: cidDir, Supervisor: "supervisor", LabelKey: "label", Phases: phases,
			QuietPeriod: time.Millisecond, PollInterval: time.Millisecond,
		}, SupervisorCleanupOps{
			RemoveContainer: func(context.Context, string) error { return nil },
			RemoveByLabel: func(context.Context, string, string) (int, error) {
				labelCalls++
				return 0, nil
			},
		}); err == nil {
			t.Errorf("unsafe cleanup phases were accepted: %q", phases)
		}
		if labelCalls < 2 {
			t.Errorf("unsafe phase contract skipped authoritative label cleanup: %q", phases)
		}
	}
}

func TestCleanupSupervisorRequiresSweepStartedAfterQuietPeriod(t *testing.T) {
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clock := &cleanupClock{now: time.Unix(1, 0)}
	calls := 0
	err = CleanupSupervisor(context.Background(), SupervisorCleanupSpec{
		Root: layout.Root, CIDDir: layout.State, Supervisor: "supervisor", LabelKey: "label",
		OperationTimeout: time.Second, QuietPeriod: 2 * time.Second, PollInterval: time.Second,
	}, SupervisorCleanupOps{
		RemoveContainer: func(context.Context, string) error { return nil },
		RemoveByLabel: func(context.Context, string, string) (int, error) {
			calls++
			if calls == 2 {
				clock.now = clock.now.Add(2 * time.Second)
			}
			return 0, nil
		},
		Now: clock.Now, Sleep: clock.Sleep,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("cleanup accepted a slow sweep that started before the quiet boundary: %d calls", calls)
	}
}

func TestCleanupSupervisorIgnoresInvalidCIDFilesButStillSweepsLabel(t *testing.T) {
	tests := []struct {
		name  string
		write func(t *testing.T, layout procharness.Layout, path string)
	}{
		{name: "missing", write: func(*testing.T, procharness.Layout, string) {}},
		{name: "empty", write: func(t *testing.T, _ procharness.Layout, path string) { writeRawResult(t, path, nil) }},
		{name: "oversized", write: func(t *testing.T, _ procharness.Layout, path string) {
			writeRawResult(t, path, []byte(strings.Repeat("a", 257)))
		}},
		{name: "whitespace", write: func(t *testing.T, _ procharness.Layout, path string) { writeRawResult(t, path, []byte("bad id\n")) }},
		{name: "slash", write: func(t *testing.T, _ procharness.Layout, path string) { writeRawResult(t, path, []byte("bad/id\n")) }},
		{name: "symlink", write: func(t *testing.T, layout procharness.Layout, path string) {
			target := filepath.Join(layout.State, "target")
			writeRawResult(t, target, []byte("valid-id\n"))
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hardlink", write: func(t *testing.T, layout procharness.Layout, path string) {
			target := filepath.Join(layout.State, "target")
			writeRawResult(t, target, []byte("valid-id\n"))
			if err := os.Link(target, path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			layout, err := procharness.NewLayout(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cidDir := filepath.Join(layout.State, "cids")
			mustMkdir(t, cidDir)
			tt.write(t, layout, filepath.Join(cidDir, "version.cid"))
			clock := &cleanupClock{now: time.Unix(1, 0)}
			removedIDs := 0
			labelCalls := 0
			err = CleanupSupervisor(context.Background(), SupervisorCleanupSpec{
				Root: layout.Root, CIDDir: cidDir, Supervisor: "supervisor", LabelKey: "label",
				QuietPeriod: time.Second, PollInterval: time.Second,
			}, SupervisorCleanupOps{
				RemoveContainer: func(context.Context, string) error { removedIDs++; return nil },
				RemoveByLabel:   func(context.Context, string, string) (int, error) { labelCalls++; return 0, nil },
				Now:             clock.Now, Sleep: clock.Sleep,
			})
			if err != nil || removedIDs != 0 || labelCalls != 2 {
				t.Errorf("cleanup = err %v, cid removals %d, label calls %d", err, removedIDs, labelCalls)
			}
		})
	}
}

func TestCleanupSupervisorReportsLabelAndCancellationFailures(t *testing.T) {
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	baseSpec := SupervisorCleanupSpec{
		Root: layout.Root, CIDDir: layout.State, Supervisor: "supervisor", LabelKey: "label",
		OperationTimeout: 10 * time.Millisecond, QuietPeriod: time.Second, PollInterval: time.Second,
	}
	t.Run("label error", func(t *testing.T) {
		err := CleanupSupervisor(context.Background(), baseSpec, SupervisorCleanupOps{
			RemoveContainer: func(context.Context, string) error { return nil },
			RemoveByLabel:   func(context.Context, string, string) (int, error) { return 0, errors.New("query failed") },
		})
		if err == nil {
			t.Fatal("label error was accepted")
		}
	})
	t.Run("operation deadline", func(t *testing.T) {
		err := CleanupSupervisor(context.Background(), baseSpec, SupervisorCleanupOps{
			RemoveContainer: func(context.Context, string) error { return nil },
			RemoveByLabel:   func(ctx context.Context, _, _ string) (int, error) { <-ctx.Done(); return 0, ctx.Err() },
		})
		if err == nil {
			t.Fatal("operation timeout was accepted")
		}
	})
	t.Run("parent canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := CleanupSupervisor(ctx, baseSpec, SupervisorCleanupOps{
			RemoveContainer: func(context.Context, string) error { return nil },
			RemoveByLabel:   func(context.Context, string, string) (int, error) { return 0, nil },
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled cleanup = %v", err)
		}
	})
	t.Run("sleep canceled", func(t *testing.T) {
		err := CleanupSupervisor(context.Background(), baseSpec, SupervisorCleanupOps{
			RemoveContainer: func(context.Context, string) error { return nil },
			RemoveByLabel:   func(context.Context, string, string) (int, error) { return 0, nil },
			Sleep:           func(context.Context, time.Duration) error { return context.Canceled },
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sleep cancellation = %v", err)
		}
	})
}
