package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/hostsurface"
	"github.com/AndrewDryga/coop/internal/secretscan"
	"github.com/AndrewDryga/coop/internal/ui"
)

// maxScanBytes caps the file size check-secrets reads; anything larger is data, not a
// secret in source. Matches the fork-merge policy's blob limit.
const maxScanBytes = 5 << 20

// cmdCheckSecrets scans for secret-looking content so a committed token is caught before
// the repo is handed to an agent. By default it scans the commit-candidate files (tracked +
// untracked, gitignored excluded) — but a `coop run`/`shell`/`loop` bind-mounts the WHOLE
// working tree, so a gitignored-but-not-shadowed file is still visible to the agent;
// `--include-ignored` scans that full visible tree too. Exits non-zero on any hit, and on a
// scan that could not finish (usable in CI / as a pre-flight check).
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
	// The exact invocation the user typed, so the retry advice is a command they can press up
	// and re-run rather than a narrower scan than the one that failed.
	retry := "coop check-secrets"
	if includeIgnored {
		retry += " --include-ignored"
	}

	scan, err := scanVisibleTree(repo, includeIgnored)
	if err != nil {
		return 1, reported("Could not check project files", pathReason(repo, "read", err),
			"Fix the path or permissions, then run "+retry+" again.")
	}
	if scan.checked == 0 && len(scan.unreadable) == 0 {
		// Zero findings is not the same answer as zero files: a scan that had nothing to read
		// proves nothing, and saying "no secrets found" here would be false assurance.
		ui.Note("No files to check.")
		return 0, nil
	}

	// Detection is pure and happens above; the reviewed exceptions are applied HERE, in this one
	// check, and nowhere else — a fork merge, a checkpoint upload and session redaction keep
	// seeing every finding, because an in-repository note is not permission to move a secret.
	exceptions, err := secretscan.LoadExceptions(filepath.Join(repo, secretscan.ExceptionsFile))
	if err != nil {
		var bad *secretscan.ExceptionsError
		action := "Fix the file permissions, then run " + retry + " again."
		if errors.As(err, &bad) && bad.Line > 0 {
			action = "Fix the entry, then run " + retry + " again."
		}
		return 1, reported("Could not read "+secretscan.ExceptionsFile, err.Error(), action)
	}
	var kept []scanFinding
	ignored := 0
	// Whether anything has been said yet, so the blocks below are separated by one blank line
	// without the first of them opening on one.
	said := false
	for _, f := range scan.findings {
		if exceptions.Excuses(f.Fingerprint) {
			ignored++
			continue
		}
		kept = append(kept, f)
	}

	switch {
	case len(kept) > 0:
		reportFindings(kept)
		said = true
	case len(scan.unreadable) > 0:
		// The incomplete-scan block below is the whole result: a clean line above it would read
		// as a verdict on files nobody managed to read.
	case ignored > 0:
		ui.OK("No new possible secrets found")
		ui.Note("  %s ignored by %s.", ui.Count(ignored, "finding"), secretscan.ExceptionsFile)
		said = true
	default:
		ui.OK("No possible secrets found")
		ui.Note("  %s", scanScope(includeIgnored, scan.git))
		said = true
	}

	// A clean default scan can be false assurance: a box bind-mounts the WHOLE tree, so a
	// gitignored-but-not-shadowed file is still readable by the agent yet skipped here. Say how
	// many such files --include-ignored would add, so the default's blind spot is visible.
	if !includeIgnored {
		if n := unscannedIgnoredCount(repo); n > 0 {
			blankBefore(&said)
			warnBlock(fmt.Sprintf("%s were not checked", ui.Count(n, "Git-ignored file")),
				"The box can read them.",
				"Check them with coop check-secrets --include-ignored.")
		}
	}
	// Independent of secrets: which files changed since the last commit alter what runs on YOUR
	// machine — a hook, a settings file, a compose file, the Makefile. The sandbox contains the
	// box; it cannot contain what your own tools do with files an agent left behind, so this is
	// the "read these before running anything here" list. Informational: it never fails the scan.
	if surfaces := hostSurfacesChanged(repo); len(surfaces) > 0 {
		blankBefore(&said)
		ui.Note("%s", ui.Bold("Review files that run commands"))
		for _, finding := range surfaces {
			ui.Note("  %s", finding.Path)
			ui.Note("    %s", finding.Reason)
			ui.Note("")
		}
		ui.Note("  Read these changes before running your tools.")
	}

	if len(scan.unreadable) > 0 {
		blankBefore(&said)
		actions := []string{"Fix the file permissions, then run " + retry + " again."}
		if len(kept) == 0 {
			actions = append(actions, "No possible secrets were found in the files that could be checked.")
		}
		failBlock("Could not finish checking for secrets", strings.Join(scan.unreadable, "\n"), actions...)
		if ignored > 0 {
			blankBefore(&said)
			ui.Note("%s ignored by %s.", ui.Count(ignored, "finding"), secretscan.ExceptionsFile)
		}
		return 1, ui.ErrReported
	}
	if len(kept) > 0 {
		return 1, ui.ErrReported
	}
	return 0, nil
}

// blankBefore separates one block from the last thing printed — and prints nothing when this is
// the first thing said, so a report never opens on an empty line.
func blankBefore(said *bool) {
	if *said {
		ui.Note("")
	}
	*said = true
}

// scanScope names what the scan actually covered, so a clean result can be read for what it is.
func scanScope(includeIgnored, usedGit bool) string {
	switch {
	case includeIgnored:
		return "Checked project files, including Git-ignored files the box can read."
	case !usedGit:
		return "Checked project files using a directory scan."
	default:
		return "Checked commit-candidate files and Coop's task files and notes."
	}
}

// reportFindings prints the findings and the one copyable block that turns a reviewed false
// positive into a .coopsecretsignore entry. The value never appears: the report names where it
// is, the entry names which finding it is, and neither carries the credential itself.
func reportFindings(findings []scanFinding) {
	failBlock(ui.Count(len(findings), "possible secret")+" found", "")
	for _, f := range findings {
		ui.Note("")
		ui.Note("  %s:%d", f.Path, f.Line)
		ui.Note("    %s", f.Label())
		if f.shadowed {
			ui.Note("    Hidden from the box, but Git would commit this file.")
		}
	}
	ui.Note("")
	ui.Note("Remove real secrets from files you intend to commit.")
	if len(findings) == 1 {
		ui.Note("If this is a false positive, add this entry to %s", secretscan.ExceptionsFile)
	} else {
		ui.Note("For false positives, copy the matching entries to %s", secretscan.ExceptionsFile)
	}
	ui.Note("and replace <reason> with an explanation:")
	for _, f := range findings {
		ui.Note("")
		ui.Note("  # %s — %s", f.Path, f.Label())
		ui.Note("  %s # <reason>", f.Fingerprint)
	}
}

// scanFinding is one finding with the file it came from and whether that file is one coop hides
// from the box but git would still commit.
type scanFinding struct {
	secretscan.SecretFinding
	Path     string
	shadowed bool
}

// treeScan is one pass over the project: what was found, what could not be read, how many files
// were actually examined, and whether git chose the candidates.
type treeScan struct {
	findings   []scanFinding
	unreadable []string // one sentence per file, in scan order
	checked    int      // files whose content was actually examined
	git        bool     // the candidate list came from git, not a directory walk
}

// unscannedIgnoredCount counts files the box can read but the default scan skipped — gitignored
// files (in the full visible tree but not the commit-candidate set) that aren't shadowed. It tells
// the user how much surface --include-ignored would add, so a clean default scan isn't false
// assurance. Returns 0 when git is unavailable (both sets fall back to the same full walk).
func unscannedIgnoredCount(repo string) int {
	all, _, err1 := candidateFiles(repo, true)
	scanned, _, err2 := candidateFiles(repo, false)
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

// scanVisibleTree scans candidateFiles, skipping only hidden noncandidates. It shares
// detectors with the fork-merge policy and box.NewShadowDecider with the mount plan.
// Hidden commit candidates still need scanning: hiding protects the box, not a push.
// includeIgnored widens the candidate set to the full tree (gitignored files included),
// but hidden noncandidates stay excluded.
//
// A file it could not READ is recorded, not skipped: an intentionally skipped binary or oversized
// blob is a decision, while a permission error is a hole in the answer, and reporting a clean
// scan over a hole is the failure mode this exists to prevent.
func scanVisibleTree(repo string, includeIgnored bool) (treeScan, error) {
	rels, usedGit, err := candidateFiles(repo, includeIgnored)
	if err != nil {
		return treeScan{}, err
	}
	scan := treeScan{git: usedGit}
	shadowed := box.NewShadowDecider(repo)
	committable := commitCandidateSet(repo)
	for _, rel := range rels {
		// Built-in names and explicit .coopignore rules both protect only the box.
		// Every commit candidate is scanned; only an exact reviewed finding may be excused.
		// A shadowed file git would not commit
		// (gitignored, or coop's own .agent/ state) is protected on both sides and skipped.
		hidden := shadowed(rel)
		if hidden && !committable[rel] {
			continue
		}
		content, status := readScannable(filepath.Join(repo, filepath.FromSlash(rel)))
		switch status.kind {
		case scanUnreadable:
			scan.unreadable = append(scan.unreadable, fmt.Sprintf("%s could not be read: %s.", rel, status.cause))
			continue
		case scanSkipped:
			continue // binary or oversized: a deliberate exclusion, and a quiet one
		}
		scan.checked++
		for _, s := range secretscan.ScanFile(rel, content) {
			scan.findings = append(scan.findings, scanFinding{SecretFinding: s, Path: rel, shadowed: hidden})
		}
	}
	return scan, nil
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
// full walk when repo isn't a git work tree; usedGit reports which list the caller got, so
// the result can say what it actually covered.
func candidateFiles(repo string, includeIgnored bool) (rels []string, usedGit bool, err error) {
	if !includeIgnored {
		// Build the args through gitArgs so the hardening (-c core.fsmonitor=, core.hooksPath=/dev/null,
		// …) applies: ls-files refreshes the index, which would otherwise EXECUTE a poisoned repo's
		// core.fsmonitor on the host — the repo's .git is agent-writable, so a prior box run can plant
		// it. Keep the raw .Output() (not gitOut) so "git failed" (→ filesystem fallback) stays
		// distinct from "git succeeded, empty list"; gitOut deliberately collapses both to "".
		if out, err := gitOutputBytes(repo, "ls-files", "--cached", "--others", "--exclude-standard", "-z"); err == nil {
			for _, p := range strings.Split(string(out), "\x00") {
				if p != "" {
					rels = append(rels, p)
				}
			}
			// Also scan coop's own gitignored .agent/ working state — the box reads it (the task
			// queue + backlog + agent notes), so a secret pasted into a task's .agent/.../log.md or
			// state.md is a real exposure the commit-candidate set (gitignored excluded) misses.
			rels = append(rels, ignoredAgentFiles(repo)...)
			return rels, true, nil
		}
		// git unavailable / not a work tree → fall through to the full walk (covers .agent too).
	}
	rels, err = walkVisibleTree(repo)
	return rels, false, err
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

// scanStatus separates the two reasons a file yields no content: coop chose not to read it, or
// coop could not. Only the second one is a hole in the result.
type scanStatus struct {
	kind  int
	cause string
}

const (
	scanRead = iota
	scanSkipped
	scanUnreadable
)

// readScannable returns a file's text for scanning. An oversized or binary file is skipped on
// purpose and quietly; anything coop failed to open or read comes back as unreadable, with the
// OS cause, so the command can say the scan did not finish.
func readScannable(path string) (string, scanStatus) {
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", scanStatus{kind: scanSkipped} // deleted between listing and read; nothing to judge
	case err != nil:
		return "", scanStatus{kind: scanUnreadable, cause: osCause(err)}
	case !fi.Mode().IsRegular():
		return "", scanStatus{kind: scanSkipped} // a socket or device holds no committed secret
	case fi.Size() > maxScanBytes:
		return "", scanStatus{kind: scanSkipped}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", scanStatus{kind: scanUnreadable, cause: osCause(err)}
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", scanStatus{kind: scanSkipped}
	}
	return string(data), scanStatus{kind: scanRead}
}

// osCause is the bare reason an OS error carries — "permission denied" — without the syscall and
// absolute path Go wraps around it; the caller already named the repo-relative file.
func osCause(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
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
