package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
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
	sha := forkspace.GitOut(repo, "rev-parse", "--short", "HEAD")
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

// Coop's host re-signing gives a commit a new id but keeps its tree, so verification a worker
// recorded at the unsigned commit still covers the signed one. The review packet maps the two, and
// only when the trees match: a re-sign entry whose copy holds another tree maps nothing.
func TestReviewPacketMapsReSignedCommitsByTree(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	if err := os.WriteFile(filepath.Join(repo, "source.go"), []byte("tested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "source.go")
	git("commit", "-q", "-m", "work")
	head := func() string { return forkspace.GitOut(repo, "rev-parse", "HEAD") }
	tested, tree := head(), forkspace.GitOut(repo, "rev-parse", "HEAD^{tree}")
	// What signUnpushed applies: the branch moves to a copy of the same tree under its reflog message.
	resign := func(from string, change func()) string {
		change()
		git("commit", "-q", "--amend", "-m", "work, signed")
		copied := head()
		git("update-ref", "refs/heads/main", from)
		git("update-ref", "-m", forkspace.ResignReflogMessage, "refs/heads/main", copied, from)
		return copied
	}
	signed := resign(tested, func() {})
	mapped := tested[:12] + " → " + signed[:12] + " (tree " + tree[:12] + ")"
	if packet := reviewPacket(repo, nil, nil, loopChangeSet{}); !strings.Contains(packet, mapped) {
		t.Fatalf("review packet does not map the tested commit to its signed copy %q:\n%s", mapped, packet)
	}

	forged := resign(signed, func() {
		if err := os.WriteFile(filepath.Join(repo, "source.go"), []byte("untested\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "source.go")
	})
	packet := reviewPacket(repo, nil, nil, loopChangeSet{})
	if strings.Contains(packet, signed[:12]+" → "+forged[:12]) {
		t.Fatalf("review packet maps a copy with another tree:\n%s", packet)
	}
	if !strings.Contains(packet, mapped) {
		t.Fatalf("review packet lost the earlier mapping:\n%s", packet)
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
