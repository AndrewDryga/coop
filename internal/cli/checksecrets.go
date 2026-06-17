package cli

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

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

// scanVisibleTree walks repo, skipping .git and every path the box shadows, and runs the
// content secret scanner on each visible text file. It shares box.ScanSecrets with the
// fork-merge policy and box.NewShadowDecider with the mount plan, so it flags exactly the
// secrets an agent would see — never one already hidden, never missing one that isn't.
func scanVisibleTree(repo string) ([]string, error) {
	shadowed := box.NewShadowDecider(repo)
	var findings []string
	err := filepath.WalkDir(repo, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == repo {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		rel, err := filepath.Rel(repo, p)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if shadowed(relSlash) {
			if d.IsDir() {
				return fs.SkipDir // the box can't see this dir, so skip it whole
			}
			return nil // already shadowed: hidden from the box, already protected
		}
		if d.IsDir() {
			return nil
		}
		content, ok := readScannable(p)
		if !ok {
			return nil // binary, oversized, or unreadable
		}
		for _, s := range box.ScanSecrets(content) {
			findings = append(findings, fmt.Sprintf("possible secret in %s:%d (%s)", relSlash, s.Line, s.Kind))
		}
		return nil
	})
	return findings, err
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
