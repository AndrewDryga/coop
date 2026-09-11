package cli

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/hostsurface"
	"github.com/AndrewDryga/coop/internal/ui"
)

// maxScanBytes caps the file size check-secrets reads; anything larger is data, not a
// secret in source. Matches the fork-merge policy's blob limit.
const maxScanBytes = 5 << 20

// cmdCheckSecrets scans for secret-looking content so a committed token is caught before
// the repo is handed to an agent. By default it scans the commit-candidate files (tracked +
// untracked, gitignored excluded) — but a `coop run`/`shell`/`loop` bind-mounts the WHOLE
// working tree, so a gitignored-but-not-shadowed file is still visible to the agent;
// `--include-ignored` scans that full visible tree too. Exits non-zero on any hit (usable in
// CI / as a pre-flight check).
func (a *app) cmdCheckSecrets(args []string) (int, error) {
	includeIgnored := false
	var rest []string
	for _, x := range args {
		switch x {
		case "--include-ignored":
			includeIgnored = true
		default:
			rest = append(rest, x)
		}
	}
	if err := rejectArgs("check-secrets", rest); err != nil {
		return 2, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	findings, err := scanVisibleTree(repo, includeIgnored)
	if err != nil {
		return -1, err
	}
	scope := "commit-candidate files + coop's .agent/ state (other gitignored paths excluded)"
	if includeIgnored {
		scope = "the full visible tree (including gitignored files)"
	}
	// A clean default scan can be false assurance: a box bind-mounts the WHOLE tree, so a
	// gitignored-but-not-shadowed file is still readable by the agent yet skipped here. Note how
	// many such files --include-ignored would add, so the user knows the default's blind spot.
	noteUnscanned := func() {
		if !includeIgnored {
			if n := unscannedIgnoredCount(repo); n > 0 {
				ui.Warn("%s not scanned, but a box can read them anyway — rescan with --include-ignored to cover them", ui.Count(n, "gitignored file"))
			}
		}
	}
	// Independent of secrets: which files changed since the last commit alter what runs on YOUR
	// machine — a hook, a settings file, a compose file, the Makefile. The sandbox contains the
	// box; it cannot contain what your own tools do with files an agent left behind, so this is
	// the "read these before running anything here" list. Informational: the exit code is the
	// secret scan's.
	if surfaces := hostSurfacesChanged(repo); len(surfaces) > 0 {
		ui.Warn("%s changed since the last commit alter what runs on your machine — read them before running tools here:", ui.Count(len(surfaces), "file", "files"))
		for _, finding := range surfaces {
			ui.Detail("%s — %s", finding.Path, finding.Reason)
		}
	}
	if len(findings) == 0 {
		ui.OK("no secrets found — scanned %s", scope)
		noteUnscanned()
		return 0, nil
	}
	for _, f := range findings {
		fmt.Printf("  %s\n", f)
	}
	ui.Error("%s found in %s — remove them, or hide an intended file with a .coopignore entry", ui.Count(len(findings), "secret"), scope)
	noteUnscanned()
	return 1, nil
}

// unscannedIgnoredCount counts files the box can read but the default scan skipped — gitignored
// files (in the full visible tree but not the commit-candidate set) that aren't shadowed. It tells
// the user how much surface --include-ignored would add, so a clean default scan isn't false
// assurance. Returns 0 when git is unavailable (both sets fall back to the same full walk).
func unscannedIgnoredCount(repo string) int {
	all, err1 := candidateFiles(repo, true)
	scanned, err2 := candidateFiles(repo, false)
	if err1 != nil || err2 != nil {
		return 0
	}
	inScan := make(map[string]bool, len(scanned))
	for _, r := range scanned {
		inScan[r] = true
	}
	shadowed := box.NewShadowDecider(repo)
	n := 0
	for _, r := range all {
		if inScan[r] || shadowed(r) {
			continue // already scanned by default (incl. coop's .agent/ state), or secret-shadowed
		}
		n++ // box-visible, user-gitignored, unprotected → a real blind spot
	}
	return n
}

// scanVisibleTree runs the content scanner on each candidate file the box can see (see
// candidateFiles), skipping any path the box shadows. It shares box.ScanSecrets with the
// fork-merge policy and box.NewShadowDecider with the mount plan, so it flags the secrets
// an agent would see — never one already hidden. includeIgnored widens the candidate set
// from commit-candidate files to the full visible tree (gitignored files included).
func scanVisibleTree(repo string, includeIgnored bool) ([]string, error) {
	rels, err := candidateFiles(repo, includeIgnored)
	if err != nil {
		return nil, err
	}
	shadowed := box.NewShadowDecider(repo)
	coopignored := box.NewCoopignoreDecider(repo)
	committable := commitCandidateSet(repo)
	var findings []string
	for _, rel := range rels {
		if coopignored(rel) {
			continue // the user's explicit hide rule — an intended file, silenced on purpose
		}
		// A file coop shadows by NAME (an id_ed25519, a *.pem) is hidden from the box, but that
		// protects only the box: when git would commit it, the push leaks it just the same, so
		// it is scanned like any other commit candidate. A shadowed file git would not commit
		// (gitignored, or coop's own .agent/ state) is protected on both sides and skipped.
		byName := shadowed(rel)
		if byName && !committable[rel] {
			continue
		}
		content, ok := readScannable(filepath.Join(repo, filepath.FromSlash(rel)))
		if !ok {
			continue // binary, oversized, or unreadable
		}
		for _, s := range box.ScanSecrets(content) {
			finding := fmt.Sprintf("possible secret in %s:%d (%s)", rel, s.Line, s.Kind)
			if byName {
				finding += " — shadowed from the box by name, but git would commit it"
			}
			findings = append(findings, finding)
		}
	}
	return findings, nil
}

// commitCandidateSet is the set of files git would commit (tracked + untracked, gitignored
// excluded). Empty when git is unavailable or repo is not a work tree: nothing is then known to
// be committable, so a shadowed file stays skipped as before.
func commitCandidateSet(repo string) map[string]bool {
	out, err := gitOutputBytes(repo, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			set[p] = true
		}
	}
	return set
}

// candidateFiles lists the repo-relative paths worth scanning. The default is the
// commit-candidate set: tracked plus untracked files, gitignored ones excluded — what
// you'd commit (and what a fork sees), not vendored deps or build output. includeIgnored
// instead walks the full working tree, since a `coop run`/`shell`/`loop` bind-mounts the
// whole tree and a gitignored file is still visible to the agent. Both fall back to the
// full walk when repo isn't a git work tree.
func candidateFiles(repo string, includeIgnored bool) ([]string, error) {
	if !includeIgnored {
		// Build the args through gitArgs so the hardening (-c core.fsmonitor=, core.hooksPath=/dev/null,
		// …) applies: ls-files refreshes the index, which would otherwise EXECUTE a poisoned repo's
		// core.fsmonitor on the host — the repo's .git is agent-writable, so a prior box run can plant
		// it. Keep the raw .Output() (not gitOut) so "git failed" (→ filesystem fallback) stays
		// distinct from "git succeeded, empty list"; gitOut deliberately collapses both to "".
		if out, err := gitOutputBytes(repo, "ls-files", "--cached", "--others", "--exclude-standard", "-z"); err == nil {
			var rels []string
			for _, p := range strings.Split(string(out), "\x00") {
				if p != "" {
					rels = append(rels, p)
				}
			}
			// Also scan coop's own gitignored .agent/ working state — the box reads it (the task
			// queue + backlog + agent notes), so a secret pasted into a task's .agent/.../log.md or
			// state.md is a real exposure the commit-candidate set (gitignored excluded) misses.
			rels = append(rels, ignoredAgentFiles(repo)...)
			return rels, nil
		}
		// git unavailable / not a work tree → fall through to the full walk (covers .agent too).
	}
	return walkVisibleTree(repo)
}

// ignoredAgentFiles lists the gitignored files under .agent/ — coop's own working state (the task
// queue, logs, notes) that `coop init` ignores but a box still reads. check-secrets scans them by
// default, so a secret pasted into agent prose isn't a silent, box-readable leak. Empty when git is
// unavailable or .agent/ has no ignored files (e.g. a repo that doesn't gitignore it — then they're
// already untracked/tracked and in the default set). .agent/kb/rules + .agent/skills are tracked, so
// they arrive via --cached, not here.
func ignoredAgentFiles(repo string) []string {
	out, err := gitOutputBytes(repo, "ls-files", "--others", "--ignored", "--exclude-standard", "-z", "--", ".agent")
	if err != nil {
		return nil
	}
	var rels []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			rels = append(rels, p)
		}
	}
	return rels
}

// skipScanDir is the set of directory names a full-tree scan prunes: .git plus the obvious
// dependency/build trees that are gitignored anyway and would only drown the scan in noise.
var skipScanDir = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "deps": true,
	"_build": true, "build": true, "dist": true, "target": true,
	".venv": true, "venv": true, ".tox": true, "__pycache__": true,
	".next": true, ".cache": true,
}

// walkVisibleTree lists every repo-relative file, pruning .git and the obvious
// dependency/build directories (see skipScanDir) — so a full-tree scan reaches gitignored
// secrets without walking thousands of vendored files.
func walkVisibleTree(repo string) ([]string, error) {
	var rels []string
	err := filepath.WalkDir(repo, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if skipScanDir[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(repo, p)
		if err != nil {
			return err
		}
		rels = append(rels, filepath.ToSlash(rel))
		return nil
	})
	return rels, err
}

// readScannable returns a file's text for scanning, or ok=false if it's oversized,
// binary, or unreadable.
func readScannable(path string) (string, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() > maxScanBytes {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(data, 0) >= 0 {
		return "", false
	}
	return string(data), true
}

// hostSurfacesChanged classifies the working tree's changes since HEAD (staged, unstaged, and
// untracked, as `git status` sees them) as host-execution surfaces. Empty outside a git work tree.
func hostSurfacesChanged(repo string) []hostsurface.Finding {
	out, err := gitOutputBytes(repo, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return nil
	}
	var listing strings.Builder
	for _, entry := range strings.Split(string(out), "\x00") {
		if len(entry) < 4 {
			continue
		}
		status, path := entry[:2], entry[3:]
		code := "M"
		switch {
		case status == "??":
			code = "A"
		case strings.ContainsRune(status, 'D'):
			code = "D"
		case strings.ContainsRune(status, 'A'):
			code = "A"
		}
		listing.WriteString(code + "\t" + path + "\n")
	}
	return hostsurface.Findings(listing.String())
}
