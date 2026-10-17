package internal_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// isolationCard is the rule TestStateTestBinariesRunUnderTeststate checks.
const isolationCard = ".agent/kb/rules/test-binaries-isolate-coop-state.md"

// coopStateRoots resolve Coop's per-user roots, so a test binary that links one can reach the
// developer's real state unless it runs under teststate.Isolate.
var coopStateRoots = []string{"internal/config", "internal/forkspace", "internal/tasks"}

// goPackage is one directory's Go sources, as much as the check needs: the Coop packages its code
// and its tests import (repository-relative), whether it has tests, and whether one calls
// teststate.Isolate.
type goPackage struct {
	imports, testImports []string
	hasTests, isolates   bool
}

// TestStateTestBinariesRunUnderTeststate is the check for isolationCard: every test binary that
// links config, forkspace or tasks, from its own code, its tests, or a dependency, calls
// teststate.Isolate. It reads imports and ignores build tags, so a link that only a tagged file
// makes counts too. Fixture programs under testdata are out of its sight.
func TestStateTestBinariesRunUnderTeststate(t *testing.T) {
	pkgs, err := scanGoPackages("..")
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) < 20 {
		t.Fatalf("scanned only %d packages; the walk lost the tree", len(pkgs))
	}
	for _, dir := range missingTeststate(pkgs) {
		t.Errorf("%s: its test binary links Coop's per-user state roots, but no test file calls "+
			"teststate.Isolate. Add `func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m.Run)) }` "+
			"(see %s)", dir, isolationCard)
	}
}

// A test binary links its package's imports and its tests' imports, and whatever those import
// outside their own tests: a dependency's tests never reach its consumers' binaries.
func TestMissingTeststateFollowsOnlyRealLinks(t *testing.T) {
	got := missingTeststate(map[string]goPackage{
		"internal/config":   {hasTests: true, isolates: true},
		"internal/mid":      {imports: []string{"internal/config"}},
		"internal/testonly": {testImports: []string{"internal/config"}, hasTests: true},
		"tools/indirect":    {imports: []string{"internal/mid"}, hasTests: true},
		"tools/throughtest": {testImports: []string{"internal/testonly"}, hasTests: true},
		"tools/isolated":    {imports: []string{"internal/mid"}, hasTests: true, isolates: true},
		"tools/untested":    {imports: []string{"internal/config"}},
	})
	if want := []string{"internal/testonly", "tools/indirect"}; !slices.Equal(got, want) {
		t.Fatalf("missingTeststate = %v, want %v", got, want)
	}
}

// missingTeststate lists, sorted, the packages whose test binary links a state root without calling
// teststate.Isolate.
func missingTeststate(pkgs map[string]goPackage) []string {
	var missing []string
	for dir, pkg := range pkgs {
		if !pkg.hasTests || pkg.isolates {
			continue
		}
		seen := map[string]bool{}
		for queue := append([]string{dir}, pkg.testImports...); len(queue) > 0; queue = queue[1:] {
			if seen[queue[0]] {
				continue
			}
			seen[queue[0]] = true
			if slices.Contains(coopStateRoots, queue[0]) {
				missing = append(missing, dir)
				break
			}
			queue = append(queue, pkgs[queue[0]].imports...)
		}
	}
	slices.Sort(missing)
	return missing
}

// scanGoPackages reads every Go package under root, skipping hidden folders and testdata.
func scanGoPackages(root string) (map[string]goPackage, error) {
	const module = "github.com/AndrewDryga/coop"
	pkgs := map[string]goPackage{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(rel)
		test := strings.HasSuffix(path, "_test.go")
		mode := parser.ImportsOnly
		if test {
			mode = 0
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, mode)
		if err != nil {
			return err
		}
		pkg := pkgs[dir]
		for _, spec := range file.Imports {
			name, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			dep, ok := strings.CutPrefix(name, module+"/")
			if name == module {
				dep, ok = ".", true
			}
			if !ok {
				continue
			}
			if test {
				pkg.testImports = append(pkg.testImports, dep)
			} else {
				pkg.imports = append(pkg.imports, dep)
			}
		}
		if test {
			pkg.hasTests = true
			ast.Inspect(file, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Isolate" {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "teststate" {
						pkg.isolates = true
					}
				}
				return !pkg.isolates
			})
		}
		pkgs[dir] = pkg
		return nil
	})
	return pkgs, err
}
