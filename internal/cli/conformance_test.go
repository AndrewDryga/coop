package cli

import (
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
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
		if _, err := cmdTasksFolder("", t.TempDir(), []string{"list"}); err == nil || !strings.Contains(err.Error(), `Unknown command "coop tasks list"`) {
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
			if _, err := run([]string{"rm"}); err != nil && strings.Contains(err.Error(), "Unknown command") {
				t.Errorf("%s: rm was not accepted: %v", name, err)
			}
			if _, err := run([]string{"remove"}); err == nil || !strings.Contains(err.Error(), "Unknown command") {
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
		forkHelpTxt := captureStdout(t, func() { _, _ = forkHelp("") })
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
		_, _, _, _, _, _, _, loopUsageErr := parseLoopArgs([]string{"claude", "extra"}, false)
		_, _, peerUsageErr := extractPeer("coop run", []string{"--peer"})

		surfaces := map[string]string{
			"top-level help":        renderHelp(newApp().cfg, true),
			"agent help":            agentHelp("claude"),
			"ACP help":              commandHelp["acp"],
			"ACP usage error":       errText("extra ACP argument", acpUsageErr),
			"loop help":             commandHelp["loop"],
			"fork help":             forkHelpText(""),
			"fork usage error":      errText("empty fork", forkUsageErr),
			"fork peer error":       errText("valueless fork peer", forkPeerErr),
			"fork ACP usage error":  errText("invalid fork ACP target", forkACPUsageErr),
			"fork ACP target error": errText("missing fork ACP target", forkACPTargetErr),
			"loop usage error":      errText("extra loop argument", loopUsageErr),
			"peer usage error":      errText("valueless peer", peerUsageErr),
			"no-provider error":     errText("missing loop target", noProviderErr("loop")),
		}
		for name, surface := range surfaces {
			if name == "top-level help" {
				// The approved beginner start row names the providers. Excuse only that
				// exact row once; peer/target syntax elsewhere still uses the full grammar.
				surface = strings.Replace(surface, "\n  coop <claude|codex|gemini|grok>   start Claude, Codex, Gemini, or Grok\n", "\n", 1)
			}
			for _, retired := range []string{
				"--peer <agent>", "--peer <peer>", "[<agent>[:model]", "[target|preset]",
				"<" + strings.Join(agents.Names(), "|") + ">",
			} {
				// The approved ACP page names every selectable value <agent>: the whole page is
				// about which agent runs an editor session, and its own examples put a full
				// target in that slot. See usage-placeholder-style's same-kind-of-thing clause.
				if name == "ACP help" && retired == "--peer <agent>" {
					continue
				}
				if strings.Contains(surface, retired) {
					t.Errorf("%s uses noncanonical target placeholder %q", name, retired)
				}
			}
		}
		for name, want := range map[string]string{
			"top-level help":        "coop <target> --peer <target>...",
			"agent help":            "coop claude[:<model>][/<effort>][@<account>]",
			"ACP help":              "coop acp [<agent|preset>] [options]",
			"ACP usage error":       "coop acp [<target|preset>] [--peer <target>...]",
			"loop help":             "coop loop [<target|preset>]",
			"fork help":             "coop fork <name> [<target|preset>]",
			"fork usage error":      "coop fork <name> [<target|preset>]",
			"fork peer error":       "--peer <target>",
			"fork ACP usage error":  "coop fork work acp <target> [--readonly] [--peer <target>...]",
			"fork ACP target error": "coop fork work acp <target>",
			"loop usage error":      "coop loop [<target|preset>]",
			"peer usage error":      "--peer <target>",
			"no-provider error":     "coop loop <target|preset>",
		} {
			if !strings.Contains(surfaces[name], want) {
				t.Errorf("%s missing canonical form %q", name, want)
			}
		}
	})

	// usage-placeholder-style: optional metavariables remain visibly distinct from literal flags
	// and verbs. This pins the public help/error surfaces that previously used bare [agent], [name],
	// [paths...], and [@account] forms without pretending to parse every provider-owned argument.
	t.Run("usage_metavariables", func(t *testing.T) {
		errorText := func(name string, err error) string {
			t.Helper()
			if err == nil {
				t.Errorf("%s unexpectedly succeeded; usage error surface is untested", name)
				return ""
			}
			return err.Error()
		}
		a := newApp()
		_, presetErr := a.cmdPresets([]string{"frontier", "extra"})
		_, presetInitErr := a.presetsInit(t.TempDir(), []string{"frontier", "extra"})
		_, loginErr := a.cmdLogin(nil)
		surfaces := map[string]string{
			"top-level help":    renderHelp(a.cfg, true),
			"agent help":        agentHelp("claude"),
			"credentials help":  commandHelp["credentials"],
			"login help":        commandHelp["login"],
			"models help":       commandHelp["models"],
			"presets help":      commandHelp["presets"],
			"context help":      commandHelp["context"],
			"preset error":      errorText("extra preset argument", presetErr),
			"preset init error": errorText("extra preset init argument", presetInitErr),
			"login error":       errorText("missing login target", loginErr),
		}
		for name, surface := range surfaces {
			for _, bare := range []string{
				" [args]", " [agent]", " [credential", " [name]", "[paths...]", "[@account]",
				"[coop flags]", "<agent args>",
			} {
				if strings.Contains(surface, bare) {
					t.Errorf("%s uses bare metavariable form %q", name, bare)
				}
			}
		}
		for name, want := range map[string]string{
			"top-level help":   "Usage: coop <command> [<args>...]",
			"agent help":       "[options] [-- <claude-args>...]",
			"credentials help": "coop credentials <agent> <account> rm       remove it",
			"login help":       "coop login <agent>[@<account>]",
			// The agents are a CLOSED set of literal tokens, so the models page lists them as
			// themselves; angle brackets are for a value the user supplies.
			"models help": "coop models [" + strings.Join(agents.Names(), "|") + "]",
			// The presets page names its one slot <name>: inside a page where every value is a
			// preset, the resource-name placeholder is the plain one (usage-placeholder-style).
			"presets help":      "coop presets init [<name>]",
			"context help":      "[<path>...]",
			"preset error":      "coop presets [init] [<preset>]",
			"preset init error": "coop presets init [<name>]",
			// The approved missing-argument transcript says <agent>: the error is about the slot,
			// not the closed provider list, which the login page still spells out.
			"login error": "coop login <agent>[@<account>]",
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
			!strings.Contains(err.Error(), `Unknown command "coop tasks definitely-not-a-verb"`) {
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
