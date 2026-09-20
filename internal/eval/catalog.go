package eval

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The shipped starter suites. A public starter is a promise — that the workload is meaningful, that
// its grader is right, and that a result from it means something — so one only appears here after
// its verifiers have been run against BOTH a correct solution and each near miss they are supposed
// to reject. A suite whose grader has never been shown to reject anything is a hypothesis, not an
// eval.
//
// They are embedded in the binary rather than fetched, so a starter is the same workload on every
// machine, offline, with no dependency that can drift underneath a comparison.

//go:embed all:starters
var starterFS embed.FS

// Starter is one shipped public suite: a pinned workload a user can run without authoring anything.
type Starter struct {
	ID      string
	Summary string
	// dir is the starter's directory inside the embedded filesystem.
	dir string
}

var starters = []Starter{{
	ID:      "core",
	dir:     "starters/core",
	Summary: "three small tasks with a near miss each — a plausible answer the grader rejects",
}}

// Starters returns the qualified public starter suites, in catalog order.
func Starters() []Starter { return append([]Starter(nil), starters...) }

// StarterPath materializes a bare starter id into root and returns its suite.yaml. ok=false for
// anything not in the catalog, so the CLI can tell "unknown starter" from "a path to a custom
// suite". The copy is rewritten every time: it belongs to this binary, and a stale one left by an
// older version would silently change what a comparison measures.
func StarterPath(id, root string) (string, bool, error) {
	for _, s := range starters {
		if s.ID != id {
			continue
		}
		dest := filepath.Join(root, "starters", id)
		if err := os.RemoveAll(dest); err != nil {
			return "", true, err
		}
		if err := extractStarter(s.dir, dest); err != nil {
			return "", true, fmt.Errorf("unpack starter %q: %w", id, err)
		}
		return filepath.Join(dest, "suite.yaml"), true, nil
	}
	return "", false, nil
}

// extractStarter writes one embedded starter tree to dest. A `.sh` file is made executable: the
// embedded filesystem carries content but not modes, and a case whose instruction says to run
// ./tally.sh has to actually be runnable.
func extractStarter(src, dest string) error {
	return fs.WalkDir(starterFS, src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := starterFS.ReadFile(path)
		if err != nil {
			return err
		}
		perm := os.FileMode(0o644)
		if strings.HasSuffix(path, ".sh") {
			perm = 0o755
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, body, perm)
	})
}
