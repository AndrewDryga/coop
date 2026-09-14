package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestReviewGateReceiptReusesOnlyExactSuccessfulProof(t *testing.T) {
	repo, git := gitrepo.New(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("gate: make check\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".agent/project.yaml")
	git("commit", "-qm", "base")

	runs, exit := 0, 0
	var runtimeErr error
	c := New(&config.Config{}, runtime.Runtime{}, "test", Host{})
	c.runID = "receipt-test"
	c.net = newNetworkLog()
	c.boxRun = func(spec box.RunSpec) (int, error) {
		runs++
		if spec.Repo == repo || spec.PolicyRepo != repo || !spec.Review {
			t.Fatalf("gate spec = repo %q policy %q review %v", spec.Repo, spec.PolicyRepo, spec.Review)
		}
		if err := os.WriteFile(filepath.Join(spec.Repo, "generated-by-gate"), []byte("scratch"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintln(spec.Stdout, "focused gate output")
		return exit, runtimeErr
	}

	first := c.reviewGateReceipt(context.Background(), repo, "image:one", "base")
	second := c.reviewGateReceipt(context.Background(), repo, "image:one", "base")
	if first.Exit != 0 || first.Reused || !second.Reused || runs != 1 {
		t.Fatalf("successful receipts = first %#v second %#v runs %d", first, second, runs)
	}
	if _, err := os.Stat(filepath.Join(repo, "generated-by-gate")); !os.IsNotExist(err) {
		t.Fatalf("scratch gate changed host checkout: %v", err)
	}
	if !strings.Contains(first.Summary, "focused gate output") || first.Log == "" {
		t.Fatalf("receipt lost bounded/full evidence: %#v", first)
	}

	changedImage := c.reviewGateReceipt(context.Background(), repo, "image:two", "base")
	if changedImage.Reused || runs != 2 {
		t.Fatalf("changed image reused receipt: %#v runs %d", changedImage, runs)
	}

	exit = 7
	failed := c.reviewGateReceipt(context.Background(), repo, "image:one", "other-base")
	again := c.reviewGateReceipt(context.Background(), repo, "image:one", "other-base")
	if failed.Exit != 7 || again.Reused || runs != 4 {
		t.Fatalf("failed receipt was reused: first %#v second %#v runs %d", failed, again, runs)
	}

	exit = 0
	runtimeErr = errors.New("runtime unavailable")
	failedRuntime := c.reviewGateReceipt(context.Background(), repo, "image:one", "runtime-error")
	runtimeErr = nil
	retriedRuntime := c.reviewGateReceipt(context.Background(), repo, "image:one", "runtime-error")
	if failedRuntime.Exit == 0 || retriedRuntime.Reused || runs != 6 {
		t.Fatalf("runtime-error receipt was reused: first %#v second %#v runs %d", failedRuntime, retriedRuntime, runs)
	}
}

func TestReviewGateKeyBindsEveryIdentity(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(repo, ".agent", "project.yaml")
	if err := os.WriteFile(configPath, []byte("gate: make check\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := reviewGateKey(repo, "tree", "base", "image", []string{"make", "check"})
	for name, key := range map[string]string{
		"tree":    reviewGateKey(repo, "other", "base", "image", []string{"make", "check"}),
		"base":    reviewGateKey(repo, "tree", "other", "image", []string{"make", "check"}),
		"image":   reviewGateKey(repo, "tree", "base", "other", []string{"make", "check"}),
		"command": reviewGateKey(repo, "tree", "base", "image", []string{"make", "test"}),
	} {
		if key == base {
			t.Errorf("%s did not change gate key", name)
		}
	}
	if err := os.WriteFile(configPath, []byte("gate: make test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if key := reviewGateKey(repo, "tree", "base", "image", []string{"make", "check"}); key == base {
		t.Error("project config did not change gate key")
	}
}

func TestReviewPacketIncludesBoundedExactCommitDiff(t *testing.T) {
	repo, git := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(repo, "source.go"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "source.go")
	git("commit", "-qm", "base")
	if err := os.WriteFile(filepath.Join(repo, "source.go"), []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "source.go")
	git("commit", "-qm", "change")
	sha := gitOut(repo, "rev-parse", "--short", "HEAD")
	packet := reviewPacket(repo, nil, nil, loopChangeSet{tasks: []taskChanges{{
		id: "task-a", commits: []commitInfo{{sha: sha, subject: "change"}}, files: []string{"source.go"},
	}}})
	for _, want := range []string{"BEGIN UNTRUSTED EXACT COMMIT DIFF", "-before", "+after", "END UNTRUSTED EXACT COMMIT DIFF"} {
		if !strings.Contains(packet, want) {
			t.Fatalf("review packet missing %q:\n%s", want, packet)
		}
	}
	if len(packet) > 16000 {
		t.Fatalf("review packet grew beyond its bound: %d", len(packet))
	}
}
