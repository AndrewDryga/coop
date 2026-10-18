// Package hostsurface names the files in a repository that change what runs on the HOST — on
// your machine, outside any sandbox — the next time you or your tools touch the checkout: a git
// hook or attribute filter that runs on your commit or checkout, an editor or agent settings file
// whose hooks run when a session starts, a compose file or Dockerfile that runs containers on your
// Docker, a Makefile that runs when you run the gate. Coop's sandbox contains what an agent does
// inside the box; it cannot contain what your own tools do with files the agent left behind. So a
// change to one of these surfaces is not proof of anything wrong — it is the place to look first.
//
// The list is deliberately about files that run AUTOMATICALLY or on an ordinary action, not every
// script in the tree: a reviewer can read `scripts/deploy.sh` when they choose to run it; they do
// not choose to run a pre-commit hook.
package hostsurface

import (
	"path"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Finding is one changed file that alters what runs on the host, with the reason a reviewer
// should read it — one sentence saying when the file runs. Automatic marks the surfaces that run WITHOUT
// a deliberate command — a hook on your next commit, a settings file your editor session loads,
// a compose file a box start runs — as opposed to a Makefile you choose to run: the former
// refuse a fork merge unless forced, the latter are listed and flagged but never block.
type Finding struct {
	Path      string
	Reason    string
	Automatic bool
}

// Key is the name a case-insensitive disk resolves a slash-separated path to: each segment in
// Unicode NFC and case-folded, with the format characters Darwin ignores dropped and the trailing
// dots and spaces Windows trims removed. Two spellings with one Key may open the same file, so a
// spelling-based safety check compares Keys, on every disk: .GITATTRIBUTES or .AGENT/compose.yml
// in a commit lands on .gitattributes or .agent/compose.yml when a Mac checks it out.
func Key(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		part = strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, part)
		parts[i] = cases.Fold().String(norm.NFC.String(strings.TrimRight(part, ". ")))
	}
	return strings.Join(parts, "/")
}

// Classify reports why a changed file at repo-relative path is a host-execution surface ("" when
// it is not) and whether it runs automatically. status is the `git diff --name-status` code
// (A/M/R/C/T…); a deletion never counts — removing a hook cannot run anything. It judges the
// path's Key, so every spelling of a surface counts.
func Classify(status, relPath string) (reason string, automatic bool) {
	if status != "" && status[0] == 'D' {
		return "", false
	}
	p := Key(strings.ReplaceAll(relPath, "\\", "/"))
	base := path.Base(p)
	dir := path.Dir(p)
	switch {
	case base == ".envrc":
		return "Runs when you enter the folder with direnv.", true
	case base == ".gitattributes":
		return "Selects Git filters and diff drivers used by your Git commands.", true
	case base == ".gitmodules":
		return "Changes sources fetched by git submodule update.", true
	case base == ".pre-commit-config.yaml":
		return "Runs on Git commits through pre-commit.", true
	case hasSegment(p, ".githooks") || hasSegment(p, ".husky") || hasSegment(p, ".git-hooks") || hasSegment(p, "githooks"):
		return "Runs during Git operations.", true
	case base == "makefile" || base == "gnumakefile":
		return "Runs when you use make.", false
	case base == "justfile" || base == "taskfile.yml" || base == "taskfile.yaml":
		return "Runs when you use just or task.", false
	case base == ".mcp.json" || strings.HasSuffix(base, ".mcp.json") || base == "mcp.json":
		return "Starts MCP server commands in your host agent session.", true
	case hasSegment(p, ".claude"):
		if base == "settings.json" || base == "settings.local.json" || hasSegment(p, "hooks") {
			return "Runs Claude Code hooks in your host session.", true
		}
		return "Provides commands or skills to your host Claude session.", true
	case hasSegment(p, ".codex") || hasSegment(p, ".gemini"):
		return "Provides agent configuration to your host sessions.", true
	case hasSegment(p, ".agent"):
		switch {
		case hasSegment(p, "skills"):
			return "Runs when your host agent uses the skill.", true
		case base == "compose.yml" || base == "compose.yaml" || base == "docker-compose.yml":
			return "Starts containers on your runtime when services start.", true
		case base == "dockerfile" || strings.HasPrefix(base, "dockerfile."):
			return "Runs build steps on your runtime when you build the box.", true
		case base == "project.yaml" || base == "loop.yaml":
			return "Changes Coop settings used on your machine.", true
		}
	case hasSegment(p, ".vscode") && (base == "tasks.json" || base == "launch.json" || base == "settings.json"):
		return "Can run commands in your VS Code session.", true
	case hasSegment(p, ".zed") && (base == "tasks.json" || base == "settings.json"):
		return "Can run commands in your Zed session.", true
	case hasSegment(p, ".idea") && strings.HasSuffix(base, ".xml"):
		return "Defines commands run by your IDE.", true
	case dir == ".github/workflows" || strings.HasPrefix(p, ".github/workflows/"):
		return "Runs on the project's CI runners.", true
	}
	return "", false
}

// Findings classifies a `git diff --name-status` (or `git show --name-status`) listing. A
// rename's NEW path is what counts. The result is sorted by path and deduplicated.
func Findings(nameStatus string) []Finding {
	seen := map[string]bool{}
	var out []Finding
	for _, line := range strings.Split(nameStatus, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) < 2 {
			fields = strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
		}
		status, p := fields[0], fields[len(fields)-1]
		reason, automatic := Classify(status, p)
		if reason == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, Finding{Path: p, Reason: reason, Automatic: automatic})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func hasSegment(p, seg string) bool {
	return strings.HasPrefix(p, seg+"/") || strings.Contains(p, "/"+seg+"/")
}
