// Package forkspace is the fork ON-DISK CONTRACT: where a fork's workspace lives, what a fork may
// be named, the sibling state directory that holds its log/pidfile/lock, the lifecycle lock itself,
// the pidfile wire format that says who owns a fork right now, and the clone/destroy primitives
// that create and remove a workspace.
//
// Forks live in a sibling directory <repo>-forks/, one subdirectory per fork, with coop's own
// per-fork state under <repo>-forks/.coop/.
//
// It is deliberately a leaf: paths, names, files, flock, git clone, and process identity — no
// container runtime, no terminal output, no command wiring. Worker SUPERVISION (signalling a
// worker, killing it, reaping its box, orchestrating a detach) reads this contract but lives in
// internal/cli, so the fork commands, the fleet, and the sessions service can share one layout
// without any of them owning it.
package forkspace

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Suffix names the sibling directory that holds a repo's forks.
const Suffix = "-forks"

// verbs are the canonical `coop fork` subcommands — the source for did-you-mean suggestions and
// the help Usage line, so those name only real, canonically-spelled commands. "acp" is here too:
// `coop fork <name> acp` fronts a fork over ACP, so a fork literally named "acp" would shadow it.
// They live with the name rule because Reserved is what keeps a fork from shadowing one.
var verbs = map[string]bool{
	"ls": true, "review": true, "merge": true, "rm": true, "open": true,
	"logs": true, "stop": true, "path": true, "acp": true,
}

// Reserved reports whether name is off-limits for a fork (ValidName refuses it), so no fork
// can shadow a subcommand. It's verbs plus "watch" (reserved so a fork can't be confused with the
// fleet-level `coop fleet watch`). Kept separate from verbs so "watch" never leaks into a
// did-you-mean suggestion for a command that doesn't exist on `coop fork`.
func Reserved(name string) bool {
	if name == "watch" {
		return true
	}
	return verbs[name]
}

// VerbList is the reserved fork subcommands as a sorted slice, for did-you-mean matching on a
// mistyped subcommand (so it isn't silently turned into a new fork name).
func VerbList() []string {
	v := make([]string, 0, len(verbs))
	for k := range verbs {
		v = append(v, k)
	}
	sort.Strings(v)
	return v
}

// Home is the sibling directory that holds every fork of repo.
func Home(repo string) string {
	return filepath.Join(filepath.Dir(repo), filepath.Base(repo)+Suffix)
}

// Workspace is the clone directory for one named fork.
func Workspace(repo, name string) string {
	return filepath.Join(Home(repo), name)
}

// Pin keeps the confirmed directory inode open through a destructive operation.
// Lstat rejects a swapped symlink, while the open handle prevents unlink/recreate inode reuse.
func Pin(path string) (*os.File, os.FileInfo, error) {
	entry, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("workspace is not a directory")
	}
	handle, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := handle.Stat()
	if err != nil || !os.SameFile(entry, info) {
		_ = handle.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("workspace changed while opening")
	}
	return handle, info, nil
}

// SamePinned reports whether path is still the very inode Pin confirmed.
func SamePinned(path string, original os.FileInfo) bool {
	current, err := os.Lstat(path)
	return err == nil && current.IsDir() && current.Mode()&os.ModeSymlink == 0 && os.SameFile(original, current)
}

// ValidExistingName accepts path-safe references to existing forks, including names that became
// reserved after creation. ValidName adds the creation-time reserved-word policy.
func ValidExistingName(name string) bool {
	if name == "" {
		return false
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") ||
		strings.HasSuffix(name, ".lock") || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// ValidName reports whether a NEW fork may be created under this name.
func ValidName(name string) bool {
	return !Reserved(name) && ValidExistingName(name)
}

// Names lists the forks of repo (subdirectories of the forks home, skipping
// the hidden state dir).
func Names(repo string) []string {
	entries, _ := os.ReadDir(Home(repo))
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// LifecycleNames includes pidfile-only forks whose workspace was removed after a worker crash.
// They remain visible until `coop fork stop` can finish exact-owner runtime cleanup.
func LifecycleNames(repo string) []string {
	seen := map[string]bool{}
	for _, name := range Names(repo) {
		seen[name] = true
	}
	entries, _ := os.ReadDir(StateDir(repo))
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".pid")
		if ok && !entry.IsDir() && ValidExistingName(name) {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// pathExists is forkspace's own three-line existence check, so the leaf owes internal/cli nothing.
func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
