package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
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
# coop zsh completion. Install: coop completion zsh > "${fpath[1]}/_coop"  (then restart the shell)
_coop() {
  local -a cands
  cands=(${(f)"$(coop __complete ${words[2,$CURRENT]} 2>/dev/null)"})
  compadd -- $cands
}
compdef _coop coop
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
	for _, c := range a.completionCandidates(prev) {
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
	if len(prev) == 0 { // completing the command itself
		return append(append([]string{}, topLevelCommands...), agents.Names()...)
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
			return []string{"init", "up", "down", "split", "watch", "prune"}
		}
	case "loop":
		if len(prev) == 1 {
			return agents.Names() // `coop loop [agent]`; `pool` is retired (tombstoned), never completed
		}
	case "login", "credentials", "models", "fusion", "acp":
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
