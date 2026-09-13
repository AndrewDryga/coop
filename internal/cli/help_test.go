package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The SERVICES section is project-aware: it names this project's real services and Compose file
// when there is one, and with nothing to act on it dims the up/down pair (on a terminal) while the
// setup row stays bright — so the menu never advertises services that aren't defined, and never
// hides the command that adds them. The exact copy is pinned by TestApprovedMainMenu.
func TestHelpServicesSectionIsProjectAware(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"),
		[]byte("services:\n  db:\n    image: postgres\n  redis:\n    image: redis\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{RepoOverride: repo, BoxHome: "/b", ConfigDir: "/c"}
	if got := helpText(cfg); !strings.Contains(got, "SERVICES — db and redis, defined in .agent/compose.yml") {
		t.Errorf("the section should name the real services from the compose file:\n%s", got)
	}
	// Configured services: all three rows read normally, even though nothing is running.
	if got := renderMenu(ui.Colored(), cfg, false); strings.Contains(got, "\x1b[2mcoop up") {
		t.Errorf("configured services must not dim the up row:\n%q", got)
	}
	empty := &config.Config{RepoOverride: t.TempDir(), BoxHome: "/b", ConfigDir: "/c"}
	if got := helpText(empty); !strings.Contains(got, "SERVICES — databases and other services defined in .agent/compose.yml") {
		t.Errorf("with no services the section keeps the generic explanation:\n%s", got)
	}
	// No services: the whole up/down rows recede on a terminal, the setup row does not.
	got := renderMenu(ui.Colored(), empty, false)
	for _, want := range []string{"\x1b[2mcoop up", "\x1b[2mcoop down"} {
		if !strings.Contains(got, want) {
			t.Errorf("an unavailable row should be dimmed whole (%q):\n%q", want, got)
		}
	}
	if strings.Contains(got, "\x1b[2mcoop init --services") {
		t.Errorf("the setup row stays at normal brightness:\n%q", got)
	}
	// And a pipe gets the same text with no escapes at all.
	if plain := helpText(empty); strings.ContainsRune(plain, '\x1b') {
		t.Errorf("piped help must carry no escape sequences:\n%q", plain)
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
		"coop help sessions", "coop help acp", "coop help prompt", "coop help completion",
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
	for _, want := range []string{"THE BOX", "RUN AGENTS", "FORKS", "LOOPS", "TASKS", "SERVICES", "SECURITY & ISOLATION", "SETUP & MAINTENANCE", "INTEGRATIONS"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing capitalized section header %q", want)
		}
	}
	// Docs points at the readable docs site (a clickable URL), not the bare words "the README".
	if !strings.Contains(out, "https://coop.dryga.com") {
		t.Errorf("help footer should link to the docs site:\n%s", out)
	}
}

// Every help surface follows the no-middle-dot rule. The top-level check alone missed focused
// command topics for years, so enumerate those sources directly instead of relying on one render.
func TestAllHelpAvoidsMiddleDots(t *testing.T) {
	pages := map[string]string{
		"top-level": helpText(&config.Config{}),
		"run":       runHelp,
		"fork":      forkHelpText(""),
	}
	for _, name := range agents.Names() { // one generated page per agent, not one shared essay
		pages["agent "+name] = agentHelp(name)
	}
	for name, help := range commandHelp {
		pages[name] = help
	}
	// The prompt page's EXAMPLE quotes `coop prompt`'s own one-line output, whose segments are
	// separated by ·. help-output-style keeps an embedded runtime example in its real formatting;
	// the rule is about not joining separate command explanations with middle dots.
	delete(pages, "prompt")
	for name, help := range pages {
		if strings.Contains(help, "·") {
			t.Errorf("%s help should not use middle-dot separators:\n%s", name, help)
		}
	}
}

// menuWidth is the widest line the APPROVED menu contains (the coop context row, and the title
// with a long version). It is the budget, not a target: a new row that pushes past it changes the
// approved layout and needs the same decision the copy did.
const menuWidth = 92

// No menu line may exceed the approved width, or the two-column layout wraps on a stock terminal.
// Uses a no-services project and no signed-in agent — the widest static shape.
func TestHelpTextWidth(t *testing.T) {
	out := helpText(&config.Config{RepoOverride: t.TempDir(), ConfigDir: "/c", BoxHome: "/b"})
	for _, line := range strings.Split(out, "\n") {
		if n := len([]rune(line)); n > menuWidth {
			t.Errorf("menu line exceeds %d cols (%d): %q", menuWidth, n, line)
		}
	}
}

// Every command row's description starts at ONE column (a command cell ≤32 runes keeps the
// 34-rune gap), so the two-column layout reads as a table. Anything over the column is a bug:
// shorten the command cell (flag detail belongs in prose or the flags block, not the row).
func TestHelpRowsAlign(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"top level", helpText(&config.Config{RepoOverride: t.TempDir(), ConfigDir: "/c", BoxHome: "/b"})},
		{"fork", forkHelpText("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, line := range strings.Split(tc.text, "\n") {
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
				if n := len([]rune(cmd)); n > 32 {
					t.Errorf("help row command cell is %d runes (max 32, or the description column drifts): %q", n, cmd)
				}
			}
		})
	}
}

// RenderManual is the single source for `coop help --all`, docs/cli.md, and site/llms.txt — it must
// be deterministic and plain (no ANSI, no host-specific version/paths/state), or gendocs -check flaps.
func TestRenderManual(t *testing.T) {
	m := RenderManual(&config.Config{BoxHome: "/host-boxhome", ConfigDir: "/host-configdir"})
	if strings.Contains(m, "\x1b[") {
		t.Error("RenderManual must be plain — no ANSI escapes")
	}
	// The MENU's first-run block is state-aware and stays out of the reference; a page may carry
	// its own GET STARTED heading (the tasks family does), which is approved copy, not state.
	if strings.Contains(m, "GET STARTED — sign in") {
		t.Error("RenderManual must omit the state-aware GET STARTED block")
	}
	if strings.Contains(m, "/host-boxhome") || strings.Contains(m, "/host-configdir") {
		t.Error("RenderManual must not leak host config paths")
	}
	if RenderManual(&config.Config{}) != m {
		t.Error("RenderManual must be cfg-independent (deterministic across machines)")
	}
	// Contributor build/test guidance belongs in README.md, where a contributor looks — not
	// appended to the user's command reference.
	if strings.Contains(m, "SOURCE-TREE CONFORMANCE") || strings.Contains(m, "make provider-scripted-e2e") {
		t.Error("the user manual must not carry contributor build/test guidance")
	}
	for _, want := range []string{
		"THE BOX", "coop fork", "coop tasks", "coop run",
		"coop models [" + strings.Join(agents.Names(), "|") + "] [--refresh]",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("RenderManual missing %q", want)
		}
	}
}

// The manual opens with the reference form of the approved menu: the same copy, with the machine's
// version, service names and dimming left out so every machine renders the same bytes.
func TestManualOpensWithTheApprovedMenu(t *testing.T) {
	manual := RenderManual(&config.Config{})
	menu, rest, ok := strings.Cut(manual, "\n"+manualSeparator+"\n")
	if !ok || rest == "" {
		t.Fatal("the manual should be the menu, then one separated page per command")
	}
	data, err := os.ReadFile(filepath.Join("testdata", "approved", "01-main-help.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// The reference form drops the build version from the title; nothing else differs.
	want := strings.Replace(string(data), "coop "+fixtureVersion+" —", "coop —", 1)
	if menu != want {
		t.Errorf("the manual's menu drifted from the approved menu\n--- got ---\n%s\n--- want ---\n%s\n%s",
			menu, want, firstDifference(menu, want))
	}
}

// wantManualOrder is the page order of the approved full reference, restricted to the pages that
// exist today: the box, the agents, accounts and models, the work queues, loops and forks, this
// project's services, the checks, setup, and the integrations last. `coop worker` is retired: its
// workflow is `coop sessions connect`, whose page sits with the rest of the sessions leaves. Every
// family lists its leaves: a command a person can run is a command the manual explains.
// The pages a person lands on from the menu's last two groups END THEMSELVES: each closes with the
// command that follows it ("Start again: coop up"), so the generic all-commands footer would be a
// second, weaker answer to a question the page already answered.
func TestMaintenancePagesEndThemselves(t *testing.T) {
	for _, cmd := range []string{"up", "down", "doctor", "check-secrets", "build", "update"} {
		if !selfContained(cmd) {
			t.Errorf("coop help %s should end itself, not with the all-commands footer", cmd)
		}
	}
}

var wantManualOrder = []string{
	"run", "shell", "claude", "codex", "gemini", "grok",
	"login", "credentials", "credentials default", "credentials rm", "credentials account",
	"models", "presets init", "presets",
	"tasks", "tasks ls", "tasks add", "tasks claim", "tasks release", "tasks lease",
	"tasks block", "tasks unblock", "tasks done", "tasks path", "tasks queues",
	"tasks decisions", "tasks lint", "tasks rm", "tasks watch",
	"backlog", "backlog ls", "backlog add", "backlog promote", "backlog rm",
	"context", "loop",
	"fork", "fork acp", "fork ls", "fork review", "fork merge", "fork rm",
	"fork stop", "fork logs", "fork path", "fork open",
	"up", "down",
	"doctor", "net", "net runs", "net inspect", "net check", "net blocked", "net approve",
	"net watch", "net export", "net forget", "net setup", "net recover", "check-secrets", "sign",
	"init", "build", "update", "version",
	"acp", "sessions", "sessions serve", "sessions doctor", "sessions policies", "sessions compact", "sessions connect",
	"prompt", "completion",
}

// The manual presents every page in the approved order, each behind the same separator.
func TestManualPageOrder(t *testing.T) {
	if !slices.Equal(manualOrder, wantManualOrder) {
		t.Errorf("manualOrder drifted from the approved reference order:\n got: %v\nwant: %v", manualOrder, wantManualOrder)
	}
	manual := RenderManual(&config.Config{})
	_, pages, _ := strings.Cut(manual, "\n"+manualSeparator+"\n")
	at := 0
	for _, name := range wantManualOrder {
		first := strings.SplitN(strings.TrimRight(manualPage(name), "\n"), "\n", 2)[0]
		if first == "" {
			t.Errorf("%s has no page in the manual", name)
			continue
		}
		i := strings.Index(pages[at:], "\n"+first+"\n")
		if i < 0 {
			t.Errorf("%s's page is missing or out of order (expected after byte %d)", name, at)
			continue
		}
		at += i + len(first)
	}
}

// Every public command coop dispatches has a page in the manual — a command added to the dispatch
// without one is drift, not a deliberate omission.
func TestManualCoversEveryCommand(t *testing.T) {
	for _, name := range topLevelCommands {
		if name == "help" { // `coop help` IS the menu the manual opens with
			continue
		}
		if !slices.Contains(manualOrder, name) {
			t.Errorf("%q is dispatched but has no page in the manual", name)
			continue
		}
		if manualPage(name) == "" {
			t.Errorf("%q is in the manual's order but renders no page", name)
		}
	}
	for _, name := range agents.Names() {
		if !slices.Contains(manualOrder, name) {
			t.Errorf("registered agent %q has no page in the manual", name)
		}
	}
}

// Current public references must not re-advertise config names the strict loader rejects, or
// claim init creates starter subagents when its focused help correctly says it commits none.
func TestCurrentDocsDoNotAdvertiseRetiredContracts(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	surfaces := map[string]string{
		"CLI manual": RenderManual(&config.Config{}),
		"README":     read(filepath.Join("..", "..", "README.md")),
		"site docs":  read(filepath.Join("..", "..", "site", "docs.html")),
	}
	for name, surface := range surfaces {
		for _, retired := range []string{
			"COOP_LOOP_CMD", "COOP_LOOP_MODEL", "COOP_REVIEW_MODEL",
			"COOP_MAX_REVIEW_ROUNDS", "COOP_PREFLIGHT",
		} {
			if strings.Contains(surface, retired) {
				t.Errorf("%s advertises retired config %s", name, retired)
			}
		}
		if strings.Contains(surface, "can't set it over ACP") {
			t.Errorf("%s claims Codex ACP cannot receive its target model", name)
		}
	}

	manual := surfaces["CLI manual"]
	// The mode a project runs under comes from .agent/project.yaml and nowhere
	// else: `coop net approve` reviews that file, so no help surface may offer a
	// flag that would let the caller name a different one.
	if strings.Contains(manual, "--mode") {
		t.Errorf("the CLI manual offers a --mode flag; access comes from .agent/project.yaml:\n%s", manual)
	}
	if strings.Contains(manual, "scaffold the queue, hooks, skills, subagents") ||
		!strings.Contains(manual, "Shared instructions and skills for all your AI agents.") {
		t.Errorf("the init page does not match the current scaffold:\n%s", manual)
	}
	if site := surfaces["site docs"]; !strings.Contains(site, "commits no starter subagents") ||
		strings.Contains(site, "scaffolds two starter subagents") {
		t.Errorf("site init description does not match the current scaffold")
	}
	if readme := surfaces["README"]; !strings.Contains(readme, "selected [agent directories]") ||
		strings.Contains(readme, "[starter subagents]") {
		t.Errorf("README init description does not match the current scaffold")
	}
}

// helpRequested stops at `--`: a flag after it is passthrough to the agent, so `coop claude --
// --help` runs the agent's --help, not coop's page.
func TestHelpRequestedStopsAtDashDash(t *testing.T) {
	if helpRequested([]string{"claude", "--", "--help"}) {
		t.Error("--help after -- is passthrough, not a coop help request")
	}
	if !helpRequested([]string{"--help"}) || !helpRequested([]string{"x", "-h"}) {
		t.Error("--help / -h before -- must request coop help")
	}
}

// A newcomer with no usable account gets the GET STARTED block; once signed in, it's gone.
// Its exact copy is pinned by TestApprovedMainMenu.
func TestHelpFirstRunHint(t *testing.T) {
	fresh := &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir(), BoxHome: t.TempDir()}
	if anyAgentSignedIn(fresh) {
		t.Fatal("a fresh temp config must report no signed-in agent")
	}
	if !strings.Contains(helpText(fresh), "GET STARTED") {
		t.Error("help with no signed-in agent should show the GET STARTED block")
	}
	if strings.Contains(helpText(signedInConfig(t)), "GET STARTED") {
		t.Error("a signed-in user should not be told to get started")
	}
}

func TestHelpRunAgentsNamesProviders(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  *config.Config
		ref  bool
	}{
		{"first run", freshConfig(t), false},
		{"signed in", signedInConfig(t), false},
		{"first run with services", withServices(t, freshConfig(t)), false},
		{"signed in with services", withServices(t, signedInConfig(t)), false},
		{"reference", freshConfig(t), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			out := renderMenu(ui.Palette{}, test.cfg, test.ref)
			_, group, ok := strings.Cut(out, "RUN AGENTS —")
			if !ok {
				t.Fatal("RUN AGENTS group is missing")
			}
			group, _, _ = strings.Cut(group, "\n\n")
			const syntax = "  coop <claude|codex|gemini|grok>  "
			if !strings.Contains(group, "\n"+syntax) || strings.Contains(group, "\n  coop <agent>") {
				t.Fatalf("RUN AGENTS must name the supported providers:\n%s", group)
			}
		})
	}
}
