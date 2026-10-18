package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/project"
)

// A settings file Coop cannot use names the file, the line that carries the problem and the setting
// — never the value, which in this file may be a token. The fix is the last line, so the reader ends
// on the one thing to change.
func TestApprovedSettingsFailures(t *testing.T) {
	t.Run("78-settings-unknown-setting", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config")
		lines := make([]string, 11)
		for i := range lines {
			lines[i] = "# Coop settings"
		}
		lines = append(lines, `COOP_PIDD=4096`)
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("COOP_CONF", path)
		_, err := config.Load()
		if err == nil {
			t.Fatal("a misspelled setting should be refused, not ignored")
		}
		got := strings.ReplaceAll(usageBlock(t, err), path, "/Users/example/.config/coop/config")
		assertApprovedOutput(t, "78-settings-unknown-setting", got)
	})

	t.Run("78-settings-invalid-value", func(t *testing.T) {
		empty := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(empty, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("COOP_CONF", empty)
		t.Setenv("COOP_PIDS", "several")
		_, err := config.Load()
		if err == nil {
			t.Fatal("a process limit that is not a number should be refused")
		}
		assertApprovedOutput(t, "78-settings-invalid-value", usageBlock(t, err))
	})
}

// The project file is committed and read on the host, so Coop never follows a link to reach it. The
// refusal says what the path is, not what an attacker might have meant by it.
func TestApprovedProjectFileRefusal(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(repo, "elsewhere.yaml")
	if err := os.WriteFile(target, []byte("serve: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(repo, project.File)); err != nil {
		t.Fatal(err)
	}
	_, err := project.Load(repo)
	if err == nil {
		t.Fatal("a linked project file should be refused, not followed")
	}
	assertApprovedOutput(t, "78-project-file-symlink", usageBlock(t, err))
}
