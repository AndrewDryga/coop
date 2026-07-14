package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSweepQueueGuard(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	hookBytes, err := templates.ReadFile("templates/skills/sweep/queue-guard.sh")
	if err != nil {
		t.Fatal(err)
	}
	skill, err := templates.ReadFile("templates/skills/sweep/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(skill), "---", 3)
	if len(parts) != 3 {
		t.Fatal("sweep SKILL.md has no YAML frontmatter")
	}
	var frontmatter struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `yaml:"type"`
				Command string `yaml:"command"`
			} `yaml:"hooks"`
		} `yaml:"hooks"`
	}
	if err := yaml.Unmarshal([]byte(parts[1]), &frontmatter); err != nil {
		t.Fatalf("parse sweep frontmatter: %v", err)
	}
	stop := frontmatter.Hooks["Stop"]
	if len(stop) != 1 || len(stop[0].Hooks) != 1 || stop[0].Hooks[0].Type != "command" ||
		stop[0].Hooks[0].Command != `bash "$CLAUDE_PROJECT_DIR/.agent/skills/sweep/queue-guard.sh"` {
		t.Fatalf("sweep Stop hook is not the scoped queue guard: %+v", stop)
	}
	if strings.Contains(string(skill), ".agent/active") {
		t.Fatal("sweep skill still carries the retired repo-global marker")
	}
	proj := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(proj, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("queue-guard.sh", string(hookBytes))
	rootQueue := filepath.Join(proj, ".agent", "tasks")
	serviceQueue := filepath.Join(proj, "service", ".agent", "tasks")
	write(filepath.Join("bin", "coop"), "#!/bin/bash\n[ \"$1\" = tasks ] && [ \"$2\" = queues ] && printf '%s\\n' \""+rootQueue+"\" \""+serviceQueue+"\"\n")

	toolBin := filepath.Join(proj, "tools")
	if err := os.MkdirAll(toolBin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"find", "tr", "wc"} {
		src, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available", tool)
		}
		if err := os.Symlink(src, filepath.Join(toolBin, tool)); err != nil {
			t.Fatal(err)
		}
	}

	run := func(path string) (int, string) {
		t.Helper()
		cmd := exec.Command("bash", filepath.Join(proj, "queue-guard.sh"))
		cmd.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR="+proj, "PATH="+path)
		cmd.Stdin = strings.NewReader(`{"stop_hook_active":true}`)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode(), stderr.String()
			}
			t.Fatalf("hook run: %v", err)
		}
		return 0, stderr.String()
	}
	hostPath := filepath.Join(proj, "bin") + string(os.PathListSeparator) + toolBin

	for _, tc := range []struct {
		name, queue, state string
	}{
		{"host todo", rootQueue, "00_todo"},
		{"host in progress", serviceQueue, "10_in_progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(tc.queue, tc.state, strings.ReplaceAll(tc.name, " ", "-"), "task.md")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("id: x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if code, stderr := run(hostPath); code != 2 || !strings.Contains(stderr, "1 actionable task(s)") {
				t.Errorf("exit=%d stderr=%q, want aggregate hold", code, stderr)
			}
			if err := os.RemoveAll(filepath.Dir(filepath.Dir(path))); err != nil {
				t.Fatal(err)
			}
		})
	}

	write(filepath.Join(".agent", "tasks", "00_todo", "x", "task.md"), "id: x\n")
	write(filepath.Join("service", ".agent", "tasks", "10_in_progress", "y", "task.md"), "id: y\n")
	if code, stderr := run(toolBin); code != 2 || !strings.Contains(stderr, "2 actionable task(s)") {
		t.Errorf("no-coop discovery: exit=%d stderr=%q, want two-task hold", code, stderr)
	}
	if err := os.RemoveAll(filepath.Join(proj, ".agent", "tasks")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(proj, "service", ".agent", "tasks")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(".agent", "tasks", "50_blocked", "blocked", "task.md"), "id: blocked\n")
	if code, stderr := run(toolBin); code != 0 {
		t.Errorf("blocked-only queue: exit=%d stderr=%q, want release", code, stderr)
	}
	if err := os.RemoveAll(filepath.Join(proj, ".agent", "tasks")); err != nil {
		t.Fatal(err)
	}
	if code, stderr := run(toolBin); code != 0 {
		t.Errorf("empty queues: exit=%d stderr=%q, want release", code, stderr)
	}
}
