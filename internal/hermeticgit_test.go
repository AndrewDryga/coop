package internal_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Every failure names both of these: the rule says why, the table is where a justified exception
// goes.
const (
	hermeticCard  = ".agent/kb/rules/hermetic-git-tests.md"
	hermeticTable = "the hermeticGitExceptions table in internal/hermeticgit_test.go"
)

// hermeticGitExceptions are the repo-creating functions whose config pins legitimately live
// somewhere the scan cannot see — one line each, with the reason. An entry is a claim about
// another file, so it is checked in both directions: a function that stops creating a repo, or
// grows its own pins, has to lose its entry.
var hermeticGitExceptions = map[string]string{
	"internal/cli/scripted_process_e2e_test.go:initProcessRepo": "runs git with the env its caller built from procharness.Environment, which pins GIT_CONFIG_GLOBAL and GIT_CONFIG_NOSYSTEM for every process-e2e child",
}

// TestHermeticGitTests is the check for hermeticCard: a test fixture that creates a git repository
// must close BOTH config doors, so the developer's commit.gpgsign, core.hooksPath, commit.template,
// or core.excludesFile can never change what the test observes.
//
// It reads source, not behavior: every test-serving .go file under internal/ and tools/ is parsed,
// and any function that builds a repo (a "git" command with an "init" subcommand) must also name
// gitrepo.New, GIT_CONFIG_GLOBAL + GIT_CONFIG_SYSTEM, or GIT_CONFIG_NOSYSTEM. That makes it
// tag-agnostic — the acpe2e and cooplivetest fixtures are covered like any other file — and
// comment-blind, since comments are dropped before the scan: prose about gitrepo.New is not a pin.
//
// What it cannot see, and does not pretend to: a pin (or a git binary) that lives in a different
// function — internal/testutil/liveprovider's InitRepository names neither, so the scan skips it and
// review is what holds it — and whether a pinned env actually reaches the command. Nor can it know
// that the CODE UNDER TEST shells out to git, which is why several fixtures here pin the process
// environment rather than a child's. Production code is out of scope entirely: coop's own git calls
// must honor the user's config, not hide from it.
func TestHermeticGitTests(t *testing.T) {
	fixtures := scanGitFixtures(t)

	var creators []string
	for _, fixture := range fixtures {
		if !fixture.createsRepo {
			continue
		}
		creators = append(creators, fixture.id)
		if _, excused := hermeticGitExceptions[fixture.id]; excused || fixture.hermetic() {
			continue
		}
		t.Errorf("%s creates a git repository without closing both config doors: the developer's "+
			"global and system git config still reach it, so commit.gpgsign, core.hooksPath, "+
			"commit.template, or core.excludesFile can change what the test observes. Use "+
			"internal/testutil/gitrepo.New(t), or pin GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM (or "+
			"GIT_CONFIG_NOSYSTEM=1) on the env the git commands run with — see %s. Identity "+
			"(GIT_AUTHOR_NAME, GIT_COMMITTER_EMAIL) is not isolation. If the pins really live in "+
			"another function, add the reason to %s", fixture.id, hermeticCard, hermeticTable)
	}

	// The table cannot rot into a description of code that used to exist: an entry that no longer
	// names a repo-creating function, or one that has since grown its own pins, is a stale claim.
	for _, id := range slices.Sorted(maps.Keys(hermeticGitExceptions)) {
		if !slices.Contains(creators, id) {
			t.Errorf("%s is excused in %s but no longer creates a git repository: drop the entry "+
				"(same commit as the change that removed it)", id, hermeticTable)
		}
	}
	for _, fixture := range fixtures {
		if _, excused := hermeticGitExceptions[fixture.id]; excused && fixture.createsRepo && fixture.hermetic() {
			t.Errorf("%s is excused in %s but now pins the config doors itself: drop the entry, the "+
				"scan can see it", fixture.id, hermeticTable)
		}
	}
}

// TestHermeticGitTestsScannerCatchesTheNearMisses pins the scan's verdict against synthetic
// fixtures — every shape it must accept and every one it must reject — so the check is known to be
// able to FAIL without anyone reintroducing a violation in the tree to find out.
func TestHermeticGitTestsScannerCatchesTheNearMisses(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		body                      string
		wantCreator, wantHermetic bool
	}{
		{"the helper", `repo, run := gitrepo.New(t); run("commit", "-qm", "x"); _ = repo`, false, true},
		{"both doors in the process env", `t.Setenv("GIT_CONFIG_GLOBAL", "/g"); t.Setenv("GIT_CONFIG_SYSTEM", "/s")
			cmd := exec.Command("git", "init", "-q"); _ = cmd.Run()`, true, true},
		{"both doors on the child env", `cmd := exec.Command("git", "init", "-q")
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/g", "GIT_CONFIG_SYSTEM=/s"); _ = cmd.Run()`, true, true},
		{"nosystem instead of a system file", `cmd := exec.Command("git", "init", "-q")
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/g", "GIT_CONFIG_NOSYSTEM=1"); _ = cmd.Run()`, true, true},
		{"the git binary through a variable", `cmd := exec.Command(gitBin, "init", "-q")
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/g", "GIT_CONFIG_NOSYSTEM=1"); _ = cmd.Run()`, true, true},

		{"global pinned alone — the near miss this rule exists for", `cmd := exec.Command("git", "init", "-q")
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/g"); _ = cmd.Run()`, true, false},
		{"identity is not isolation", `cmd := exec.Command("git", "init", "-q")
			cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_COMMITTER_EMAIL=t@t"); _ = cmd.Run()`, true, false},
		{"the ambient environment", `cmd := exec.Command("git", "init", "-q"); _ = cmd.Run()`, true, false},
		{"the git binary through a variable, unpinned", `cmd := exec.Command(gitBin, "init", "-q"); _ = cmd.Run()`, true, false},
		{"a comment is not a pin", `// gitrepo.New(t) pins GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM for us
			cmd := exec.Command("git", "init", "-q"); _ = cmd.Run()`, true, false},

		{"git without a repo of its own is not this rule's business", `out, _ := exec.Command("git", "log", "-1").Output(); _ = out`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixtures, err := classifyGitFixtures("fixture_test.go", "package p\n\nfunc Fixture(t *testing.T) {\n"+tc.body+"\n}\n")
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if len(fixtures) != 1 {
				t.Fatalf("want exactly 1 scanned function, got %d: %+v", len(fixtures), fixtures)
			}
			if got := fixtures[0]; got.createsRepo != tc.wantCreator || got.hermetic() != tc.wantHermetic {
				t.Errorf("createsRepo = %v, hermetic = %v; want %v and %v (%+v)",
					got.createsRepo, got.hermetic(), tc.wantCreator, tc.wantHermetic, got)
			}
		})
	}

	// A function that never touches git is not scanned at all — the check stays silent about the
	// rest of the tree.
	fixtures, err := classifyGitFixtures("fixture_test.go", "package p\n\nfunc Fixture(t *testing.T) {\n\tt.Setenv(\"HOME\", \"/tmp\")\n}\n")
	if err != nil || len(fixtures) != 0 {
		t.Errorf("a function that runs no git must not be scanned: got %+v, err %v", fixtures, err)
	}
}

// gitFixture is what one scanned function said about itself.
type gitFixture struct {
	id          string // "internal/box/image_test.go:TestStageBuildContext" — file, then function
	createsRepo bool   // runs git AND names the "init" subcommand
	pinsGlobal  bool
	pinsSystem  bool
	usesHelper  bool // calls gitrepo.New, which pins both doors on its own runner
}

func (f gitFixture) hermetic() bool { return f.usesHelper || (f.pinsGlobal && f.pinsSystem) }

// scanGitFixtures classifies every function in every test-serving file under internal/ and tools/,
// in a stable order. Test-serving means a _test.go file or one under testutil/ or testdata/ — the
// code that builds fixtures, whichever build tag it hides behind.
func scanGitFixtures(t *testing.T) []gitFixture {
	t.Helper()
	var fixtures []gitFixture
	// The test runs in internal/, so each root is walked from there and reported under the name a
	// reader would grep for from the repo root.
	for _, root := range []struct{ dir, reportedAs string }{{".", "internal"}, {"../tools", "tools"}} {
		err := filepath.WalkDir(root.dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
				return err
			}
			within, err := filepath.Rel(root.dir, p)
			if err != nil {
				t.Fatalf("relative path for %s: %v", p, err)
			}
			rel := path.Join(root.reportedAs, filepath.ToSlash(within))
			if !strings.HasSuffix(rel, "_test.go") && !strings.Contains(rel, "/testutil/") && !strings.Contains(rel, "/testdata/") {
				return nil
			}
			source, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			found, err := classifyGitFixtures(rel, source)
			if err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			fixtures = append(fixtures, found...)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root.dir, err)
		}
	}
	slices.SortFunc(fixtures, func(a, b gitFixture) int { return strings.Compare(a.id, b.id) })
	return fixtures
}

// classifyGitFixtures parses one file's source and reports one gitFixture per top-level declaration
// that runs git. Parsing with mode 0 drops the comments, so only code can count as a pin.
func classifyGitFixtures(name string, src any) ([]gitFixture, error) {
	file, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		return nil, err
	}
	var fixtures []gitFixture
	for _, decl := range file.Decls {
		fixture := gitFixture{id: name + ":" + declName(decl)}
		runsGit, namesInit := false, false
		ast.Inspect(decl, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.BasicLit:
				if node.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(node.Value)
				if err != nil {
					return true
				}
				switch {
				case value == "git" || strings.HasSuffix(value, "/git"):
					runsGit = true // the git binary, as an argv[0] or a PATH lookup
				case value == "init":
					namesInit = true
				case strings.Contains(value, "GIT_CONFIG_GLOBAL"):
					fixture.pinsGlobal = true
				case strings.Contains(value, "GIT_CONFIG_SYSTEM"), strings.Contains(value, "GIT_CONFIG_NOSYSTEM"):
					fixture.pinsSystem = true
				}
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, isIdent := sel.X.(*ast.Ident)
				switch {
				case isIdent && pkg.Name == "gitrepo" && sel.Sel.Name == "New":
					fixture.usesHelper = true
				case isIdent && pkg.Name == "exec" && strings.HasPrefix(sel.Sel.Name, "Command"):
					runsGit = runsGit || slices.ContainsFunc(node.Args, isGitBinary) // exec.Command(gitBin, …)
				}
			}
			return true
		})
		fixture.createsRepo = runsGit && namesInit
		if runsGit || fixture.usesHelper {
			fixtures = append(fixtures, fixture)
		}
	}
	return fixtures, nil
}

// isGitBinary reports whether an argument names the git binary through a variable — the shape
// process-level fixtures use so they can point at a stubbed git (exec.Command(gitBin, …)).
func isGitBinary(arg ast.Expr) bool {
	ident, ok := arg.(*ast.Ident)
	return ok && strings.Contains(strings.ToLower(ident.Name), "git")
}

// declName is the function a failure points at, so a violation reads as a place a reader can open,
// not a file to search.
func declName(decl ast.Decl) string {
	fn, ok := decl.(*ast.FuncDecl)
	if !ok {
		return "(package-level declaration)"
	}
	return fn.Name.Name
}
