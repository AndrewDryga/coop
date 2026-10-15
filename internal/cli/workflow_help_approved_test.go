package cli

import (
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/forkspace"
)

// Asked about ONE fork, the family page names it in the usage line and the launch examples — and
// keeps the description column aligned when the name is longer than the approved placeholder.
// The placeholder form is the manual's page, which docs/cli.md pins.
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

// An example fork name has to read as a name. A Coop command, fork verb or provider in that slot
// reads as syntax: `coop fork login claude` looked like signing in to forks.
func TestForkHelpExampleNamesAreNotCoopWords(t *testing.T) {
	words := map[string]bool{}
	for _, word := range slices.Concat(topLevelCommands, forkspace.VerbList(), agents.Names()) {
		words[word] = true
	}
	pages := map[string]string{"fork": forkHelpText(""), "acp": commandHelp["acp"]}
	for key, page := range commandHelp {
		if strings.HasPrefix(key, "fork ") {
			pages[key] = page
		}
	}
	for key, page := range pages {
		for _, line := range strings.Split(page, "\n") {
			for _, rest := range strings.Split(line, "coop fork ")[1:] {
				fields := strings.Fields(rest)
				if len(fields) > 0 && forkspace.Reserved(fields[0]) {
					fields = fields[1:] // coop fork <verb> <name>
				}
				if len(fields) == 0 || strings.HasPrefix(fields[0], "<") || strings.HasPrefix(fields[0], "-") {
					continue // a placeholder or a flag, not an example name
				}
				if words[fields[0]] {
					t.Errorf("%q help names an example fork %q, which is a Coop word:\n%s", key, fields[0], line)
				}
			}
		}
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
