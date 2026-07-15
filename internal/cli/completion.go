package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/preset"
)

// coop completion bash|zsh prints a static script that defers every dynamic value to `coop __complete`
// (a hidden command), so the completion surface never becomes a fourth hand-synced copy of the
// commands/verbs — it reads them from the same code the dispatch and help do. All candidate lookups
// are local filesystem reads (fork dirs, task ids, credential profiles); nothing hits a container.

const bashCompletion = `# coop bash completion. Install: coop completion bash > /etc/bash_completion.d/coop
#   (or: coop completion bash > ~/.local/share/bash-completion/completions/coop)
_coop() {
  local IFS=$'\n'
  COMPREPLY=($(coop __complete "${COMP_WORDS[@]:1:COMP_CWORD}" 2>/dev/null))
}
complete -o default -F _coop coop
`

const zshCompletion = `#compdef coop
# coop zsh integration. Generate to "${fpath[1]}/_coop", then source that file AFTER
# compinit from .zshrc. Sourcing installs completion plus a command-local nocorrect alias.
_coop() {
  local -a cands
  cands=(${(f)"$(coop __complete "${(@)words[2,$CURRENT]}" 2>/dev/null)"})
  compadd -- $cands
}
compdef _coop coop
alias coop='nocorrect coop'
`

// cmdCompletion prints the static completion script for a shell.
func cmdCompletion(args []string) (int, error) {
	if len(args) != 1 {
		return 2, fmt.Errorf("usage: coop completion <bash|zsh>")
	}
	switch args[0] {
	case "bash":
		fmt.Print(bashCompletion)
	case "zsh":
		fmt.Print(zshCompletion)
	default:
		return 2, fmt.Errorf("coop completion: unsupported shell %q — use bash or zsh", args[0])
	}
	return 0, nil
}

// cmdComplete is the hidden `coop __complete <words...>` the shell scripts call: words are the tokens
// after `coop` up to and including the (possibly empty) word being completed. It prints the candidates
// whose prefix matches that last word, one per line. Best-effort — any lookup error just yields fewer
// candidates (exit 0), so a broken completion never blocks the shell.
func (a *app) cmdComplete(words []string) (int, error) {
	cur := ""
	prev := words
	if len(words) > 0 {
		cur = words[len(words)-1]
		prev = words[:len(words)-1]
	}
	cands := a.completionCandidatesFor(prev, cur)
	// Zsh invokes completion for an already-complete command without adding a trailing
	// empty word (`coop loop<TAB>`). Advance that exact command into its next slot so the
	// first Tab is useful; a partial word still completes against the top-level list.
	if len(prev) == 0 && cur != "" {
		if next := a.completionCandidatesFor([]string{cur}, ""); len(next) > 0 {
			cands = next
			cur = ""
		}
	}
	for _, c := range cands {
		if strings.HasPrefix(c, cur) {
			fmt.Println(c)
		}
	}
	return 0, nil
}

// completionCandidates returns the possible values for the word after prev (the already-typed args
// after `coop`). It mirrors the dispatch: top-level commands + agents first, then per-family verbs and
// the local dynamic values (fork names, task ids, profiles).
func (a *app) completionCandidates(prev []string) []string {
	return a.completionCandidatesFor(prev, "")
}

// completionCandidatesFor returns candidates for prev (the already-complete words after `coop`)
// and cur (the word currently being edited). Keeping cur here lets target completion stay useful
// without dumping every model/effort/account combination into an empty completion menu.
func (a *app) completionCandidatesFor(prev []string, cur string) []string {
	if len(prev) == 0 { // completing the command itself
		return appendCompletionCandidates(topLevelCommands, a.targetCandidates(cur, false, true), a.presetCandidates())
	}
	switch prev[0] {
	case "fork":
		if len(prev) == 1 { // a verb, or a fork to re-enter
			repo, _ := box.ResolveRepo(a.cfg.RepoOverride)
			return append(forkVerbList(), forkNames(repo)...)
		}
		if len(prev) == 2 && forkVerbList2(prev[1]) { // coop fork <verb> <name> — an existing fork
			repo, _ := box.ResolveRepo(a.cfg.RepoOverride)
			return forkNames(repo)
		}
	case "tasks":
		if len(prev) == 1 {
			return tasksVerbs
		}
		if len(prev) == 2 && taskIDVerb(prev[1]) {
			return a.taskIDs()
		}
	case "backlog":
		if len(prev) == 1 {
			return backlogVerbs
		}
		if len(prev) == 2 && (prev[1] == "rm" || prev[1] == "promote") {
			return a.backlogIDs()
		}
	case "fleet":
		if len(prev) == 1 {
			return []string{"init", "up", "down", "watch", "prune"}
		}
	case "loop":
		if len(prev) == 1 {
			return appendCompletionCandidates(
				a.targetCandidates(cur, true, true),
				a.presetCandidates(),
				[]string{"--tasks", "--peer", "--max-tasks", "--preflight", "--no-preflight", "--no-mcp", "--debug-on-fail"},
			) // `coop loop [target|preset]`; `pool` is not a command, never completed
		}
		if len(prev) > 1 && prev[len(prev)-1] == "--max-tasks" {
			return []string{"1", "2", "3", "5"}
		}
		if len(prev) > 1 && prev[len(prev)-1] == "--peer" {
			return a.targetCandidates(cur, true, false)
		}
		if len(prev) > 1 {
			return []string{"--tasks", "--peer", "--max-tasks", "--preflight", "--no-preflight", "--no-mcp", "--debug-on-fail"}
		}
	case "fusion":
		if len(prev) == 1 {
			return appendCompletionCandidates(a.targetCandidates(cur, true, true), a.presetCandidates(), []string{"--peer"})
		}
		if len(prev) > 1 && prev[len(prev)-1] == "--peer" {
			return a.targetCandidates(cur, true, false)
		}
	case "acp":
		if len(prev) == 1 {
			return appendCompletionCandidates(a.targetCandidates(cur, true, true), a.presetCandidates(), []string{"--peer"})
		}
		if len(prev) > 1 && prev[len(prev)-1] == "--peer" {
			return a.targetCandidates(cur, true, false)
		}
	case "login", "credentials", "models":
		if len(prev) == 1 {
			return agents.Names()
		}
	case "completion":
		if len(prev) == 1 {
			return []string{"bash", "zsh"}
		}
	}
	return nil
}

var effortCompletionLevels = []string{"low", "medium", "high", "xhigh", "max"}

// targetCandidates returns provider targets in the CLI's one target grammar. Model ids come from
// the existing live cache/static menu; account variants are only expanded once the user types '@'
// so the initial menu stays readable. Effort variants are likewise shown only after '/'.
func (a *app) targetCandidates(cur string, includeModels, includeAccounts bool) []string {
	wantModels := includeModels || strings.Contains(cur, ":") || strings.Contains(cur, "/")
	wantEffort := strings.Contains(cur, "/")
	wantAccounts := includeAccounts && strings.Contains(cur, "@")
	var out []string
	seen := make(map[string]bool)
	add := func(value string) {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		add(name)
		if wantAccounts {
			for _, profile := range a.cfg.Profiles(name) {
				add(name + "@" + profile)
			}
		}
		if !wantModels {
			continue
		}
		models, _, _ := a.agentModels(name, ag)
		for _, model := range models {
			base := name + ":" + model
			add(base)
			if wantAccounts {
				for _, profile := range a.cfg.Profiles(name) {
					add(base + "@" + profile)
				}
			}
			if !wantEffort || !agents.SupportsEffort(ag) {
				continue
			}
			for _, level := range effortCompletionLevels {
				effort := base + "/" + level
				add(effort)
				if wantAccounts {
					for _, profile := range a.cfg.Profiles(name) {
						add(effort + "@" + profile)
					}
				}
			}
		}
		if wantEffort && agents.SupportsEffort(ag) {
			for _, level := range effortCompletionLevels {
				effort := name + "/" + level
				add(effort)
				if wantAccounts {
					for _, profile := range a.cfg.Profiles(name) {
						add(effort + "@" + profile)
					}
				}
			}
		}
	}
	return out
}

// presetCandidates lists only names that can actually be loaded from this repo or the global
// preset directory. Broken presets remain visible so completion never hides repairable config.
func (a *app) presetCandidates() []string {
	repo, _ := box.ResolveRepo(a.cfg.RepoOverride)
	return preset.List(repo, a.cfg.GlobalPresetsDir())
}

func appendCompletionCandidates(groups ...[]string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, group := range groups {
		for _, candidate := range group {
			if candidate != "" && !seen[candidate] {
				seen[candidate] = true
				out = append(out, candidate)
			}
		}
	}
	return out
}

// forkVerbList2 reports whether v is a fork subcommand that takes a fork name (rm/merge/stop/…), so
// `coop fork <v> <TAB>` offers existing forks.
func forkVerbList2(v string) bool {
	switch v {
	case "rm", "merge", "stop", "logs", "review", "open", "path":
		return true
	}
	return false
}

// taskIDVerb reports whether a tasks subcommand takes a task id (so `coop tasks <verb> <TAB>` offers ids).
func taskIDVerb(v string) bool {
	switch v {
	case "claim", "block", "unblock", "done", "path", "rm":
		return true
	}
	return false
}

// taskIDs lists the task ids across the configured queue(s) — local reads, for `__complete`.
func (a *app) taskIDs() []string {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return nil
	}
	rels, err := taskQueues(a.cfg, repo, nil)
	if err != nil {
		return nil
	}
	var ids []string
	for _, rel := range rels {
		for _, it := range readTaskTree(filepath.Join(repo, rel)) {
			ids = append(ids, it.ID)
		}
	}
	return ids
}

// backlogIDs lists the backlog item ids across the configured queue(s) — local reads, for
// `coop backlog rm|promote <TAB>` completion.
func (a *app) backlogIDs() []string {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return nil
	}
	rels, err := taskQueues(a.cfg, repo, nil)
	if err != nil {
		return nil
	}
	var ids []string
	for _, rel := range rels {
		for _, it := range readBacklog(filepath.Join(repo, rel)) {
			ids = append(ids, it.ID)
		}
	}
	return ids
}
