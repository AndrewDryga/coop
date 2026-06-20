package cli

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// maxScanBytes caps the file size check-secrets reads; anything larger is data, not a
// secret in source. Matches the fork-merge policy's blob limit.
const maxScanBytes = 5 << 20

// cmdCheckSecrets scans the files the box can actually see — the non-shadowed working
// tree — for secret-looking content, so a committed token is caught before the repo is
// handed to an agent. Exits non-zero on any hit (usable in CI / as a pre-flight check).
func (a *app) cmdCheckSecrets(args []string) (int, error) {
	if err := rejectArgs("check-secrets", args); err != nil {
		return 2, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	findings, err := scanVisibleTree(repo)
	if err != nil {
		return -1, err
	}
	if len(findings) == 0 {
		ui.Info("check-secrets: no secrets found in the files the box can see")
		return 0, nil
	}
	for _, f := range findings {
		fmt.Printf("  %s\n", f)
	}
	ui.Info("check-secrets: %d finding(s) — remove the secret, or if intended hide the file with a .coopignore entry", len(findings))
	return 1, nil
}

// scanVisibleTree runs the content scanner on each candidate file the box can see (see
// candidateFiles), skipping any path the box shadows. It shares box.ScanSecrets with the
// fork-merge policy and box.NewShadowDecider with the mount plan, so it flags the secrets
// an agent would see — never one already hidden.
func scanVisibleTree(repo string) ([]string, error) {
	rels, err := candidateFiles(repo)
	if err != nil {
		return nil, err
	}
	shadowed := box.NewShadowDecider(repo)
	var findings []string
	for _, rel := range rels {
		if shadowed(rel) {
			continue // hidden from the box → already protected
		}
		content, ok := readScannable(filepath.Join(repo, filepath.FromSlash(rel)))
		if !ok {
			continue // binary, oversized, or unreadable
		}
		for _, s := range box.ScanSecrets(content) {
			findings = append(findings, fmt.Sprintf("possible secret in %s:%d (%s)", rel, s.Line, s.Kind))
		}
	}
	return findings, nil
}

// candidateFiles lists the repo-relative paths worth scanning: tracked plus untracked
// files, with gitignored ones excluded — so check-secrets covers what you'd commit (and
// what a fork sees), not vendored deps or build output (node_modules, .build, dist, …).
// Falls back to a filesystem walk (skipping .git) when repo isn't a git work tree.
func candidateFiles(repo string) ([]string, error) {
	// Build the args through gitArgs so the hardening (-c core.fsmonitor=, core.hooksPath=/dev/null,
	// …) applies: ls-files refreshes the index, which would otherwise EXECUTE a poisoned repo's
	// core.fsmonitor on the host — the repo's .git is agent-writable, so a prior box run can plant
	// it. Keep the raw .Output() (not gitOut): gitOut trims and would eat the -z NUL separators, and
	// we need to tell "git failed" (→ filesystem fallback) apart from "git succeeded, empty list".
	args := gitArgs(repo, []string{"ls-files", "--cached", "--others", "--exclude-standard", "-z"})
	if out, err := exec.Command("git", args...).Output(); err == nil {
		var rels []string
		for _, p := range strings.Split(string(out), "\x00") {
			if p != "" {
				rels = append(rels, p)
			}
		}
		return rels, nil
	}
	var rels []string
	err := filepath.WalkDir(repo, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == ".git" {
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
