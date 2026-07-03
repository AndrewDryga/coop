package cli

import (
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// TestCLIConformance graduates the committed .agent/rules into the gate: it walks the CLI surface as
// data and asserts the taste rules mechanically, so drift (a lister that forgot `list`, a destructive
// verb without `remove`, a verb added with no help row, a retired alias quietly re-accepted) fails CI
// instead of review. See .agent/rules/{list-verb-ls,destructive-verb-rm,help-output-style}.md and the
// v3 tombstone registry (removedCommandNote).
func TestCLIConformance(t *testing.T) {
	newApp := func() *app {
		return &app{cfg: &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir()}}
	}

	// list-verb-ls: the families with a list verb accept BOTH `ls` and `list`, routed to the lister
	// (exit 0 on an empty repo/queue). A family that grew an `ls` without `list` fails here.
	t.Run("ls_and_list_both_list", func(t *testing.T) {
		for _, v := range []string{"ls", "list"} {
			if code, err := newApp().cmdFork([]string{v}); code != 0 || err != nil {
				t.Errorf("coop fork %s = (%d, %v), want (0, nil)", v, code, err)
			}
			if code, err := cmdTasksFolder("", t.TempDir(), []string{v}); code != 0 || err != nil {
				t.Errorf("coop tasks %s = (%d, %v), want (0, nil)", v, code, err)
			}
		}
	})

	// destructive-verb-rm: every destructive family accepts BOTH `rm` and `remove` — each routes to the
	// handler (a bare invocation yields that handler's usage/gate error, never the unknown-command
	// suggester). A family that dropped the `remove` alias fails here.
	t.Run("rm_and_remove_both_accepted", func(t *testing.T) {
		a := newApp()
		families := map[string]func([]string) (int, error){
			"fork":  a.cmdFork,
			"pool":  a.cmdPool,
			"tasks": func(args []string) (int, error) { return cmdTasksFolder("", t.TempDir(), args) },
		}
		for name, run := range families {
			for _, v := range []string{"rm", "remove"} {
				if _, err := run([]string{v}); err != nil && strings.Contains(err.Error(), "unknown") {
					t.Errorf("%s: %q was not accepted (got the unknown-command error): %v", name, v, err)
				}
			}
		}
	})

	// help-output-style: every canonical verb appears in its family's help — a verb added to the
	// dispatch without a help row is drift this catches.
	t.Run("verbs_documented_in_help", func(t *testing.T) {
		forkHelpTxt := captureStdout(t, func() { _, _ = forkHelp() })
		for _, v := range forkVerbList() {
			if !strings.Contains(forkHelpTxt, v) {
				t.Errorf("fork verb %q has no row in forkHelp", v)
			}
		}
		for _, v := range tasksVerbs {
			if !strings.Contains(commandHelp["tasks"], v) {
				t.Errorf("tasks verb %q is missing from commandHelp[tasks]", v)
			}
		}
	})

	// A closed verb set (tasks) rejects anything not in it with the unknown-command suggester — so a
	// stray dispatch case (a verb not reflected in tasksVerbs) can't hide. (fork's names are open by
	// design — a non-verb IS a fork name — so no-stray doesn't apply there.)
	t.Run("unknown_verb_rejected", func(t *testing.T) {
		if _, err := cmdTasksFolder("", t.TempDir(), []string{"definitely-not-a-verb"}); err == nil ||
			!strings.Contains(err.Error(), "unknown tasks command") {
			t.Error("an unknown tasks verb should hit the unknown-command error")
		}
	})

	// v3 retired aliases tombstone (exit 2) rather than being silently re-accepted or squatting a
	// generic name — locked in against a future re-mint.
	t.Run("retired_forms_tombstone", func(t *testing.T) {
		for _, argv := range [][]string{{"clone", "x"}, {"pool", "add", "p"}} {
			if code, err := newApp().dispatch(argv); code != 2 || err == nil {
				t.Errorf("retired %v should tombstone (exit 2), got (%d, %v)", argv, code, err)
			}
		}
	})
}
