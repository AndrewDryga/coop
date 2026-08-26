package cli

import (
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ui"
)

// TestCLIConformance graduates the committed .agent/kb/rules into the gate: it walks the CLI surface as
// data and asserts the taste rules mechanically, so drift (a lister that forgot `list`, a destructive
// verb without `remove`, a verb added with no help row, a retired alias quietly re-accepted) fails CI
// instead of review. See .agent/kb/rules/{list-verb-ls,destructive-verb-rm,help-output-style}.md.
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
		for _, v := range forkspace.VerbList() {
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

	// usage-placeholder-style: launch and peer values carry the full target grammar, so they use
	// <target>/<target|preset>, never the narrower provider-only <agent> or an undefined <peer>.
	t.Run("target_placeholders", func(t *testing.T) {
		errText := func(name string, err error) string {
			t.Helper()
			if err == nil {
				t.Errorf("%s unexpectedly succeeded; usage error surface is untested", name)
				return ""
			}
			return err.Error()
		}
		_, forkUsageErr := parseForkCreate(nil)
		_, forkPeerErr := parseForkCreate([]string{"work", "codex", "--loop", "--peer"})
		_, forkACPUsageErr := newApp().forkACP("work", []string{"not-a-target"})
		_, forkACPTargetErr := newApp().forkACP("work", nil)
		_, acpUsageErr := newApp().cmdACP([]string{"codex", "extra"})
		_, _, _, _, _, _, _, loopUsageErr := parseLoopArgs([]string{"--unknown"}, false)
		_, _, peerUsageErr := extractPeer([]string{"--peer"})

		surfaces := map[string]string{
			"top-level help":        renderHelp(newApp().cfg, true),
			"agent help":            agentHelp,
			"ACP help":              commandHelp["acp"],
			"ACP usage error":       errText("extra ACP argument", acpUsageErr),
			"loop help":             commandHelp["loop"],
			"fork help":             forkHelpText(ui.Palette{}),
			"fork usage error":      errText("empty fork", forkUsageErr),
			"fork peer error":       errText("valueless fork peer", forkPeerErr),
			"fork ACP usage error":  errText("invalid fork ACP target", forkACPUsageErr),
			"fork ACP target error": errText("missing fork ACP target", forkACPTargetErr),
			"loop usage error":      errText("unknown loop argument", loopUsageErr),
			"peer usage error":      errText("valueless peer", peerUsageErr),
			"no-provider error":     errText("missing loop target", noProviderErr("loop")),
		}
		for name, surface := range surfaces {
			for _, retired := range []string{
				"--peer <agent>", "--peer <peer>", "[<agent>[:model]", "[target|preset]",
				"<" + strings.Join(agents.Names(), "|") + ">",
			} {
				if strings.Contains(surface, retired) {
					t.Errorf("%s uses noncanonical target placeholder %q", name, retired)
				}
			}
		}
		for name, want := range map[string]string{
			"top-level help":        "coop <target> --peer <target>...",
			"agent help":            "Usage: coop <target> [coop flags]",
			"ACP help":              "coop acp <target|preset> [--peer <target>...]",
			"ACP usage error":       "coop acp <target|preset> [--peer <target>...]",
			"loop help":             "coop loop [<target|preset>]",
			"fork help":             "coop fork <name> [<target|preset>]",
			"fork usage error":      "usage: coop fork <name> [<target|preset>]",
			"fork peer error":       "--peer <target>",
			"fork ACP usage error":  "coop fork work acp <target> [--peer <target>...]",
			"fork ACP target error": "coop fork work acp <target>",
			"loop usage error":      "--peer <target>",
			"peer usage error":      "--peer <target>",
			"no-provider error":     "coop loop <target|preset>",
		} {
			if !strings.Contains(surfaces[name], want) {
				t.Errorf("%s missing canonical form %q", name, want)
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

	// Retired top-level forms are unknown commands (exit 2) rather than being silently re-accepted
	// or squatting a generic name — locked in against a future re-mint.
	t.Run("retired_forms_unknown", func(t *testing.T) {
		for _, argv := range [][]string{{"clone", "x"}, {"pool", "add", "p"}, {"fusion", "claude"}, {"fleet"}} {
			if code, err := newApp().dispatch(argv); code != 2 || err == nil {
				t.Errorf("retired %v should be an unknown command (exit 2), got (%d, %v)", argv, code, err)
			}
		}
	})
}
