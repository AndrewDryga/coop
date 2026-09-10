package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdPresets is the presets view, path-grammar like credentials: bare lists every
// preset under .agent/presets/ (a broken one shows its error instead of hiding),
// `coop presets <name>` shows one — the lead and each role with its mode/model/
// credentials/routing. Read-only: presets are edited as YAML files, not via the CLI.
func (a *app) cmdPresets(args []string) (int, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if len(args) > 0 && args[0] == "init" {
		return a.presetsInit(repo, args[1:])
	}
	if len(args) > 1 {
		return 2, fmt.Errorf("unexpected argument %q (usage: coop presets [init] [<preset>])", args[1])
	}
	if len(args) == 1 {
		if args[0] == "ls" { // rule: `ls` must lead somewhere useful, not read as a preset name
			return 2, fmt.Errorf("coop presets already lists every preset — just run `coop presets` (no %q)", args[0])
		}
		return a.showPreset(repo, args[0])
	}

	globalDir := a.cfg.GlobalPresetsDir()
	names := preset.List(repo, globalDir)
	pal := ui.For(os.Stdout) // stdout view — gate color on stdout so a pipe stays clean
	if len(names) == 0 {
		fmt.Println("no presets — scaffold the documented frontier recipe: coop presets init")
		return 0, nil
	}
	w := colWidth(names, 0, 24)
	for _, name := range names {
		// Pad the bare name to the column, THEN append the origin marker — so the dim
		// "(global)" tag (and its ANSI codes) don't throw off the column alignment.
		label := pal.Bold(padRight(name, w))
		if preset.Origin(repo, globalDir, name) {
			label += " " + pal.Dim("(global)")
		}
		p, err := preset.Load(repo, globalDir, name)
		if err != nil {
			// A broken preset must be visible in the listing — it would fail every run under it.
			fmt.Printf("  %s  %s\n", label, pal.Red("broken: "+err.Error()))
			continue
		}
		lead := p.Lead().String() // the wire form, so the summary is a target you can paste
		var roles []string
		for _, r := range p.Roles {
			roles = append(roles, fmt.Sprintf("%s (%s %s)", r.Name, r.Mode, r.Primary().Provider))
		}
		summary := pal.Dim("no roles")
		if len(roles) > 0 {
			summary = strings.Join(roles, pal.Dim(" · "))
		}
		fmt.Printf("  %s  lead %s  %s\n", label, lead, summary)
	}
	fmt.Println()
	// Same pointer the preset headers carry: say what the reader will learn, not "format".
	fmt.Println(ui.Dim("  run one by naming it: coop <name> · coop loop <name> · coop acp <name>   ·   learn how presets work: coop help presets"))
	return 0, nil
}

// presetsInit scaffolds a ready-to-edit preset from the documented frontier template
// (`coop presets init [<preset>]`, name defaulting to "frontier"). The template loads
// cleanly as written, so the new preset lists and runs immediately.
func (a *app) presetsInit(repo string, args []string) (int, error) {
	name := "frontier"
	switch {
	case len(args) > 1:
		return 2, fmt.Errorf("unexpected argument %q (usage: coop presets init [<preset>])", args[1])
	case len(args) == 1:
		name = args[0]
	}
	path, err := preset.Scaffold(repo, name)
	if err != nil {
		return 2, err
	}
	ui.OK("wrote %s (with starter prompts in roles/) — edit it, then run: coop %s", path, name)
	return 0, nil
}

// showPreset prints one preset's full recipe — the path grammar's read at preset depth.
func (a *app) showPreset(repo, name string) (int, error) {
	p, err := preset.Load(repo, a.cfg.GlobalPresetsDir(), name)
	if err != nil {
		return 2, err
	}
	fmt.Print(presetDetail(p, repo, ui.For(os.Stdout)))
	return 0, nil
}

// helpForPreset answers `coop help <name>` for a preset, through the same roots and
// repo-over-global precedence a run uses — files only, so it works with no container runtime.
// ok=false means no such preset FILE exists (the caller then reports an unknown command); a
// preset whose YAML is broken returns its validation error instead, because calling the name
// unknown would send its author hunting for a typo instead of the actual mistake.
func helpForPreset(name string, cfg *config.Config) (code int, ok bool) {
	if !preset.ValidName(name) {
		return 0, false
	}
	repo, err := box.ResolveRepo(cfg.RepoOverride)
	if err != nil {
		return 0, false
	}
	globalDir := cfg.GlobalPresetsDir()
	if _, err := os.Stat(preset.Path(repo, globalDir, name)); err != nil {
		return 0, false
	}
	p, err := preset.Load(repo, globalDir, name)
	if err != nil {
		ui.Error("%v", err)
		return 2, true
	}
	fmt.Print(presetDetail(p, repo, ui.For(os.Stdout)))
	return 0, true
}

// presetDetail renders a loaded preset for a human: what it is, how to run it, the lead's
// models, one labeled block per role, and the file to edit. It is the ONE projection behind
// both `coop presets <name>` and `coop help <name>`, so the two can never drift.
//
// Every column is measured on plain text and styled afterwards, and every path is written the
// way the reader can use it — project-relative inside the repo, ~-shortened for a global preset
// (see .agent/kb/rules/no-color-in-width-fields.md, entity-blocks-with-labeled-fields.md).
func presetDetail(p *preset.Preset, repo string, pal ui.Palette) string {
	path, global := presetPaths(repo, p.Dir)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", pal.Bold(p.Name+" — "+presetSummary(p)))

	fmt.Fprintf(&b, "%s\n", pal.Bold("Run it"))
	for _, form := range []string{"coop ", "coop loop ", "coop acp "} {
		fmt.Fprintf(&b, "  %s%s\n", form, p.Name)
	}

	lead := "Lead model"
	if len(p.LeadTargets) > 1 { // only a ladder can fall back, so only a ladder says so
		lead = "Lead models — next selected when the previous is unavailable:"
	}
	fmt.Fprintf(&b, "\n%s\n", pal.Bold(lead))
	for _, t := range p.LeadTargets {
		fmt.Fprintf(&b, "  %s\n", t.String())
	}
	if p.LeadPromptPath != "" {
		fmt.Fprintf(&b, "  %s %s\n", pal.Dim("Prompt:"), path(p.LeadPromptPath))
	}

	if len(p.Roles) > 0 {
		fmt.Fprintf(&b, "\n%s\n", pal.Bold("Roles available to the lead"))
		// One gutter for the whole preset: Mode/Agent/When/Prompt start at the same column in
		// every block, so the labels read as a column instead of a ragged edge.
		gutter := 0
		for _, r := range p.Roles {
			if n := utf8.RuneCountInString(r.Name); n > gutter {
				gutter = n
			}
		}
		indent := strings.Repeat(" ", 2+gutter+3)
		for i, r := range p.Roles {
			if i > 0 {
				fmt.Fprintln(&b)
			}
			mode := pal.Dim("Mode:") + " " + r.Mode
			if meaning := presetModeMeaning(r.Mode); meaning != "" {
				mode += pal.Dim(" — " + meaning)
			}
			fmt.Fprintf(&b, "  %s   %s\n", pal.Bold(padRight(r.Name, gutter)), mode)
			targets := make([]string, len(r.Targets))
			for j, t := range r.Targets {
				targets[j] = t.String()
			}
			fmt.Fprintf(&b, "%s%s %s\n", indent, pal.Dim("Agent:"), strings.Join(targets, ", "))
			if r.Subagent != "" {
				fmt.Fprintf(&b, "%s%s %s\n", indent, pal.Dim("Subagent:"), r.Subagent)
			}
			if len(r.When) > 0 {
				fmt.Fprintf(&b, "%s%s %s\n", indent, pal.Dim("When:"), presetWhen(r.When))
			}
			if r.PromptPath != "" {
				fmt.Fprintf(&b, "%s%s %s\n", indent, pal.Dim("Prompt:"), path(r.PromptPath))
			}
		}
	}

	fmt.Fprintf(&b, "\n%s\n  %s", pal.Bold("Edit this preset"), path("preset.yaml"))
	if global {
		b.WriteString(" " + pal.Dim("(global)"))
	}
	fmt.Fprintf(&b, "\n\nFor a guide to creating and using presets:\n  coop help presets\n")
	return b.String()
}

// presetSummary says what this preset is FOR, derived from the targets it actually names —
// never from its name, which its author chose.
func presetSummary(p *preset.Preset) string {
	providers, models := map[string]bool{}, map[string]bool{}
	targets := append([]agents.Target{}, p.LeadTargets...)
	for _, r := range p.Roles {
		targets = append(targets, r.Targets...)
	}
	for _, t := range targets {
		providers[t.Provider] = true
		if t.Model != "" {
			models[t.Model] = true
		}
	}
	switch {
	case len(providers) > 1 && len(models) > 1:
		return "a preset for multiple models and providers to work together"
	case len(providers) > 1:
		return "a preset for multiple providers to work together"
	case len(models) > 1:
		return "a preset for multiple models to work together"
	case len(p.Roles) > 0:
		return "a preset for " + titleName(p.Lead().Provider) + " to work with focused roles"
	default:
		return "a preset that runs " + titleName(p.Lead().Provider)
	}
}

// presetModeMeaning is the short human meaning printed right on a role's Mode: line — the
// editable YAML value first, then what it does. `coop help presets` carries the complete legend;
// a preset explains only the modes it uses. Delegate's meaning absorbs its two fixed invariants
// (commit: never, concurrent: never) instead of spending a row on each.
func presetModeMeaning(mode string) string {
	switch mode {
	case preset.ModeNative:
		return "runs inside the lead agent's session"
	case preset.ModeConsult:
		return "read-only advice"
	case preset.ModeDelegate:
		return "edits files; never commits; runs one at a time"
	}
	return ""
}

// presetWhen reads a role's routing hints as a phrase: the YAML tokens with their dashes
// relaxed, joined the way a person would say them. The words stay the author's.
func presetWhen(when []string) string {
	hints := make([]string, len(when))
	for i, w := range when {
		hints[i] = strings.ReplaceAll(w, "-", " ")
	}
	if len(hints) == 2 {
		return hints[0] + " and " + hints[1]
	}
	return strings.Join(hints, ", ")
}

// presetPaths renders paths inside a preset folder the way the reader can use them: relative to
// the repo for a project preset, ~-shortened for a global one (global=true, which the caller
// labels, so an origin outside the checkout is never mistaken for a file in it).
func presetPaths(repo, dir string) (path func(rel string) string, global bool) {
	base := tildeify(dir)
	global = true
	if rel, err := filepath.Rel(repo, dir); err == nil && !strings.HasPrefix(rel, "..") {
		base, global = filepath.ToSlash(rel), false
	}
	return func(rel string) string {
		if rel == "" {
			return base
		}
		return base + "/" + filepath.ToSlash(rel)
	}, global
}
