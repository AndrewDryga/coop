package cli

import (
	"strings"
	"testing"
)

// Every `coop help backlog|context|loop [<command>]` page is a byte-exact fixture. The approved
// design is the source of truth; a page that drifts from its fixture is a regression, not a new
// opinion.
func TestApprovedWorkflowHelpPages(t *testing.T) {
	for fixture, key := range map[string]string{
		"47-backlog-family-help":  "backlog",
		"48-backlog-ls-help":      "backlog ls",
		"48-backlog-add-help":     "backlog add",
		"49-backlog-promote-help": "backlog promote",
		"49-backlog-rm-help":      "backlog rm",
		"50-context-help":         "context",
		"52-loop-help":            "loop",
	} {
		t.Run(fixture, func(t *testing.T) {
			assertApprovedPage(t, fixture, commandHelp[key])
		})
	}
}

// The fork family page and every fork command's page, byte-exact. The family page is generated
// (its launch examples name the fork the reader asked about), so the manual's form is pinned here
// and the per-fork form below.
func TestApprovedForkHelpPages(t *testing.T) {
	assertApprovedPage(t, "59-fork-family-help", forkHelpText(""))
	for fixture, key := range map[string]string{
		"61-fork-acp-help":    "fork acp",
		"62-fork-ls-help":     "fork ls",
		"63-fork-review-help": "fork review",
		"64-fork-merge-help":  "fork merge",
		"65-fork-rm-help":     "fork rm",
		"65-fork-stop-help":   "fork stop",
		"66-fork-logs-help":   "fork logs",
		"66-fork-path-help":   "fork path",
		"66-fork-open-help":   "fork open",
	} {
		t.Run(fixture, func(t *testing.T) {
			assertApprovedPage(t, fixture, commandHelp[key])
		})
	}
}

// Asked about ONE fork, the family page names it in the usage line and the launch examples — and
// keeps the description column aligned when the name is longer than the approved placeholder.
func TestForkLaunchHelpNamesItsFork(t *testing.T) {
	page := forkHelpText("release-notes")
	for _, want := range []string{
		"Usage: coop fork release-notes [<target|preset>] [<options>]",
		"  coop fork release-notes claude     create a fork and start Claude",
		"  coop fork release-notes claude -d  work through tasks in the background",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("launch help is missing %q:\n%s", want, page)
		}
	}
	// The command index is about OTHER forks, so it keeps its placeholder.
	if !strings.Contains(page, "  review <name>  review a fork's changes") {
		t.Errorf("the command index must stay generic:\n%s", page)
	}
}

// Every page this design redesigned ends with its own next step, so none of them gets the generic
// "Run 'coop help' for all commands." footer.
func TestApprovedWorkflowHelpPagesEndThemselves(t *testing.T) {
	for _, key := range []string{"backlog", "backlog add", "context", "loop", "fork", "fork ls", "fork merge"} {
		if !selfContained(key) {
			t.Errorf("%q must print without the all-commands footer", key)
		}
	}
}
