package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
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
	if len(args) > 1 {
		return 2, fmt.Errorf("unexpected argument %q (usage: coop presets [name])", args[1])
	}
	if len(args) == 1 {
		if args[0] == "ls" { // rule: `ls` must lead somewhere useful, not read as a preset name
			return 2, fmt.Errorf("coop presets already lists every preset — just run `coop presets` (no %q)", args[0])
		}
		return a.showPreset(repo, args[0])
	}

	names := preset.List(repo)
	pal := ui.For(os.Stdout) // stdout view — gate color on stdout so a pipe stays clean
	if len(names) == 0 {
		fmt.Println("no presets — create " + preset.Dir + "/<name>/preset.yaml (see 'coop help presets')")
		return 0, nil
	}
	w := colWidth(names, 0, 24)
	for _, name := range names {
		p, err := preset.Load(repo, name)
		if err != nil {
			// A broken preset must be visible in the listing — it would fail every --preset run.
			fmt.Printf("  %s  %s\n", pal.Bold(padRight(name, w)), pal.Red("broken: "+err.Error()))
			continue
		}
		lead := p.LeadAgent
		if p.LeadModel != "" {
			lead += "/" + p.LeadModel
		}
		var roles []string
		for _, r := range p.Roles {
			roles = append(roles, fmt.Sprintf("%s (%s %s)", r.Name, r.Mode, r.Agent))
		}
		summary := pal.Dim("no roles")
		if len(roles) > 0 {
			summary = strings.Join(roles, pal.Dim(" · "))
		}
		fmt.Printf("  %s  lead %s  %s\n", pal.Bold(padRight(name, w)), lead, summary)
	}
	fmt.Println()
	fmt.Println(ui.Dim("  run one: coop <agent>|loop|fusion|acp --preset <name>   ·   format: coop help presets"))
	return 0, nil
}

// showPreset prints one preset's full recipe — the path grammar's read at preset depth.
func (a *app) showPreset(repo, name string) (int, error) {
	p, err := preset.Load(repo, name)
	if err != nil {
		return 2, err
	}
	pal := ui.For(os.Stdout)
	fmt.Println(pal.Bold(name) + pal.Dim("  ("+preset.Path(repo, name)+")"))
	lead := fmt.Sprintf("  %s  %s", pal.Bold(padRight("lead", 10)), p.LeadAgent)
	if p.LeadModel != "" {
		lead += pal.Dim("  model ") + p.LeadModel
	}
	if len(p.LeadCredentials) > 0 {
		lead += pal.Dim("  credentials ") + strings.Join(p.LeadCredentials, ",")
	}
	if p.LeadPromptText != "" {
		lead += pal.Dim("  +lead.md")
	}
	fmt.Println(lead)
	for _, r := range p.Roles {
		line := fmt.Sprintf("  %s  %s %s", pal.Bold(padRight(r.Name, 10)), r.Mode, r.Agent)
		if r.Subagent != "" {
			line += pal.Dim("  @") + r.Subagent
		}
		if r.Model != "" {
			line += pal.Dim("  model ") + r.Model
		}
		if len(r.Credentials) > 0 {
			line += pal.Dim("  credentials ") + strings.Join(r.Credentials, ",")
		}
		if len(r.When) > 0 {
			line += pal.Dim("  for: " + strings.Join(r.When, ", "))
		}
		if r.PromptText != "" {
			line += pal.Dim("  +md")
		}
		fmt.Println(line)
	}
	fmt.Println()
	fmt.Println(ui.Dim("  run it: coop " + p.LeadAgent + " --preset " + name + "   ·   coop loop --preset " + name))
	return 0, nil
}
