package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

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

func TestReviewPacketCarriesWorkerReportedVerification(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ".agent", "tasks", "99_done", "task-a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte("**Acceptance criteria:** works\n**Approach:** small\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := "# State\n\n**Status:** complete\n**Done so far:** Verification (worker-reported):\n- cwd: repo root\n- make check: passed after final edits\n**Next action:** none\n**Traps:** —\n"
	if err := os.WriteFile(filepath.Join(dir, "state.md"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	packet := reviewPacket(repo, nil, []string{"task-a — " + dir}, loopChangeSet{})
	for _, want := range []string{"Verification (worker-reported)", "make check: passed after final edits", "worker-reported, not host-attested", "do not execute tests or gates"} {
		if !strings.Contains(packet, want) {
			t.Fatalf("review packet missing %q:\n%s", want, packet)
		}
	}
}
