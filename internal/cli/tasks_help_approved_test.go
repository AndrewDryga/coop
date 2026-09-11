package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every `coop help tasks [<command>]` page is a byte-exact fixture. The approved design is the
// source of truth; a page that drifts from its fixture is a regression, not a new opinion.
func TestApprovedTaskHelpPages(t *testing.T) {
	for fixture, key := range map[string]string{
		"35-family-help":       "tasks",
		"36-list-help":         "tasks ls",
		"37-creation-help":     "tasks add",
		"38-claim-help":        "tasks claim",
		"38-release-help":      "tasks release",
		"38-lease-help":        "tasks lease",
		"39-block-help":        "tasks block",
		"39-unblock-help":      "tasks unblock",
		"39-done-help":         "tasks done",
		"40-path-help":         "tasks path",
		"40-queues-help":       "tasks queues",
		"41-42-decisions-help": "tasks decisions",
		"44-lint-help":         "tasks lint",
		"45-removal-help":      "tasks rm",
		"46-watch-help":        "tasks watch",
	} {
		t.Run(fixture, func(t *testing.T) {
			assertApprovedPage(t, fixture, commandHelp[key])
		})
	}
}

// Each task page ends with its own next step, so none of them gets the generic
// "Run 'coop help' for all commands." footer.
func TestApprovedTaskHelpPagesEndThemselves(t *testing.T) {
	for _, key := range []string{"tasks", "tasks ls", "tasks add", "tasks block", "tasks watch"} {
		if !selfContained(key) {
			t.Errorf("%q must print without the all-commands footer", key)
		}
	}
}

// assertApprovedPage compares a rendered HELP PAGE to its approved fixture, reporting the first
// differing line so a drift is readable instead of a wall of text. A fixture ends with a newline
// (files do); a rendered page does not, so the comparison is on the trimmed form of both —
// unlike assertApprovedOutput, which pins a whole transcript byte for byte.
func assertApprovedPage(t *testing.T, fixture, got string) {
	t.Helper()
	path := filepath.Join("testdata", "approved", fixture+".txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	want := strings.TrimRight(string(data), "\n")
	if strings.TrimRight(got, "\n") == want {
		return
	}
	wantLines, gotLines := strings.Split(want, "\n"), strings.Split(strings.TrimRight(got, "\n"), "\n")
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		w, g := "", ""
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			t.Fatalf("%s line %d:\n want %q\n  got %q", path, i+1, w, g)
		}
	}
	t.Fatalf("%s differs from the approved fixture", path)
}
