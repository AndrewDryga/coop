package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
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
	if got := helpText(cfg); !strings.Contains(got, "SERVICES: db and redis, defined in .agent/compose.yml") {
		t.Errorf("the section should name the real services from the compose file:\n%s", got)
	}
	// Configured services: all three rows read normally, even though nothing is running.
	if got := renderMenu(ui.Colored(), cfg, false); strings.Contains(got, "\x1b[2mcoop up") {
		t.Errorf("configured services must not dim the up row:\n%q", got)
	}
	empty := &config.Config{RepoOverride: t.TempDir(), BoxHome: "/b", ConfigDir: "/c"}
	if got := helpText(empty); !strings.Contains(got, "SERVICES: databases and other services defined in .agent/compose.yml") {
		t.Errorf("with no services the section keeps the generic explanation:\n%s", got)
	}
	// A Compose file coop will not read, here one over 1 MiB, says nothing about the services: the
	// generic explanation stays and the rows are not dimmed as if there were none.
	unreadable := t.TempDir()
	if err := os.MkdirAll(filepath.Join(unreadable, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unreadable, ".agent", "compose.yml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(unreadable, ".agent", "compose.yml"), 1<<20+1); err != nil {
		t.Fatal(err)
	}
	unknown := &config.Config{RepoOverride: unreadable, BoxHome: "/b", ConfigDir: "/c"}
	if got := helpText(unknown); !strings.Contains(got, "SERVICES: databases and other services defined in .agent/compose.yml") {
		t.Errorf("an unreadable Compose file should keep the generic explanation:\n%s", got)
	}
	if got := renderMenu(ui.Colored(), unknown, false); strings.Contains(got, "\x1b[2mcoop up") {
		t.Errorf("an unreadable Compose file must not dim the up row as if there were no services:\n%q", got)
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
	// The prompt page's EXAMPLE OUTPUT quotes `coop prompt`'s own one-line output, whose segments are
	// separated by ·. help-output-style keeps an embedded runtime example in its real formatting;
	// the rule is about not joining separate command explanations with middle dots.
	delete(pages, "prompt")
	for name, help := range pages {
		if strings.Contains(help, "·") {
			t.Errorf("%s help should not use middle-dot separators:\n%s", name, help)
		}
	}
}

// helpWidth is the widest line a help page may print: a stock 80-column terminal, where a longer
// line wraps through the middle of a table.
const helpWidth = 80

// The live menu keeps to the same width as the reference pages. Uses a no-services project and no
// signed-in agent, which adds the GET STARTED rows.
func TestHelpTextWidth(t *testing.T) {
	out := helpText(&config.Config{RepoOverride: t.TempDir(), ConfigDir: "/c", BoxHome: "/b"})
	for _, line := range strings.Split(out, "\n") {
		if n := len([]rune(line)); n > helpWidth {
			t.Errorf("menu line exceeds %d cols (%d): %q", helpWidth, n, line)
		}
	}
}

// An installed coop has no docs/ folder beside it, so a help page points at the published guide,
// never at a path in this repository; a GitHub URL to the file is a real link and stays allowed.
func TestHelpLinksPublishedGuidesNotRepoPaths(t *testing.T) {
	repoPath := regexp.MustCompile(`(^|[\s(])docs/\S+\.md`)
	for key, page := range commandHelp {
		if m := repoPath.FindString(page); m != "" {
			t.Errorf("help page %q points at the repository path %q; link the published page instead", key, strings.TrimSpace(m))
		}
	}
	if !strings.Contains(commandHelp["eval run"], "https://coop.dryga.com/evals.html") {
		t.Error(`coop help eval run does not link the published evaluation guide`)
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

// referenceHelpPages is every page the manual prints, keyed by its `coop help` path, plus the menu
// it opens with (key ""). These are the reference forms, so their bytes do not depend on the host.
// The checks below read these sources rather than a byte-pinned copy, which keeps whatever drift it
// was captured with: a fixture once pinned a one-column drift on the ACP page for months.
func referenceHelpPages() map[string]string {
	pages := map[string]string{"": renderHelp(&config.Config{}, true)}
	for _, name := range manualOrder {
		pages[name] = manualPage(name)
	}
	return pages
}

// No help line is wider than a stock terminal.
func TestHelpPagesFitTheTerminal(t *testing.T) {
	for name, page := range referenceHelpPages() {
		for _, line := range strings.Split(page, "\n") {
			if n := utf8.RuneCountInString(line); n > helpWidth {
				t.Errorf("coop help %s: a line is %d columns, over %d:\n%s", name, n, helpWidth, line)
			}
		}
	}
}

// Every two-column table on every page (commands, options, values, exit statuses) starts its
// second column in one place, and a wrapped description continues in that column. Stacked labeled
// pointers ("Start again: coop up") are a table too: their commands share one column.
func TestHelpColumnsAlign(t *testing.T) {
	for name, page := range referenceHelpPages() {
		for _, block := range strings.Split(page, "\n\n") {
			lines := strings.Split(strings.Trim(block, "\n"), "\n")
			for _, problem := range helpTableProblems(lines) {
				t.Errorf("coop help %s: %s:\n%s", name, problem, block)
			}
		}
	}
}

// Every page has one shape: the title, the Usage label, UPPERCASE section headings with their
// content indented directly beneath them, OPTIONS before EXAMPLES before EXIT STATUS, and closing
// pointers at column zero after the last section. Nothing else starts at column zero.
func TestHelpPagesShareOneLayout(t *testing.T) {
	for name, page := range referenceHelpPages() {
		for _, problem := range helpLayoutProblems(page) {
			t.Errorf("coop help %s: %s", name, problem)
		}
	}
}

// helpPointer is a labeled next step, "Start again: coop up": a capitalized label, a colon, then
// the command or link it names. Group 2 is where that command starts.
var helpPointer = regexp.MustCompile(`^( *)[A-Z][^:]*: +(coop |https://)`)

func helpIndent(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

// helpPointerColumn is where a pointer's command starts, in runes.
func helpPointerColumn(line string) int {
	m := helpPointer.FindStringSubmatchIndex(line)
	return utf8.RuneCountInString(line[:m[4]])
}

// helpSecondColumn is where a table row's second column starts, in runes: past the first run of
// two or more spaces after the row's first cell. It is -1 for a line with no such gap: prose, or a
// lone command.
func helpSecondColumn(line string) int {
	r := []rune(line)
	for i := helpIndent(line); i+1 < len(r); i++ {
		if r[i] != ' ' || r[i+1] != ' ' {
			continue
		}
		for i < len(r) && r[i] == ' ' {
			i++
		}
		if i == len(r) {
			return -1
		}
		return i
	}
	return -1
}

// helpTableProblems checks one blank-line-separated block. Lines at one indent with two or more
// gapped rows form a table: every line at that indent must be a row, every row's second column
// must start in one place, and a deeper line before that column is a misaligned continuation.
func helpTableProblems(block []string) []string {
	var problems []string
	for i := 0; i < len(block); {
		j := i + 1
		for j < len(block) && helpPointer.MatchString(block[i]) && helpPointer.MatchString(block[j]) &&
			helpIndent(block[j]) == helpIndent(block[i]) {
			j++
		}
		for _, line := range block[i+1 : j] {
			if got, want := helpPointerColumn(line), helpPointerColumn(block[i]); got != want {
				problems = append(problems, fmt.Sprintf("%q starts its command at column %d, not %d like the line above",
					strings.TrimSpace(line), got, want))
			}
		}
		i = j
	}
	levels := map[int][]string{}
	for _, line := range block {
		if !helpPointer.MatchString(line) {
			levels[helpIndent(line)] = append(levels[helpIndent(line)], line)
		}
	}
	for indent, lines := range levels {
		column, rows := -1, 0
		for _, line := range lines {
			if c := helpSecondColumn(line); c >= 0 {
				rows++
				if column < 0 {
					column = c
				}
			}
		}
		if rows < 2 {
			continue
		}
		for _, line := range lines {
			switch c := helpSecondColumn(line); {
			case c < 0:
				problems = append(problems, fmt.Sprintf("table row %q has no two-space gap before its description", strings.TrimSpace(line)))
			case c != column:
				problems = append(problems, fmt.Sprintf("table row %q starts its description at column %d, not %d",
					strings.TrimSpace(line), c, column))
			}
		}
		for _, line := range block {
			if in := helpIndent(line); in > indent && in < column && strings.TrimSpace(line) != "" {
				problems = append(problems, fmt.Sprintf("continuation %q starts at column %d, not %d",
					strings.TrimSpace(line), in, column))
			}
		}
	}
	return problems
}

// helpHeading reports whether a column-zero line is a section heading: UPPERCASE words, optionally
// followed by a colon and a short explanation ("CONFIGURE STEPS: .agent/loop.yaml").
func helpHeading(line string) bool {
	name, _, _ := strings.Cut(line, ": ")
	return line != "" && line[0] >= 'A' && line[0] <= 'Z' && strings.ToUpper(name) == name
}

// helpLabel reports whether lines[i] is a "Label:" whose commands follow it, indented.
func helpLabel(lines []string, i int) bool {
	return strings.HasSuffix(lines[i], ":") && i+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i+1]), "coop ")
}

func helpLayoutProblems(page string) []string {
	var problems []string
	lines := strings.Split(page, "\n")
	var headings []string
	closing := false                  // a closing pointer was seen: only pointers may follow
	for i := 1; i < len(lines); i++ { // lines[0] is the title
		line := lines[i]
		switch {
		case line == "" || strings.HasPrefix(line, " "), strings.HasPrefix(line, "Usage:"):
		case helpHeading(line):
			name, _, _ := strings.Cut(line, ": ")
			switch {
			case closing:
				problems = append(problems, fmt.Sprintf("heading %q follows the closing pointers", line))
			case name == "EXAMPLE":
				problems = append(problems, "the heading is EXAMPLES, even over one example")
			case i+1 == len(lines) || lines[i+1] == "":
				problems = append(problems, fmt.Sprintf("heading %q needs its content directly beneath it", line))
			}
			headings = append(headings, name)
		case helpPointer.MatchString(line) || helpLabel(lines, i):
			if !closing && lines[i-1] != "" {
				problems = append(problems, fmt.Sprintf("a blank line separates the closing pointers from the last section:\n%s\n%s", lines[i-1], line))
			}
			closing = true
		default:
			problems = append(problems, fmt.Sprintf("prose at column zero; indent it under its section:\n%s", line))
		}
	}
	if examples := slices.Index(headings, "EXAMPLES"); examples >= 0 {
		for i, h := range headings {
			if (strings.HasSuffix(h, "OPTIONS") && i > examples) || (h == "EXIT STATUS" && i < examples) {
				problems = append(problems, fmt.Sprintf("sections run %v; OPTIONS come before EXAMPLES, and EXIT STATUS after", headings))
				break
			}
		}
	}
	// The page's closing pointers start at column zero: indented under the last section, they read
	// as part of it.
	last := lines
	if i := strings.LastIndex(page, "\n\n"); i >= 0 {
		last = strings.Split(page[i+2:], "\n")
	}
	for i, line := range last {
		if helpIndent(line) > 0 && (helpPointer.MatchString(line) || helpLabel(last, i)) {
			problems = append(problems, fmt.Sprintf("closing pointer %q belongs at column zero, after the last section", strings.TrimSpace(line)))
		}
	}
	return problems
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
	if strings.Contains(m, "GET STARTED: sign in") {
		t.Error("RenderManual must omit the state-aware GET STARTED block")
	}
	if strings.Contains(m, "/host-boxhome") || strings.Contains(m, "/host-configdir") {
		t.Error("RenderManual must not leak host config paths")
	}
	if RenderManual(&config.Config{}) != m {
		t.Error("RenderManual must be cfg-independent (deterministic across machines)")
	}
	// Contributor build/test guidance belongs in CONTRIBUTING.md, where a contributor looks — not
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
	want := strings.Replace(string(data), "coop "+fixtureVersion+":", "coop:", 1)
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
	"models", "usage", "presets init", "presets",
	"tasks", "tasks ls", "tasks add", "tasks claim", "tasks release", "tasks lease",
	"tasks block", "tasks unblock", "tasks park", "tasks done", "tasks path", "tasks queues",
	"tasks decisions", "tasks lint", "tasks rm", "tasks watch",
	"backlog", "backlog ls", "backlog add", "backlog promote", "backlog rm",
	"context", "loop",
	"eval", "eval run", "eval runs", "eval inspect", "eval compare", "eval ls", "eval init",
	"fork", "fork acp", "fork ls", "fork review", "fork merge", "fork rm",
	"fork stop", "fork logs", "fork path", "fork open",
	"up", "down",
	"doctor", "approve", "net", "net runs", "net inspect", "net check", "net blocked",
	"net watch", "net export", "net forget", "net setup", "net recover", "check-secrets", "sign",
	"init", "build", "update", "version",
	"acp", "sessions", "sessions doctor", "sessions check", "sessions quiescence", "sessions compact", "sessions connect",
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

// docs/cli.md pins each page's bytes, but a trailing newline would print as an extra blank line
// after `coop help <command>`, which adds its own.
func TestManualPagesEndWithoutANewline(t *testing.T) {
	for _, name := range manualOrder {
		if strings.HasSuffix(manualPage(name), "\n") {
			t.Errorf("the %q page ends in a newline", name)
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
	// else: `coop approve` reviews that file, so no help surface may offer a
	// flag that would let the caller name a different one.
	if strings.Contains(manual, "--mode") {
		t.Errorf("the CLI manual offers a --mode flag; access comes from .agent/project.yaml:\n%s", manual)
	}
	if strings.Contains(manual, "scaffold the queue, hooks, skills, subagents") ||
		!strings.Contains(manual, "Shared instructions and skills for all your AI agents.") {
		t.Errorf("the init page does not match the current scaffold:\n%s", manual)
	}
	if site := surfaces["site docs"]; !strings.Contains(site, "does not create starter subagents") ||
		strings.Contains(site, "scaffolds two starter subagents") {
		t.Errorf("site init description does not match the current scaffold")
	}
	if readme := surfaces["README"]; !strings.Contains(readme, "selected [agent directories]") ||
		strings.Contains(readme, "[starter subagents]") {
		t.Errorf("README init description does not match the current scaffold")
	}
}

// A person spells the no-network mode offline on every surface; none is internal only and the
// parsers refuse it, so no page may teach it (.agent/kb/rules/offline-not-none.md). The generated
// docs/cli.md follows the manual, so the manual stands for it.
func TestHelpSpellsTheNoNetworkModeOffline(t *testing.T) {
	surfaces := map[string]string{"CLI manual": RenderManual(&config.Config{})}
	for _, path := range []string{"README.md", "docs/agents.md", "docs/box.md", "docs/configuration.md",
		"docs/integrations.md", "docs/loop.md", "docs/networking.md"} {
		data, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		surfaces[path] = string(data)
	}
	for name, surface := range surfaces {
		for _, retired := range []string{"--egress none", "--egress=none", "EGRESS=none", "egress: none", "open or none", "open, or none"} {
			if strings.Contains(surface, retired) {
				t.Errorf("%s teaches the retired %q; the word is offline", name, retired)
			}
		}
	}
	for _, example := range []string{"coop run --egress offline", "coop shell --egress offline", "open or offline"} {
		if !strings.Contains(surfaces["CLI manual"], example) {
			t.Errorf("the CLI manual lost %q", example)
		}
	}
}

// A box nothing chose a mode for is filtered, inside a project or not, so the pages people read
// before a run say so, and how to get unrestricted access back.
func TestRunAndShellHelpStateTheFilteredDefault(t *testing.T) {
	for name, page := range map[string]string{"run": runHelp, "shell": manualPage("shell")} {
		for _, want := range []string{
			"filtered  allow traffic permitted by approved network rules (default)",
			"Without --egress, the box is filtered unless the project's network",
			"coop " + name + " --egress open",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("coop help %s lost %q:\n%s", name, want, page)
			}
		}
	}
}

// A rules file's shape is not guessable, and the file adds to the project's rules rather than
// replacing them: the run page shows a minimal file that really decodes, the launch pages say the
// flag adds rules, and the network paragraph says a one-off open or offline run leaves them out.
func TestRunHelpShowsAMinimalRulesFile(t *testing.T) {
	_, rest, found := strings.Cut(runHelp, "RULES FILE\n")
	if !found {
		t.Fatalf("coop help run has no RULES FILE section:\n%s", runHelp)
	}
	var example []string
	for _, line := range strings.Split(rest, "\n") {
		if strings.HasPrefix(line, "    ") {
			example = append(example, strings.TrimPrefix(line, "    "))
		} else if len(example) != 0 {
			break
		}
	}
	rules, err := egress.DecodeRules([]byte(strings.Join(example, "\n") + "\n"))
	if err != nil || len(rules) != 1 || rules[0].To.Domain != "docs.example.com" {
		t.Fatalf("the help's rules file does not decode: %v %v\n%s", rules, err, strings.Join(example, "\n"))
	}
	for name, page := range map[string]string{"run": runHelp, "shell": manualPage("shell")} {
		if want := "With --egress open or\n  offline, the project's network rules do not apply to that run."; !strings.Contains(page, want) {
			t.Errorf("coop help %s does not say a one-off open or offline run leaves the project's rules out", name)
		}
	}
	for name, page := range map[string]string{"run": runHelp, "shell": manualPage("shell"), "acp": manualPage("acp")} {
		if strings.Contains(page, "load network rules") || !strings.Contains(page, "--egress-rules <path>  add the network rules in a file") {
			t.Errorf("coop help %s does not say --egress-rules adds rules", name)
		}
	}
}

// coop net check answers through its exit status too, so its page documents every code it returns,
// and its options name the connection types its usage lines show.
func TestNetCheckHelpDocumentsItsExitStatus(t *testing.T) {
	page := commandHelp["net check"]
	for _, want := range []string{
		"coop net check <ip> --run <run> --protocol <tcp|udp> --port <n>",
		"  --protocol <tcp|udp>  choose the connection type for an IP address",
		"EXIT STATUS\n  0  the rules allow the connection\n  1  the check could not finish\n  2  invalid arguments\n" +
			"  3  the rules block the connection, or new runs need approval first",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("coop help net check lost %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "tls|tcp|udp") {
		t.Errorf("coop help net check offers a connection type its usage does not:\n%s", page)
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
			_, group, ok := strings.Cut(out, "RUN AGENTS:")
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
