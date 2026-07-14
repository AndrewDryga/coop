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
	for _, queue := range []string{rootQueue, serviceQueue} {
		if err := os.MkdirAll(queue, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	called := filepath.Join(proj, "coop-called")
	write(filepath.Join("bin", "coop"), "#!/bin/bash\nprintf called > \"$COOP_CALLED\"\n[ \"$1\" = tasks ] && [ \"$2\" = queues ] || exit 1\nprintf '%s\\n' \"$COOP_QUEUES\"\n")

	toolBin := filepath.Join(proj, "tools")
	if err := os.MkdirAll(toolBin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"find"} {
		src, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available", tool)
		}
		if err := os.Symlink(src, filepath.Join(toolBin, tool)); err != nil {
			t.Fatal(err)
		}
	}

	run := func(t *testing.T, path, queues string) (int, string) {
		t.Helper()
		cmd := exec.Command("bash", filepath.Join(proj, "queue-guard.sh"))
		cmd.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR="+proj, "PATH="+path,
			"COOP_CALLED="+called, "COOP_QUEUES="+queues)
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
	configured := rootQueue + "\n" + serviceQueue
	write(filepath.Join("unconfigured", ".agent", "tasks", "00_todo", "must-ignore", "task.md"), "id: ignored\n")

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
			if code, stderr := run(t, hostPath, configured); code != 2 || !strings.Contains(stderr, "1 actionable task(s)") {
				t.Errorf("exit=%d stderr=%q, want aggregate hold", code, stderr)
			}
			if _, err := os.Stat(called); err != nil {
				t.Errorf("host queue resolver was not invoked: %v", err)
			}
			if err := os.RemoveAll(filepath.Dir(filepath.Dir(path))); err != nil {
				t.Fatal(err)
			}
		})
	}

	write(filepath.Join(".agent", "tasks", "00_todo", "x", "task.md"), "id: x\n")
	write(filepath.Join("service", ".agent", "tasks", "10_in_progress", "y", "task.md"), "id: y\n")
	if code, stderr := run(t, toolBin, ""); code != 2 || !strings.Contains(stderr, "3 actionable task(s)") {
		t.Errorf("no-coop discovery: exit=%d stderr=%q, want three-task hold", code, stderr)
	}
	if err := os.RemoveAll(filepath.Join(proj, ".agent", "tasks")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(proj, "service", ".agent", "tasks")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(proj, "unconfigured", ".agent", "tasks")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootQueue, "00_todo", "malformed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, stderr := run(t, toolBin, ""); code != 2 || !strings.Contains(stderr, "1 actionable task(s)") {
		t.Errorf("malformed live task folder: exit=%d stderr=%q, want hold", code, stderr)
	}
	if err := os.RemoveAll(rootQueue); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(".agent", "tasks", "50_blocked", "blocked", "task.md"), "id: blocked\n")
	if code, stderr := run(t, toolBin, ""); code != 0 {
		t.Errorf("blocked-only queue: exit=%d stderr=%q, want release", code, stderr)
	}
	if err := os.RemoveAll(filepath.Join(proj, ".agent", "tasks")); err != nil {
		t.Fatal(err)
	}
	if code, stderr := run(t, toolBin, ""); code != 0 {
		t.Errorf("empty queues: exit=%d stderr=%q, want release", code, stderr)
	}

	t.Run("configured discovery failure fails closed", func(t *testing.T) {
		write(filepath.Join("bin", "coop"), "#!/bin/bash\nexit 7\n")
		if code, stderr := run(t, hostPath, configured); code != 2 || !strings.Contains(stderr, "could not discover") {
			t.Errorf("exit=%d stderr=%q, want actionable failure", code, stderr)
		}
	})

	t.Run("count failure fails closed", func(t *testing.T) {
		write(filepath.Join("bin", "coop"), "#!/bin/bash\nprintf '%s\\n' \"$COOP_QUEUES\"\n")
		if err := os.MkdirAll(filepath.Join(rootQueue, "00_todo"), 0o755); err != nil {
			t.Fatal(err)
		}
		failTools := filepath.Join(proj, "fail-tools")
		if err := os.MkdirAll(failTools, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(failTools, "find"), []byte("#!/bin/bash\nexit 9\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(proj, "bin") + string(os.PathListSeparator) + failTools
		if code, stderr := run(t, path, rootQueue); code != 2 || !strings.Contains(stderr, "cannot count") {
			t.Errorf("exit=%d stderr=%q, want count failure", code, stderr)
		}
	})

	t.Run("symlinked queue root fails closed", func(t *testing.T) {
		target := t.TempDir()
		link := filepath.Join(proj, "linked-tasks")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		write(filepath.Join("bin", "coop"), "#!/bin/bash\nprintf '%s\\n' \"$COOP_QUEUES\"\n")
		if code, stderr := run(t, hostPath, link); code != 2 || !strings.Contains(stderr, "symlinked queue root") {
			t.Errorf("exit=%d stderr=%q, want symlink denial", code, stderr)
		}
	})
}
