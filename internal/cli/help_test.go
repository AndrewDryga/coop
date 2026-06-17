package cli

import (
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

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
		"coop doctor", "coop check-secrets", "coop tasks list|lint|add|split",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q", want)
		}
	}
	if strings.Contains(out, "<verb>") {
		t.Error("help still collapses fork verbs into a <verb> placeholder")
	}
}
