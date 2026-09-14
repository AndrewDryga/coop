package loop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/loopcfg"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestReviewRepoReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		writes   loopcfg.ReviewWrites
		readOnly bool
	}{
		{name: "default", readOnly: true},
		{name: "explicit tasks", writes: loopcfg.ReviewWritesTasks, readOnly: true},
		{name: "explicit repository", writes: loopcfg.ReviewWritesRepo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if readOnly := reviewRepoReadOnly(tc.writes); readOnly != tc.readOnly {
				t.Errorf("reviewRepoReadOnly(%q) = %v, want %v", tc.writes, readOnly, tc.readOnly)
			}
		})
	}
}

func TestReadOnlyReviewFallbackReusesPreparedServices(t *testing.T) {
	repo := t.TempDir()
	task := taskForLease(t, repo, stateDone, "task-a")
	c := New(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}, runtime.Runtime{Name: "true"}, "test", Host{})
	c.net = newNetworkLog()
	c.runID = "service-reuse-test"
	var launches []box.RunSpec
	c.boxRun = func(spec box.RunSpec) (int, error) {
		launches = append(launches, spec)
		if len(launches) == 1 {
			_, _ = fmt.Fprintln(spec.Stderr, "rate limit exceeded")
			return 1, nil
		}
		_, _ = fmt.Fprintln(spec.Stdout, "review complete")
		return 0, nil
	}
	command := func(_ string, _ string, _ string, _ bool) ([]string, bool, bool) {
		return []string{"review-fixture"}, false, false
	}
	rot := ladder.NewRotation([]agents.Target{
		{Provider: "claude", Accounts: []string{"first"}},
		{Provider: "claude", Accounts: []string{"second"}},
	})
	if _, err := c.runReview(context.Background(), repo, "image", rot, "", "review", "signoff", command,
		[]string{repo}, []string{task.ID}, nil, loopcfg.ReviewWritesTasks, io.Discard, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(launches) != 2 || launches[0].ReuseServices || launches[0].Quiet || !launches[0].LoopPresentation ||
		!launches[1].ReuseServices || !launches[1].Quiet || launches[1].LoopPresentation {
		t.Fatalf("review fallback launches = %+v, want narrated setup once then prepared-service reuse", launches)
	}
}

func TestWorkCredentialFallbackReusesServicesOnlyFromCleanUnchangedTree(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(repo, "source.go"), []byte("package source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "source.go")
	git("commit", "-qm", "base")
	head := gitOut(repo, "rev-parse", "HEAD")

	if !cleanCredentialRetryCanReuseServices(repo, head, head, "rate_limit") ||
		!cleanCredentialRetryCanReuseServices(repo, head, head, "authentication") {
		t.Fatal("clean credential fallback did not reuse prepared services")
	}
	if cleanCredentialRetryCanReuseServices(repo, head, head, "process_failure") ||
		cleanCredentialRetryCanReuseServices(repo, head, head+"changed", "rate_limit") {
		t.Fatal("non-credential or changed-HEAD retry reused prepared services")
	}
	if err := os.WriteFile(filepath.Join(repo, "source.go"), []byte("package changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cleanCredentialRetryCanReuseServices(repo, head, head, "rate_limit") {
		t.Fatal("dirty credential fallback reused prepared services")
	}
}

func TestReviewVerdictUsesOneExactSessionCorrection(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".agent/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "source.go"), []byte("package source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".gitignore", "source.go")
	git("commit", "-qm", "base")
	task := taskForLease(t, repo, stateDone, "task-a")

	c := New(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}, runtime.Runtime{Name: "true"}, "test", Host{})
	c.net = newNetworkLog()
	c.runID = "correction-test"
	launches := 0
	c.boxRun = func(spec box.RunSpec) (int, error) {
		launches++
		if spec.OnRuntimeLaunch != nil {
			spec.OnRuntimeLaunch()
		}
		verdict := "AUDIT EVIDENCE — task-a — gate: focused tests passed — findings: none\nREVIEW COMPLETE — PASS — reopened: none"
		if !spec.FormatCorrection {
			verdict += "\ntrailing prose"
		} else if spec.Preset != nil || len(spec.Peers) != 0 || spec.TaskTools != nil {
			t.Fatal("format correction repeated preset peers or task tooling")
		}
		_, _ = fmt.Fprintf(spec.Stdout, "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n", verdict)
		_, _ = fmt.Fprintln(spec.Stdout, `{"type":"result","subtype":"success","num_turns":1,"duration_ms":1}`)
		return 0, nil
	}
	type commandCall struct {
		prompt, id string
		resume     bool
	}
	var commands []commandCall
	command := func(_ string, prompt, id string, resume bool) ([]string, bool, bool) {
		commands = append(commands, commandCall{prompt: prompt, id: id, resume: resume})
		return []string{"claude", "-p", prompt}, true, true
	}
	rot := ladder.NewRotation([]agents.Target{{Provider: "claude", Accounts: []string{"test"}}})
	observed := 0
	run, err := c.runReviewVerdict(context.Background(), repo, "image", rot, "", "review", "signoff", command,
		[]string{repo}, []string{task.ID}, nil, loopcfg.ReviewWritesTasks, io.Discard, nil, nil,
		func(reviewRunResult, time.Time, string) { observed++ })
	if err != nil {
		t.Fatalf("corrected review: %v\n%s", err, run.output)
	}
	if launches != 2 || len(commands) != 2 || commands[0].resume || !commands[1].resume || commands[0].id == "" || commands[1].id != commands[0].id {
		t.Fatalf("review/correction launches = %d commands = %+v", launches, commands)
	}
	if !strings.Contains(commands[1].prompt, "Validation error:") || !strings.Contains(commands[1].prompt, "AUDIT EVIDENCE — task-a") {
		t.Fatalf("correction prompt lacks precise repair data:\n%s", commands[1].prompt)
	}
	if observed != 1 || run.retries != 1 {
		t.Fatalf("recovery episode observer/retries = %d/%d, want 1/1", observed, run.retries)
	}
	if current, ok, err := tasks.CurrentTask(repo, task.ID); err != nil || !ok || current.State != stateDone {
		t.Fatalf("passing correction changed completed task: %+v %v %v", current, ok, err)
	}
}

func TestReviewVerdictRejectsSourceChangeBeforeCorrection(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".agent/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(repo, "source.go")
	if err := os.WriteFile(source, []byte("package source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".gitignore", "source.go")
	git("commit", "-qm", "base")
	task := taskForLease(t, repo, stateDone, "task-a")

	c := New(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}, runtime.Runtime{Name: "true"}, "test", Host{})
	c.net = newNetworkLog()
	c.runID = "source-change-test"
	launches := 0
	c.boxRun = func(spec box.RunSpec) (int, error) {
		launches++
		if err := os.WriteFile(source, []byte("package changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintln(spec.Stdout, `{"type":"assistant","message":{"content":[{"type":"text","text":"invalid"}]}}`)
		_, _ = fmt.Fprintln(spec.Stdout, `{"type":"result","subtype":"success","num_turns":1,"duration_ms":1}`)
		return 0, nil
	}
	command := func(_ string, prompt, id string, resume bool) ([]string, bool, bool) {
		return []string{"claude", "-p", prompt}, true, true
	}
	rot := ladder.NewRotation([]agents.Target{{Provider: "claude", Accounts: []string{"test"}}})
	_, err := c.runReviewVerdict(context.Background(), repo, "image", rot, "", "review", "signoff", command,
		[]string{repo}, []string{task.ID}, nil, loopcfg.ReviewWritesTasks, io.Discard, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "review source changed") {
		t.Fatalf("source-changing review error = %v", err)
	}
	if launches != 1 {
		t.Fatalf("source-changing malformed review launched %d corrections, want 0", launches-1)
	}
}

func TestReviewSourceSnapshotRejectsHeadAndTrackedChanges(t *testing.T) {
	repo, git := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(repo, "source.go"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "source.go")
	git("commit", "-qm", "base")
	snapshot, err := snapshotReviewSource(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateReviewSource(repo, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "source.go"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateReviewSource(repo, snapshot); err == nil {
		current, _ := snapshotReviewSource(repo)
		t.Fatalf("tracked source change preserved a stale review snapshot: before %#v after %#v", snapshot, current)
	}
	git("add", "source.go")
	git("commit", "-qm", "changed")
	if err := validateReviewSource(repo, snapshot); err == nil {
		t.Fatal("HEAD change preserved a stale review snapshot")
	}
	if err := validateReviewSourceAfterConcurrent(repo, snapshot, true); err != nil {
		t.Fatalf("host-observed concurrent commit rejected: %v", err)
	}
}

func TestReviewVerdictCorrectionDropsControlBytes(t *testing.T) {
	got := reviewVerdictCorrection(errors.New("invalid\x00receipt"), "answer\x00tail", []string{"task-a"})
	if strings.ContainsRune(got, '\x00') || !strings.Contains(got, "invalidreceipt") || !strings.Contains(got, "answertail") {
		t.Fatalf("unsafe correction prompt %q", got)
	}
}

// TestReviewLadder: a review stage's ladder keeps each rung's PROVIDER, model, effort, and the
// fallback rungs — the fix for stepModel, which kept only (model, effort) off the first rung and
// dropped the provider, so a claude-led run's `codex:…` signoff resolved to `claude --model
// <a-codex-model>` and the cross-vendor reviewer was never actually run.
func TestReviewLadder(t *testing.T) {
	ladder, err := reviewLadder([]string{"codex:gpt-5.6-sol/xhigh", "claude:claude-fable-5/xhigh"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ladder) != 2 {
		t.Fatalf("both rungs must survive (the fallback too), got %d", len(ladder))
	}
	// Rung 0 keeps its provider — NOT discarded onto the work provider.
	if ladder[0].Provider != "codex" || ladder[0].Model != "gpt-5.6-sol" || ladder[0].Effort != "xhigh" {
		t.Errorf("rung 0 = %+v, want codex / gpt-5.6-sol / xhigh", ladder[0])
	}
	// Rung 1 (the fallback) survives with its own provider — stepModel dropped it entirely.
	if ladder[1].Provider != "claude" || ladder[1].Model != "claude-fable-5" {
		t.Errorf("rung 1 = %+v, want claude / claude-fable-5", ladder[1])
	}
	// An empty ladder yields no rungs — the caller falls back to the work rotation.
	if got, _ := reviewLadder(nil); len(got) != 0 {
		t.Errorf("empty ladder → no rungs, got %v", got)
	}
}

func TestReviewSubjectSnapshotsRejectLifecycleGenerationChange(t *testing.T) {
	root := t.TempDir()
	task := taskForLease(t, root, stateDone, "task-a")
	snapshots, err := snapshotReviewSubjects([]string{root}, []string{task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateReviewSubjects([]string{root}, snapshots); err != nil {
		t.Fatalf("unchanged review subject failed validation: %v", err)
	}

	oldDir := filepath.Join(t.TempDir(), task.ID)
	if err := os.Rename(task.Dir, oldDir); err != nil {
		t.Fatal(err)
	}
	taskForLease(t, root, stateDone, task.ID)
	if err := validateReviewSubjects([]string{root}, snapshots); err == nil ||
		!strings.Contains(err.Error(), "changed completion generation") {
		t.Fatalf("replacement review subject validation = %v, want generation change", err)
	}
}
