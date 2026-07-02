package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// `coop up`/`down` in the top-level help are compose-aware: they name the repo's real service keys
// when a compose.agent.yml is present, and fall back to a "none" wording (dimmed on a tty) when it
// isn't — so the help never advertises generic services that aren't actually defined.
func TestHelpUpDownComposeAware(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "compose.agent.yml"),
		[]byte("services:\n  db:\n    image: postgres\n  redis:\n    image: redis\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := helpText(&config.Config{RepoOverride: repo, BoxHome: "/b", ConfigDir: "/c"})
	if !strings.Contains(got, "compose.agent.yml services (db, redis)") {
		t.Errorf("up should name the real services from the compose file:\n%s", got)
	}
	if got := helpText(&config.Config{RepoOverride: t.TempDir(), BoxHome: "/b", ConfigDir: "/c"}); !strings.Contains(got, "none in compose.agent.yml") {
		t.Errorf("up should say none when there is no compose file:\n%s", got)
	}
}

func TestHelpTextAligned(t *testing.T) {
	out := helpText(&config.Config{BoxHome: "/home/u/.config/coop", ConfigDir: "/home/u/.config/coop/agents"})

	// No command row may glue its description to the command: every "  coop …" line must
	// keep a column gap (a run of ≥2 spaces after the 2-space indent). This is the bug the
	// old fixed-width %-33s produced when a command was longer than the column.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  coop ") && !strings.Contains(line[2:], "  ") {
			t.Errorf("command row has no column gap (description glued):\n%q", line)
		}
	}

	// Commands are listed individually, not collapsed into a "<verb>" placeholder.
	for _, want := range []string{
		"coop fork review <name>", "coop fork merge <name>", "coop fork stop <name>",
		"coop doctor", "coop check-secrets", "coop tasks ls", "coop tasks decisions",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q", want)
		}
	}
	if strings.Contains(out, "<verb>") {
		t.Error("help still collapses fork verbs into a <verb> placeholder")
	}
	// No middle-dot separators, and section headers are capitalized.
	if strings.Contains(out, "·") {
		t.Errorf("help should not use · separators:\n%s", out)
	}
	for _, want := range []string{"AGENTS", "FORKS", "UNATTENDED", "TASKS", "SETUP & MAINTENANCE"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing capitalized section header %q", want)
		}
	}
	// Docs points at the readable docs site (a clickable URL), not the bare words "the README".
	if !strings.Contains(out, "https://coop.dryga.com") {
		t.Errorf("help footer should link to the docs site:\n%s", out)
	}
}
