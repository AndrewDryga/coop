// Package internal_test freezes the shape of the internal dependency graph. It carries no
// production code — `internal/` is a directory of packages, not a package — only the allowlist
// below and the checks that hold the tree to it.
package internal_test

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// importPrefix is the module path every internal package hangs off; an import that starts with it
// is an internal→internal edge, and the rest of the path is the table key below.
const importPrefix = "github.com/AndrewDryga/coop/internal/"

// Every failure names BOTH of these. A new import edge is an architecture decision, and it is only
// a decision when the table and the card that explains it move in the same commit — otherwise the
// next convenient import silently becomes architecture.
const (
	tableFile = "the allowedEdges table in internal/importdag_test.go"
	cardFile  = ".agent/kb/rules/internal-import-dag.md"
)

// allowedEdges is the internal→internal import graph, frozen: one line per package, listing every
// internal package it may import, alphabetical. `nil` means a leaf that imports nothing internal.
//
// This is the whole point of the file — read it as the architecture diagram it is. cli sits at the
// top and wires the engines together, box and scaffold build on the provider spine
// (agent/preset/fusion/loopcfg), and the rest are leaves. Changing a line here is changing the
// architecture: do it deliberately, with the card.
//
// runtime→liveprocess is the one edge a default build won't show: it lives in
// process_group_live.go, behind the `cooplivetest` tag. The scan is tag-agnostic on purpose, so the
// frozen graph never depends on which tags or GOOS the gate happens to run under.
var allowedEdges = map[string][]string{
	"acpctl":                {"acpproxy", "agent", "box", "config", "ladder", "liveprocess", "preset", "processidentity"},
	"acpproxy":              nil,
	"agent":                 {"config", "mcp"},
	"box":                   {"agent", "config", "fusion", "preset", "processidentity", "project", "runtime", "ui"},
	"cli":                   {"acpctl", "acpproxy", "agent", "box", "config", "contextc", "forkspace", "fusion", "ladder", "liveprocess", "loopcfg", "preset", "project", "runtime", "scaffold", "sessionsvc", "taskstate", "ui"},
	"config":                nil,
	"contextc":              {"project"},
	"forkspace":             {"processidentity"},
	"fusion":                {"agent"},
	"ladder":                {"agent"},
	"liveprocess":           nil,
	"loopcfg":               {"agent"},
	"mcp":                   nil,
	"preset":                {"agent"},
	"processidentity":       nil,
	"project":               nil,
	"runtime":               {"liveprocess"},
	"scaffold":              {"agent", "project", "taskstate", "ui"},
	"session":               nil,
	"sessionsvc":            {"agent", "box", "config", "forkspace", "ladder", "runtime", "session"},
	"taskstate":             nil,
	"testutil/gitrepo":      nil,
	"testutil/liveprovider": {"agent", "config", "liveprocess", "processidentity", "testutil/procharness"},
	"testutil/procharness":  nil,
	"ui":                    nil,
}

// uiPresentationOwners are the only packages allowed to import internal/ui. Terminal rendering
// belongs at the edges: everything else returns data and lets its caller print it. cli owns the
// terminal outright; box narrates image builds and runs (ui.Info, ui.IsTerminal) and scaffold
// narrates what it generated (ui.Bold, ui.Detail).
//
// Deliberately a SECOND list, not derived from allowedEdges: granting a package the ui edge has to
// cost two edits, so "just add it to the table" can't quietly move presentation back into a
// library.
var uiPresentationOwners = []string{"box", "cli", "scaffold"}

// TestInternalImportDAG diffs the real tree against the frozen table in both directions: an
// unexpected edge fails, and so does an edge the table still expects but the code has dropped, so
// the table can never rot into a description of a graph that used to exist.
func TestInternalImportDAG(t *testing.T) {
	for _, problem := range diffEdges(allowedEdges, scan(t)) {
		t.Error(problem)
	}
}

// TestInternalImportDAGInvariants holds the two properties that outrank the table: they stay true
// no matter how the table is edited, so the fix for a violation is never "add a row".
func TestInternalImportDAGInvariants(t *testing.T) {
	graph := scan(t)

	// 1. Nothing imports internal/cli. cli is the top of the graph — it owns the terminal, the
	//    flags, and the wiring — so an edge back into it is how a package below starts depending
	//    on the CLI's shape. The 2026-08 plan extracts engines OUT of cli; this keeps that
	//    one-way.
	for _, pkg := range slices.Sorted(maps.Keys(graph)) {
		if graph[pkg]["cli"] {
			t.Errorf("%s imports internal/cli, which nothing may import: cli is the top of the graph, "+
				"so an edge back into it inverts the architecture. Extract the shared piece into its own "+
				"internal package. Adding the edge to %s is NOT the fix — this invariant holds independently "+
				"of the table (see %s)", pkgPath(pkg), tableFile, cardFile)
		}
	}

	// 2. internal/ui is imported only by the granted presentation owners — checked both ways, so a
	//    grant can't outlive the import it was written for.
	owners := map[string]bool{}
	for _, owner := range uiPresentationOwners {
		owners[owner] = true
	}
	for _, pkg := range slices.Sorted(maps.Keys(graph)) {
		if graph[pkg]["ui"] && !owners[pkg] {
			t.Errorf("%s imports internal/ui but is not a presentation owner: keep terminal rendering at "+
				"the edges — return data and let the caller print it. If %s really owns presentation, grant "+
				"it in uiPresentationOwners in internal/importdag_test.go AND say why in %s; the edge in %s "+
				"alone is not enough", pkgPath(pkg), pkgPath(pkg), cardFile, tableFile)
		}
	}
	for _, owner := range uiPresentationOwners {
		if !graph[owner]["ui"] {
			t.Errorf("%s is granted internal/ui in uiPresentationOwners but no longer imports it: drop the "+
				"stale grant in internal/importdag_test.go and record it in %s", pkgPath(owner), cardFile)
		}
	}
}

// TestInternalImportDAGDiff pins the diff itself against a synthetic graph — every direction it has
// to catch, proven without touching production imports.
func TestInternalImportDAGDiff(t *testing.T) {
	want := map[string][]string{"a": {"b"}, "b": nil, "c": nil}
	base := func() map[string]map[string]bool {
		return map[string]map[string]bool{"a": {"b": true}, "b": {}, "c": {}}
	}

	if problems := diffEdges(want, base()); len(problems) != 0 {
		t.Fatalf("a graph that matches the table must produce no problems, got %q", problems)
	}

	for _, tc := range []struct {
		name    string
		mutate  func(map[string]map[string]bool)
		wantAll []string // fragments the single problem must contain
	}{
		{"unexpected edge", func(g map[string]map[string]bool) { g["a"]["c"] = true },
			[]string{"internal/a", "internal/c", tableFile, cardFile}},
		{"edge gone", func(g map[string]map[string]bool) { delete(g["a"], "b") },
			[]string{"internal/a", "internal/b", tableFile, cardFile}},
		{"new package", func(g map[string]map[string]bool) { g["d"] = map[string]bool{"b": true} },
			[]string{"internal/d", `"d": {"b"},`, tableFile, cardFile}},
		{"package gone", func(g map[string]map[string]bool) { delete(g, "c") },
			[]string{"internal/c", tableFile, cardFile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := base()
			tc.mutate(got)
			problems := diffEdges(want, got)
			if len(problems) != 1 {
				t.Fatalf("want exactly 1 problem, got %d: %q", len(problems), problems)
			}
			for _, fragment := range tc.wantAll {
				if !strings.Contains(problems[0], fragment) {
					t.Errorf("problem %q does not name %q", problems[0], fragment)
				}
			}
		})
	}
}

// diffEdges compares the frozen table against a scanned graph and returns one problem per
// difference, in a stable order. Both directions are failures: an edge the code grew and an edge
// the table outlived are the same mistake — a graph nobody decided on.
func diffEdges(want map[string][]string, got map[string]map[string]bool) []string {
	names := map[string]bool{}
	for pkg := range want {
		names[pkg] = true
	}
	for pkg := range got {
		names[pkg] = true
	}

	var problems []string
	for _, pkg := range slices.Sorted(maps.Keys(names)) {
		wantDeps, inTable := want[pkg]
		gotDeps, onDisk := got[pkg]
		switch {
		case !onDisk:
			problems = append(problems, fmt.Sprintf(
				"%s is in the frozen graph but has no non-test source any more: if the package is gone for "+
					"good, delete its line from %s and record it in %s (same commit)",
				pkgPath(pkg), tableFile, cardFile))
			continue
		case !inTable:
			problems = append(problems, fmt.Sprintf(
				"%s is a new internal package the frozen graph does not know: add %s to %s and record the "+
					"decision in %s (same commit)",
				pkgPath(pkg), tableLine(pkg, slices.Sorted(maps.Keys(gotDeps))), tableFile, cardFile))
			continue
		}
		for _, dep := range slices.Sorted(maps.Keys(gotDeps)) {
			if !slices.Contains(wantDeps, dep) {
				problems = append(problems, fmt.Sprintf(
					"%s imports %s, an edge the frozen graph does not allow: if it is deliberate, add %q to "+
						"%s's line in %s and record the decision in %s (same commit) — otherwise drop the import",
					pkgPath(pkg), pkgPath(dep), dep, pkg, tableFile, cardFile))
			}
		}
		for _, dep := range wantDeps {
			if !gotDeps[dep] {
				problems = append(problems, fmt.Sprintf(
					"%s no longer imports %s, but the frozen graph still expects it: if dropping the edge is "+
						"deliberate, remove %q from %s's line in %s and record it in %s (same commit) — "+
						"otherwise the import was deleted by accident",
					pkgPath(pkg), pkgPath(dep), dep, pkg, tableFile, cardFile))
			}
		}
	}
	return problems
}

// scan reads the graph off disk. `go test ./internal` runs with this directory as the working
// directory, so the tree under "." is exactly the internal packages.
func scan(t *testing.T) map[string]map[string]bool {
	t.Helper()
	graph, err := scanImports(".")
	if err != nil {
		t.Fatalf("scanning internal/: %v", err)
	}
	return graph
}

// scanImports walks a tree of packages and returns, for every directory holding at least one
// non-test source file, the set of internal packages it imports.
//
// Deliberately stdlib-only and build-tag agnostic (go/parser, ImportsOnly): the frozen graph must
// be the same on every GOOS and under every tag combination, so an edge that exists only behind
// `cooplivetest` still counts. `_test.go` files are excluded — a test may import anything it needs
// (internal/scaffold's tests drive internal/box on purpose) — and so is `testdata`, which holds
// standalone fixture programs that import internal packages to act as independent oracles.
func scanImports(root string) (map[string]map[string]bool, error) {
	graph := map[string]map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(rel)
		if graph[pkg] == nil {
			graph[pkg] = map[string]bool{}
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range f.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return fmt.Errorf("%s: unquoting %s: %w", p, spec.Path.Value, err)
			}
			if dep, ok := strings.CutPrefix(imported, importPrefix); ok {
				graph[pkg][dep] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return graph, nil
}

// pkgPath renders a table key the way a reader of the failure sees it in the repo.
func pkgPath(pkg string) string { return path.Join("internal", pkg) }

// tableLine renders the exact allowedEdges line a new package needs, so the fix is a paste.
func tableLine(pkg string, deps []string) string {
	if len(deps) == 0 {
		return fmt.Sprintf("%q: nil,", pkg)
	}
	quoted := make([]string, len(deps))
	for i, dep := range deps {
		quoted[i] = strconv.Quote(dep)
	}
	return fmt.Sprintf("%q: {%s},", pkg, strings.Join(quoted, ", "))
}
