package loop

import (
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// A loop in a shallow clone, which no completion could pass, is refused before any agent runs,
// with the reason and the way on (results/forks.md, F1). A repository with full history starts.
func TestLoopRefusesAShallowCloneBeforeAnyAgent(t *testing.T) {
	src, run := gitrepo.New(t)
	run("commit", "-q", "--allow-empty", "-m", "c1")
	run("commit", "-q", "--allow-empty", "-m", "c2")
	if err := RequireHead(src); err != nil {
		t.Fatalf("a full repository was refused: %v", err)
	}
	clone, _ := gitrepo.ShallowClone(t, src, 1)
	head := forkspace.GitOut(clone, "rev-parse", "HEAD")
	for _, fork := range []string{"", "f"} {
		var err error
		notes := captureStderr(t, func() { err = RefuseShallowHistory(clone, head, fork) })
		want := []string{"This checkout is a shallow clone", "No agent ran", "git fetch --unshallow"}
		if fork != "" {
			want = []string{"Fork f was made from a shallow clone", "No agent ran", "git fetch --unshallow", "coop fork rm f --force"}
		}
		if err == nil {
			t.Fatalf("fork %q: a shallow clone was not refused", fork)
		}
		for _, text := range want {
			if !strings.Contains(notes, text) {
				t.Errorf("fork %q: refusal lacks %q:\n%s", fork, text, notes)
			}
		}
	}
	if err := RequireHead(clone); err == nil {
		t.Fatal("RequireHead let a shallow clone through")
	}
}
