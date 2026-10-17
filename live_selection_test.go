package main

import (
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// A test that exists only under the providerlivee2e tag runs under no ordinary
// `go test`. The deterministic ones must be selected by `make live-process-control`; the ones that
// need a real provider (named …LiveCompatibility, with …LiveChild subprocesses) run only on demand.
// A rename, or a tagged file in a package the target does not list, would drop a deterministic test
// silently, so every tagged test must be one or the other. The target runs inside `make check`, so a
// provider test its prefixes happen to match would spend money in the free gate.
var liveOnlyName = regexp.MustCompile(`(LiveCompatibility|LiveChild)$`)

func TestTaggedTestsRunUnderLiveProcessControl(t *testing.T) {
	problems, err := liveSelectionProblems(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) > 0 {
		t.Fatalf("live-process-control and the tagged tests disagree — give a deterministic test a prefix in a listed package, name one that needs a real provider …LiveCompatibility, or narrow a prefix that selects one:\n%s", strings.Join(problems, "\n"))
	}
}

func TestLiveSelectionProblemsFindWhatTheTargetMisses(t *testing.T) {
	root := t.TempDir()
	write := fixtureWriter(t, root)
	write("Makefile", "live-process-control: ## x\n"+
		"\t@go test -race -tags providerlivee2e,acpe2e -run '^Test(LiveA|LiveB)' -count=1 ./internal/a/ ./internal/b/\n"+
		"provider-live-e2e: ## y\n\t@go test -run \"^TestLiveC\" ./internal/c/\n")
	write("internal/a/a_live_test.go", "// A header comment above the build line.\n\n//go:build providerlivee2e && !windows\n\npackage a\n\n"+
		"func TestLiveAWorks(t *testing.T)                  {}\n"+
		"func TestLiveAWorksLiveCompatibility(t *testing.T) {}\n"+
		"func TestProviderLiveCompatibility(t *testing.T)   {}\n"+
		"func TestProviderLiveChild(t *testing.T)           {}\n"+
		"func TestMain(m *testing.M)                        {}\n"+
		"func TestRenamedDeterministic(t *testing.T)        {}\n")
	write("internal/a/a_noacp_live_test.go", "//go:build providerlivee2e && !acpe2e\n\npackage a\n\nfunc TestLiveANotBuilt(t *testing.T) {}\n")
	write("internal/a/a_default_test.go", "//go:build !providerlivee2e\n\npackage a\n\nfunc TestUntagged(t *testing.T) {}\n")
	write("internal/b/b_test.go", "package b\n\nfunc TestLiveBPlain(t *testing.T) {}\n")
	write("internal/b/b_live_test.go", "//go:build providerlivee2e\n\npackage b\n\nfunc TestLiveBHelperLiveChild(t *testing.T) {}\n")
	write("internal/c/c_live_test.go", "//go:build providerlivee2e\n\npackage c\n\nfunc TestLiveAUnlisted(t *testing.T) {}\nfunc TestLiveAUnlistedLiveCompatibility(t *testing.T) {}\n")
	write("internal/a/testdata/x_live_test.go", "//go:build providerlivee2e\n\npackage x\n\nfunc TestIgnored(t *testing.T) {}\n")
	write(".hidden/x_live_test.go", "//go:build providerlivee2e\n\npackage x\n\nfunc TestIgnored(t *testing.T) {}\n")

	problems, err := liveSelectionProblems(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"internal/a: TestLiveANotBuilt is in a_noacp_live_test.go, which live-process-control's -tags do not build",
		"internal/a: TestLiveAWorksLiveCompatibility needs a real provider, but live-process-control selects it",
		"internal/a: TestRenamedDeterministic matches no live-process-control prefix",
		"internal/b: TestLiveBHelperLiveChild needs a real provider, but live-process-control selects it",
		"internal/b: listed by live-process-control but has no selected tagged test",
		"internal/c: TestLiveAUnlisted is in a package live-process-control does not list",
	}
	if !slices.Equal(problems, want) {
		t.Fatalf("problems =\n%s\nwant\n%s", strings.Join(problems, "\n"), strings.Join(want, "\n"))
	}
}

func TestLiveProcessControlSelectionRejectsAnUnreadableTarget(t *testing.T) {
	const liveTags = "-tags providerlivee2e"
	for name, makefile := range map[string]string{
		"no target":        "check:\n\t@go test " + liveTags + " -run '^TestX' ./internal/x/\n",
		"double quotes":    "live-process-control:\n\t@go test " + liveTags + " -run \"^TestX\" ./internal/x/\n",
		"after the recipe": "live-process-control:\n\t@echo ok\n\nother:\n\t@go test " + liveTags + " -run '^TestX' ./internal/x/\n",
		"no packages":      "live-process-control:\n\t@go test " + liveTags + " -run '^TestX' ./...\n",
		"no live tag":      "live-process-control:\n\t@go test -tags acpe2e -run '^TestX' ./internal/x/\n",
		"a skip":           "live-process-control:\n\t@go test " + liveTags + " -run '^TestX' -skip '^TestXY' ./internal/x/\n",
		"a second run":     "live-process-control:\n\t@go test " + liveTags + " -run '^TestX' ./internal/x/\n\t@go test " + liveTags + " ./internal/y/\n",
		"two run lines":    "live-process-control:\n\t@go test " + liveTags + " -run '^TestX' ./internal/x/\n\t@go test -run '^TestLive' ./internal/y/\n",
		"a run past blank and comment lines": "live-process-control:\n\t@go test " + liveTags + " -run '^TestX' ./internal/x/\n\n# Make reads on\n" +
			"\t@go test " + liveTags + " ./internal/y/\n",
		"a subtest pattern": "live-process-control:\n\t@go test " + liveTags + " -run '^TestX/sub' ./internal/x/\n",
	} {
		if _, err := targetSelection(makefile, "live-process-control", "providerlivee2e"); err == nil {
			t.Errorf("%s: selection was read from %q", name, makefile)
		}
	}
}

// liveSelectionProblems lists the tagged tests under root that `make live-process-control` does not
// run and that are not named as needing a real provider, plus the packages the target lists that have
// no selected tagged test (a stale list, or a scan that found nothing).
func liveSelectionProblems(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return nil, err
	}
	run, err := targetSelection(string(data), "live-process-control", "providerlivee2e")
	if err != nil {
		return nil, err
	}
	files, err := taggedTestFiles(root, []string{"providerlivee2e"})
	if err != nil {
		return nil, err
	}
	selected := map[string]bool{}
	var problems []string
	for _, f := range files {
		built, err := run.builds(f)
		if err != nil {
			return nil, err
		}
		listed := slices.Contains(run.pkgs, f.pkg)
		for _, name := range f.tests {
			matched := run.pattern.MatchString(name)
			switch {
			case liveOnlyName.MatchString(name):
				if matched && listed && built {
					problems = append(problems, f.pkg+": "+name+" needs a real provider, but live-process-control selects it")
				}
			case !matched:
				problems = append(problems, f.pkg+": "+name+" matches no live-process-control prefix")
			case !listed:
				problems = append(problems, f.pkg+": "+name+" is in a package live-process-control does not list")
			case !built:
				problems = append(problems, f.pkg+": "+name+" is in "+f.name+", which live-process-control's -tags do not build")
			default:
				selected[f.pkg] = true
			}
		}
	}
	for _, pkg := range run.pkgs {
		if !selected[pkg] {
			problems = append(problems, pkg+": listed by live-process-control but has no selected tagged test")
		}
	}
	slices.Sort(problems)
	return problems, nil
}

// goTestRun is what one target's go test line runs: the build tags it passes, its -run pattern and
// the packages it lists.
type goTestRun struct {
	target  string
	tags    []string
	pattern *regexp.Regexp
	pkgs    []string
}

// builds reports whether the line's go test compiles f, which only its -tags decide.
func (run goTestRun) builds(f taggedFile) (bool, error) {
	return buildsWith(run.tags, f.dir, f.name)
}

// targetSelection reads the go test line target's recipe runs. It refuses a line it cannot model:
// one without tag compiles fewer tagged files than the scan assumes, a -skip drops tests the -run
// pattern still matches, and go splits a pattern into subtest patterns at each /.
func targetSelection(makefile, target, tag string) (goTestRun, error) {
	recipe, ok := makeRecipes(makefile)[target]
	if !ok {
		return goTestRun{}, errors.New("the Makefile has no " + target + " recipe")
	}
	run := regexp.MustCompile(`-run '([^']+)'`)
	var line string
	for _, next := range recipe {
		if !runsGoTests(next) {
			continue
		}
		// One line may run tests, the one read below; any other test run would go unread.
		if line != "" || !run.MatchString(next) {
			return goTestRun{}, errors.New(target + " runs a go test line the check cannot read: " + next)
		}
		line = next
	}
	if line == "" {
		return goTestRun{}, errors.New(target + " has no go test -run '…' line")
	}
	if strings.Contains(line, "-skip") {
		return goTestRun{}, errors.New(target + "'s go test line uses -skip: " + line)
	}
	tags := goTestTags(line)
	if !slices.Contains(tags, tag) {
		return goTestRun{}, errors.New(target + "'s go test line lacks -tags " + tag + ": " + line)
	}
	expr := strings.ReplaceAll(run.FindStringSubmatch(line)[1], "$$", "$")
	if strings.Contains(expr, "/") {
		return goTestRun{}, errors.New(target + "'s -run pattern has a /, where go splits it into subtest patterns: " + line)
	}
	pattern, err := regexp.Compile(expr)
	if err != nil {
		return goTestRun{}, err
	}
	var pkgs []string
	for _, field := range strings.Fields(line) {
		if strings.HasPrefix(field, "./") && field != "./..." {
			pkgs = append(pkgs, path.Clean(field))
		}
	}
	if len(pkgs) == 0 {
		return goTestRun{}, errors.New(target + "'s go test line names no packages: " + line)
	}
	return goTestRun{target: target, tags: tags, pattern: pattern, pkgs: pkgs}, nil
}

// makeRecipes maps each Makefile target to its recipe: the tab-indented lines after its rule. As in
// GNU Make, blank and comment lines among them do not end it; any other line does.
func makeRecipes(makefile string) map[string][]string {
	rule := regexp.MustCompile(`^([a-z][a-z0-9-]*):`)
	recipes := map[string][]string{}
	target := ""
	for _, line := range strings.Split(makefile, "\n") {
		trimmed := strings.TrimSpace(line)
		switch m := rule.FindStringSubmatch(line); {
		case m != nil:
			target = m[1]
		case strings.HasPrefix(line, "\t"):
			if target != "" {
				recipes[target] = append(recipes[target], line)
			}
		case trimmed != "" && !strings.HasPrefix(trimmed, "#"):
			target = ""
		}
	}
	return recipes
}

// runsGoTests reports whether a recipe line runs go test; a compile-only one (-c) runs nothing.
func runsGoTests(line string) bool {
	return strings.Contains(line, "go test") && !slices.Contains(strings.Fields(line), "-c")
}

// goTestTags lists the build tags a go test line passes; as with go, the last -tags wins.
func goTestTags(line string) []string {
	var tags []string
	fields := strings.Fields(line)
	for i, field := range fields {
		if field == "-tags" && i+1 < len(fields) {
			tags = strings.Split(fields[i+1], ",")
		}
	}
	return tags
}

// taggedFile is a test file that builds only with some of a guard's tags: where it is, its package
// and the tests it declares.
type taggedFile struct {
	dir, name, pkg string
	tests          []string
}

// taggedTestFiles lists the test files under root that build with some combination of tags but not
// without any, decided the way go test decides it on this platform. The files are parsed, so a
// test-shaped line inside a string, or a method named Test…, is not a test.
func taggedTestFiles(root string, tags []string) ([]taggedFile, error) {
	fset := token.NewFileSet()
	var files []taggedFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir, file := filepath.Split(path)
		if plain, err := buildsWith(nil, dir, file); err != nil || plain {
			return err // ordinary go test builds it
		}
		tagged := false
		for set := 1; set < 1<<len(tags) && !tagged; set++ { // each nonempty combination of tags
			var with []string
			for i, tag := range tags {
				if set&(1<<i) != 0 {
					with = append(with, tag)
				}
			}
			if tagged, err = buildsWith(with, dir, file); err != nil {
				return err
			}
		}
		if !tagged {
			return nil // it needs another tag, or never builds on this platform
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(rel) // clean already, like the path.Clean form of a target's list
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		tf := taggedFile{dir: dir, name: file, pkg: pkg}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") && len(fn.Type.Params.List) == 1 &&
				types.ExprString(fn.Type.Params.List[0].Type) == "*testing.T" {
				tf.tests = append(tf.tests, fn.Name.Name)
			}
		}
		files = append(files, tf)
		return nil
	})
	return files, err
}

// buildsWith reports whether go test, passed tags, compiles the file on this platform.
func buildsWith(tags []string, dir, file string) (bool, error) {
	ctx := build.Default
	ctx.BuildTags = tags
	return ctx.MatchFile(dir, file)
}

// fixtureWriter returns a function that writes a file under root, creating its folders.
func fixtureWriter(t *testing.T, root string) func(name, body string) {
	return func(name, body string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
