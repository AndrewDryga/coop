package cli

import (
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// TestCLIConformance graduates the committed .agent/rules into the gate: it walks the CLI surface as
// data and asserts the taste rules mechanically, so drift (a lister that forgot `list`, a destructive
// verb without `remove`, a verb added with no help row, a retired alias quietly re-accepted) fails CI
// instead of review. See .agent/rules/{list-verb-ls,destructive-verb-rm,help-output-style}.md.
func TestCLIConformance(t *testing.T) {
	newApp := func() *app {
		return &app{cfg: &config.Config{RepoOverride: t.TempDir(), ConfigDir: t.TempDir()}}
	}

	// list-verb-ls: `ls` is the list verb (fork + tasks list on it, exit 0). v3 keeps NO `list` alias —
	// it's an unknown verb in the closed families.
	t.Run("ls_lists_no_list_alias", func(t *testing.T) {
		if code, err := newApp().cmdFork([]string{"ls"}); code != 0 || err != nil {
			t.Errorf("coop fork ls = (%d, %v), want (0, nil)", code, err)
		}
		if code, err := cmdTasksFolder("", t.TempDir(), []string{"ls"}); code != 0 || err != nil {
			t.Errorf("coop tasks ls = (%d, %v), want (0, nil)", code, err)
		}
		if _, err := cmdTasksFolder("", t.TempDir(), []string{"list"}); err == nil || !strings.Contains(err.Error(), "unknown tasks command") {
			t.Errorf("coop tasks list should be unknown (no compat alias in v3), got %v", err)
		}
	})

	// destructive-verb-rm: `rm` is the destructive verb (a bare call is a usage/gate error, not the
	// unknown suggester). v3 keeps NO `remove` alias — it's unknown in the closed families (fork names
	// are open, so `remove` is a NAME there, not asserted).
	t.Run("rm_no_remove_alias", func(t *testing.T) {
		a := newApp()
		closed := map[string]func([]string) (int, error){
			"tasks": func(args []string) (int, error) { return cmdTasksFolder("", t.TempDir(), args) },
		}
		for name, run := range closed {
			if _, err := run([]string{"rm"}); err != nil && strings.Contains(err.Error(), "unknown") {
				t.Errorf("%s: rm was not accepted: %v", name, err)
			}
			if _, err := run([]string{"remove"}); err == nil || !strings.Contains(err.Error(), "unknown") {
				t.Errorf("%s: remove should be unknown (no compat alias in v3), got %v", name, err)
			}
		}
		if _, err := a.cmdFork([]string{"rm"}); err != nil && strings.Contains(err.Error(), "unknown") {
			t.Errorf("fork: rm was not accepted: %v", err)
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

	// v3 retired aliases are unknown commands (exit 2) rather than being silently re-accepted or
	// squatting a generic name — locked in against a future re-mint.
	t.Run("retired_forms_unknown", func(t *testing.T) {
		for _, argv := range [][]string{{"clone", "x"}, {"pool", "add", "p"}} {
			if code, err := newApp().dispatch(argv); code != 2 || err == nil {
				t.Errorf("retired %v should be an unknown command (exit 2), got (%d, %v)", argv, code, err)
			}
		}
	})
}
