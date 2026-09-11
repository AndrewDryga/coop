// Package contextc is coop's deterministic, path-routed context compiler: given a scope (the
// repo-relative paths touched by the active work) and the repo's context.routes, it selects which
// committed instruction/rule/KB docs an agent needs — cutting context cost without ever dropping
// the canonical AGENTS.md/CLAUDE.md. It is pure file+glob work: no embeddings, model calls, or
// generated summaries, and it never infers paths from a free-form prompt (the caller passes an
// explicit, deterministic scope).
package contextc

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/project"
)

// Selected is one compiled doc: its repo-relative path and why it was included (for the report).
type Selected struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// canonicalDocs are the always-included instruction files, discovered at the repo root and never
// replaced, summarized, or budget-truncated. Fixed order for determinism; a CLAUDE.md that symlinks
// to AGENTS.md dedups to the one file (dedup is by symlink-resolved real path).
var canonicalDocs = []string{"AGENTS.md", "CLAUDE.md"}

// Compile returns the ordered, deduped docs to compile for scope (repo-relative touched paths).
// Canonical instruction files come first (whichever exist), then each route whose glob matches a
// scope path contributes its includes, in declaration order. A selected include that is missing or
// escapes the repo (directly or via a symlink) is an error; a missing canonical file is not (it's
// discovery, not a requirement). Dedup is by real path, so the same file reached two ways appears
// once, at its earliest (nearest-scope) position.
func Compile(repo string, routes []project.Route, scope []string) ([]Selected, error) {
	var out []Selected
	seen := map[string]bool{} // symlink-resolved abs path → already included

	add := func(rel, reason string) (present bool, err error) {
		real, ok, err := resolveInRepo(repo, rel)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		if !seen[real] {
			seen[real] = true
			out = append(out, Selected{File: rel, Reason: reason})
		}
		return true, nil
	}

	for _, name := range canonicalDocs {
		if _, err := add(name, "canonical"); err != nil {
			return nil, err
		}
	}
	for _, r := range routes {
		g, sp, ok := firstMatch(r.Paths, scope)
		if !ok {
			continue
		}
		for _, inc := range r.Include {
			present, err := add(inc, fmt.Sprintf("route %s → %s", g, sp))
			if err != nil {
				return nil, err
			}
			if !present {
				return nil, fmt.Errorf("context: route include %q does not exist", inc)
			}
		}
	}
	return out, nil
}

// firstMatch returns the first (glob, scopePath) pair where a glob matches, scanning globs then
// scope paths so the reason is deterministic. ok=false when nothing matches.
func firstMatch(globs, scope []string) (glob, scopePath string, ok bool) {
	for _, g := range globs {
		for _, sp := range scope {
			if matchGlob(g, sp) {
				return g, sp, true
			}
		}
	}
	return "", "", false
}

// matchGlob reports whether the slash-path name matches pattern, supporting `*` within a segment
// (via path.Match) and `**` across zero or more segments. Both are cleaned to slash form first.
func matchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(path.Clean(name), "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true // trailing ** matches any remaining segments (incl. none)
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// resolveInRepo resolves a repo-relative path to its real (symlink-followed) absolute path, and
// verifies it stays inside the repo. exists=false (no error) when the path is simply absent — a
// missing canonical file is fine. An escaping symlink IS an error: a committed route must never
// pull a file from outside the repo onto the host.
func resolveInRepo(repo, rel string) (real string, exists bool, err error) {
	abs := filepath.Join(repo, filepath.FromSlash(rel))
	if _, e := os.Lstat(abs); e != nil {
		return "", false, nil
	}
	real, e := filepath.EvalSymlinks(abs)
	if e != nil {
		return "", false, nil
	}
	root, e := filepath.EvalSymlinks(repo)
	if e != nil {
		root = repo
	}
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return "", false, fmt.Errorf("context: %q resolves outside the repo (escaping symlink)", rel)
	}
	return real, true, nil
}
