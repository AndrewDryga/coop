package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseLoopCommits(t *testing.T) {
	out := "a1\tadd json\ttask-json\n" +
		"b2\tfix egress\ttask-egress\n" +
		"c3\tmore json\ttask-json\n" +
		"d4\tmanual fixup\t\n"
	order, byTask, misc := parseLoopCommits(out)
	if want := []string{"task-json", "task-egress"}; !slices.Equal(order, want) {
		t.Errorf("order = %v, want %v (first-seen, deduped)", order, want)
	}
	if len(byTask["task-json"]) != 2 || byTask["task-json"][0].subject != "add json" {
		t.Errorf("task-json commits = %+v", byTask["task-json"])
	}
	if len(misc) != 1 || misc[0].subject != "manual fixup" {
		t.Errorf("misc = %+v, want the one untrailered commit", misc)
	}
}

func TestSubsystemsOf(t *testing.T) {
	got := subsystemsOf([]string{
		"internal/box/run.go", "internal/box/image.go", "internal/cli/loop.go",
		"site/index.html", "README.md",
	})
	want := []string{"(root)", "internal/box", "internal/cli", "site"}
	if !slices.Equal(got, want) {
		t.Errorf("subsystemsOf = %v, want %v", got, want)
	}
}

func TestReviewBlockAndHealth(t *testing.T) {
	cs := loopChangeSet{
		tasks: []taskChanges{
			{id: "task-json", commits: []commitInfo{{"a1", "add --json"}}, files: []string{"internal/cli/output.go"}},
			{id: "task-egress", commits: []commitInfo{{"b2", "fix egress"}}, files: []string{"internal/box/run.go", "Makefile"}},
		},
		subsystems: []string{"internal/box", "internal/cli"},
		stat:       " 3 files changed, 40 insertions(+)",
	}
	h := newLoopHealth()
	h.noteReopen([]string{"task-json"})                                 // reopened once
	h.noteIteration([]string{"task-egress"}, []string{"Makefile"}, nil) // edited a gate file
	block := cs.reviewBlock(h)
	for _, want := range []string{
		"task-json", "add --json", "internal/cli/output.go", "Affected areas: internal/box, internal/cli",
		"Look harder at", "signoff reopened it 1×", "edited gate file(s) Makefile",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("reviewBlock missing %q:\n%s", want, block)
		}
	}
	if (loopChangeSet{}).reviewBlock(newLoopHealth()) != "" {
		t.Error("an empty change set must render no review block")
	}
}

func TestSubstituteLoopVars(t *testing.T) {
	cs := loopChangeSet{
		tasks:      []taskChanges{{id: "task-json", commits: []commitInfo{{"a1", "add json"}}}},
		subsystems: []string{"internal/cli"},
	}
	out := substituteLoopVars("e2e {loop.affected}; tasks: {loop.tasks}", cs, newLoopHealth())
	if !strings.Contains(out, "e2e internal/cli") || !strings.Contains(out, "tasks: task-json") {
		t.Errorf("substituteLoopVars = %q", out)
	}
	if got := substituteLoopVars("plain prompt", cs, newLoopHealth()); got != "plain prompt" {
		t.Errorf("no-var prompt changed: %q", got)
	}
}

// TestLoopChangesFromGit exercises the git-backed path: real commits carrying Coop-Task trailers
// group by task (first-seen order), each task's files come from its own commits, an untrailered
// commit lands in misc, and the affected areas are the distinct top-level dirs.
func TestLoopChangesFromGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	base := gitOut(repo, "rev-parse", "HEAD")
	commit := func(path, body, msg string) {
		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", "-A")
		git(t, repo, "commit", "-qm", msg)
	}
	commit("internal/box/run.go", "package box\n", "box: a\n\nCoop-Task: task-a")
	commit("internal/box/image.go", "package box\n", "box: b\n\nCoop-Task: task-a")
	commit("internal/cli/x.go", "package cli\n", "cli: c\n\nCoop-Task: task-b")
	commit("README.md", "changed\n", "docs tweak, no trailer")

	cs := loopChanges(repo, base, gitOut(repo, "rev-parse", "HEAD"))
	if len(cs.tasks) != 2 || cs.tasks[0].id != "task-a" || cs.tasks[1].id != "task-b" {
		t.Fatalf("tasks (want task-a then task-b) = %+v", cs.tasks)
	}
	if len(cs.tasks[0].commits) != 2 {
		t.Errorf("task-a should carry 2 commits, got %+v", cs.tasks[0].commits)
	}
	if !slices.Equal(cs.tasks[0].files, []string{"internal/box/image.go", "internal/box/run.go"}) {
		t.Errorf("task-a files = %v", cs.tasks[0].files)
	}
	if len(cs.misc) != 1 || !strings.Contains(cs.misc[0].subject, "docs tweak") {
		t.Errorf("misc (want the untrailered tweak) = %+v", cs.misc)
	}
	if !slices.Equal(cs.subsystems, []string{"(root)", "internal/box", "internal/cli"}) {
		t.Errorf("subsystems = %v", cs.subsystems)
	}
	if head := gitOut(repo, "rev-parse", "HEAD"); !loopChanges(repo, head, head).empty() {
		t.Error("an empty range must yield an empty change set")
	}
}

func TestHumanDigest(t *testing.T) {
	cs := loopChangeSet{
		tasks:      []taskChanges{{id: "task-json", commits: []commitInfo{{"a1", "add --json flag"}}, files: []string{"internal/cli/output.go"}}},
		subsystems: []string{"internal/cli"},
	}
	h := newLoopHealth()
	h.noteReopen([]string{"task-json"})
	cost := runCost{
		byTask:  map[string]stageCost{"task-json": {usd: 3.50, inTok: 120000, outTok: 4200}},
		byModel: []modelSpend{{"claude:fable-5", stageCost{usd: 2.50}}, {"codex:gpt-5.6-terra", stageCost{usd: 1.00}}},
		total:   stageCost{usd: 3.50, inTok: 120000, outTok: 4200},
	}
	d := cs.humanDigest(h, []string{"sign-key-policy"}, cost)
	for _, want := range []string{"Shipped this run", "add --json flag", "$3.50", "Cost:", "120.0k in / 4.2k out", "by model:", "claude:fable-5 $2.50", "Blocked (needs you)", "sign-key-policy", "Look at:", "reopened 1×"} {
		if !strings.Contains(d, want) {
			t.Errorf("humanDigest missing %q:\n%s", want, d)
		}
	}
}
