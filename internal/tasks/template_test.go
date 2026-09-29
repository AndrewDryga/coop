package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func writeQueueTemplate(t *testing.T, root, source string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("# Tasks\n\n## task.md — the spec\n\n**Template:**\n\n")
	for _, line := range strings.Split(strings.TrimSuffix(source, "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	b.WriteString("\n**Example:**\n\n    not the template\n")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestQueueTemplateDefaultsAndStructuredSubtasks(t *testing.T) {
	root := t.TempDir()
	source := strings.Replace(fallbackTaskTemplate,
		"- [ ] <a small step with a way to check it worked>",
		"- [ ] Run the local gate\n- [ ] <task-specific step>\n- [ ] Review the diff", 1)
	writeQueueTemplate(t, root, source)
	if code, err := tasksFolderAdd(root, []string{"Unfilled"}, StateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("unfilled add: %d, %v", code, err)
	}
	first := readFileString(filepath.Join(mustReadTaskTree(t, root)[0].Dir, "task.md"))
	for _, want := range []string{"Run the local gate", "<task-specific step>", "Review the diff"} {
		if !strings.Contains(first, want) {
			t.Errorf("scaffold lost %q", want)
		}
	}
	if code, err := tasksFolderAdd(root, []string{"Filled", "--context", "why", "--acceptance", "proof", "--approach", "how", "--subtask", "first", "--subtask", "second"}, StateBacklog, "backlog add"); code != 0 || err != nil {
		t.Fatalf("structured backlog add: %d, %v", code, err)
	}
	second := readFileString(filepath.Join(mustReadBacklog(t, root)[0].Dir, "task.md"))
	for _, want := range []string{"- [ ] Run the local gate", "- [ ] first", "- [ ] second", "- [ ] Review the diff"} {
		if !strings.Contains(second, want) {
			t.Errorf("structured task lost %q", want)
		}
	}
	if strings.Contains(second, "<task-specific step>") || strings.Contains(second, "not the template") {
		t.Errorf("structured task kept template-only text:\n%s", second)
	}
}

func TestQueueTemplateMalformedRefusesBeforeCreating(t *testing.T) {
	root := t.TempDir()
	writeQueueTemplate(t, root, strings.Replace(fallbackTaskTemplate, "**Approach:**", "**Plan:**", 1))
	code, err := tasksFolderAdd(root, []string{"Do work"}, StateTodo, "tasks add")
	if code == 0 || err == nil || !strings.Contains(err.Error(), filepath.Join(root, "README.md")) || !strings.Contains(err.Error(), "Approach") {
		t.Fatalf("malformed template: code=%d err=%v", code, err)
	}
	if _, err := os.Stat(filepath.Join(root, StateTodo)); !os.IsNotExist(err) {
		t.Fatalf("malformed template created state dirs: %v", err)
	}
}

func TestQueueReadmeWithoutTemplateMarkerUsesBuiltInStarter(t *testing.T) {
	root := t.TempDir()
	readme := "# Tasks\n\nThis queue has its own instructions.\n\n## task.md example\n\n```md\n# An example, not a template\n```\n"
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(readme), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksFolderAdd(root, []string{"Fallback task"}, StateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("README without marker blocked task creation: code=%d err=%v", code, err)
	}
	items := mustReadTaskTree(t, root)
	if len(items) != 1 {
		t.Fatalf("created %d tasks, want one", len(items))
	}
	body := readFileString(filepath.Join(items[0].Dir, "task.md"))
	if !strings.Contains(body, "**Context:** <the problem, why it matters, and where it happens>\n\n**Acceptance criteria:**") ||
		!strings.Contains(body, "**Approach:** <the steps to take; use spec.md for a longer plan>\n\n## Subtasks") ||
		!strings.Contains(body, "- [ ] <a small step with a way to check it worked>") {
		t.Fatalf("task did not use the built-in starter:\n%s", body)
	}
}

func TestQueueTemplateSelectionAndSafeTitle(t *testing.T) {
	repo := t.TempDir()
	root := filepath.Join(repo, ".agent", "tasks")
	member := filepath.Join(repo, "web", ".agent", "tasks")
	override := filepath.Join(repo, "custom", "tasks")
	for _, tc := range []struct{ root, gate string }{{root, "root gate"}, {member, "web gate"}, {override, "custom gate"}} {
		writeQueueTemplate(t, tc.root, strings.Replace(fallbackTaskTemplate, "- [ ] <a small step with a way to check it worked>", "- [ ] "+tc.gate, 1))
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("subprojects:\n  - web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args       []string
		root, gate string
	}{
		{[]string{"add", "--project", "root", "root title"}, root, "root gate"},
		{[]string{"add", "--project", "web", "web title"}, member, "web gate"},
		{[]string{"add", "--tasks", "custom/tasks", "custom title"}, override, "custom gate"},
	} {
		if code, err := CmdTasks(Host{}, &config.Config{RepoOverride: repo}, tc.args); code != 0 || err != nil {
			t.Fatalf("CmdTasks(%v): %d, %v", tc.args, code, err)
		}
		items := mustReadTaskTree(t, tc.root)
		if len(items) != 1 {
			t.Fatalf("%s: %d tasks", tc.root, len(items))
		}
		body := readFileString(filepath.Join(items[0].Dir, "task.md"))
		if !strings.Contains(body, tc.gate) {
			t.Errorf("%s did not use its template", tc.root)
		}
	}
	quoted := `Fix: "quoted" title`
	if code, err := tasksFolderAdd(root, []string{quoted}, StateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("quoted title: %d, %v", code, err)
	}
	items := mustReadTaskTree(t, root)
	if len(items) != 2 || (items[0].Title != quoted && items[1].Title != quoted) {
		t.Errorf("quoted title round-trip: %+v", items)
	}
	if code, err := tasksFolderAdd(root, []string{"bad\nheader"}, StateTodo, "tasks add"); code == 0 || err == nil {
		t.Error("newline title must be refused")
	}
}

func TestShippedTaskReadmesAreUsableTemplates(t *testing.T) {
	for _, root := range []string{"../../.agent/tasks", "../scaffold/templates/agent/tasks"} {
		template, err := loadTaskTemplate(root)
		if err != nil {
			t.Fatalf("%s: %v", root, err)
		}
		body, err := template.render("2026-01-01-test", "Test task", "2026-01-01T00:00:00Z", nil, nil)
		if err != nil || !strings.Contains(body, "- [ ] <a small step with a way to check it worked>") {
			t.Errorf("%s: rendered %q, %v", root, body, err)
		}
	}
}

func TestDraftTaskUsesQueueTemplate(t *testing.T) {
	root := t.TempDir()
	writeQueueTemplate(t, root, strings.Replace(fallbackTaskTemplate,
		"- [ ] <a small step with a way to check it worked>", "- [ ] Draft gate\n- [ ] <task-specific step>", 1))
	item, err := CreateDraftTask(root, TaskDraft{Kind: ForkProposalTask, Title: "Draft work", Context: "why", Acceptance: "proof", Approach: "how", Subtasks: []string{"test it"}})
	if err != nil {
		t.Fatal(err)
	}
	body := readFileString(filepath.Join(item.Dir, "task.md"))
	if !strings.Contains(body, "- [ ] Draft gate") || !strings.Contains(body, "- [ ] test it") {
		t.Errorf("draft lost queue template: %s", body)
	}
}
