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

// Every top-level help line fits a stock 80-column terminal, so the two-column layout doesn't wrap.
// Uses a no-compose repo (dim up/down rows) and no signed-in agent (FIRST RUN shown) — the widest
// static shape. Short fixed config paths keep it deterministic: the footer's Config/Auth lines echo
// the user's own (arbitrarily long) paths and aren't part of the two-column layout, so skip them.
func TestHelpTextWidth(t *testing.T) {
	out := helpText(&config.Config{RepoOverride: t.TempDir(), ConfigDir: "/c", BoxHome: "/b"})
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Config ") || strings.HasPrefix(line, "Auth ") {
			continue // user config paths, not layout rows
		}
		if n := len([]rune(line)); n > 80 {
			t.Errorf("help line exceeds 80 cols (%d): %q", n, line)
		}
	}
}

// Every command row's description starts at ONE column (a command cell ≤32 runes keeps the
// 34-rune gap), so the two-column layout reads as a table. A row whose command is a full
// verb list (it carries several `|`s, like fleet's) is the accepted exception — listing
// every verb beats alignment there. Anything else over the column is a bug: shorten the
// command cell (flag detail belongs in the command's own help page, not the overview).
func TestHelpRowsAlign(t *testing.T) {
	out := helpText(&config.Config{RepoOverride: t.TempDir(), ConfigDir: "/c", BoxHome: "/b"})
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "  coop ") {
			continue // headers, hints, footer — not command rows
		}
		runes := []rune(line)
		cmdEnd := len(runes)
		for i := 2; i < len(runes)-1; i++ {
			if runes[i] == ' ' && runes[i+1] == ' ' {
				cmdEnd = i
				break
			}
		}
		cmd := strings.TrimRight(string(runes[2:cmdEnd]), " ")
		if strings.Count(cmd, "|") >= 3 {
			continue // a verb-list row (e.g. fleet init|up|down|…) may run long
		}
		if n := len([]rune(cmd)); n > 32 {
			t.Errorf("help row command cell is %d runes (max 32, or the description column drifts): %q", n, cmd)
		}
	}
}

// RenderManual is the single source for `coop help --all`, docs/cli.md, and site/llms.txt — it must
// be deterministic and plain (no ANSI, no host-specific version/paths/state), or gendocs -check flaps.
func TestRenderManual(t *testing.T) {
	m := RenderManual(&config.Config{BoxHome: "/host-boxhome", ConfigDir: "/host-configdir"})
	if strings.Contains(m, "\x1b[") {
		t.Error("RenderManual must be plain — no ANSI escapes")
	}
	if strings.Contains(m, "FIRST RUN") {
		t.Error("RenderManual must omit the state-aware FIRST RUN hint")
	}
	if strings.Contains(m, "/host-boxhome") || strings.Contains(m, "/host-configdir") {
		t.Error("RenderManual must not leak host config paths")
	}
	if RenderManual(&config.Config{}) != m {
		t.Error("RenderManual must be cfg-independent (deterministic across machines)")
	}
	for _, want := range []string{"AGENTS", "coop fork", "coop tasks", "coop run"} {
		if !strings.Contains(m, want) {
			t.Errorf("RenderManual missing %q", want)
		}
	}
}

// helpRequested stops at `--`: a flag after it is passthrough to the agent, so `coop fusion claude --
// --help` runs the agent's --help, not coop's page.
func TestHelpRequestedStopsAtDashDash(t *testing.T) {
	if helpRequested([]string{"claude", "--", "--help"}) {
		t.Error("--help after -- is passthrough, not a coop help request")
	}
	if !helpRequested([]string{"--help"}) || !helpRequested([]string{"x", "-h"}) {
		t.Error("--help / -h before -- must request coop help")
	}
}

// A newcomer with no agent signed in gets the day-one FIRST RUN hint; once signed in, it's gone.
func TestHelpFirstRunHint(t *testing.T) {
	fresh := &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir(), BoxHome: t.TempDir()}
	if anyAgentSignedIn(fresh) {
		t.Fatal("a fresh temp config must report no signed-in agent")
	}
	if !strings.Contains(helpText(fresh), "FIRST RUN") {
		t.Error("help with no signed-in agent should show the FIRST RUN hint")
	}
}
