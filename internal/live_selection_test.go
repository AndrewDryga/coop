package internal_test

import (
	"bufio"
	"errors"
	"go/build"
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
// silently, so every tagged test must be one or the other.
var liveOnlyName = regexp.MustCompile(`(LiveCompatibility|LiveChild)$`)

func TestTaggedTestsRunUnderLiveProcessControl(t *testing.T) {
	problems, err := liveSelectionProblems("..")
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) > 0 {
		t.Fatalf("tagged tests run under no make target — give a deterministic one a live-process-control prefix in a listed package, or name one that needs a real provider …LiveCompatibility:\n%s", strings.Join(problems, "\n"))
	}
}

func TestLiveSelectionProblemsFindWhatTheTargetMisses(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", "live-process-control: ## x\n"+
		"\t@go test -race -tags providerlivee2e -run '^Test(LiveA|LiveB)' -count=1 ./internal/a/ ./internal/b/\n"+
		"provider-live-e2e: ## y\n\t@go test -run \"^TestLiveC\" ./internal/c/\n")
	write("internal/a/a_live_test.go", "// A header comment above the build line.\n\n//go:build providerlivee2e && !windows\n\npackage a\n\n"+
		"func TestLiveAWorks(t *testing.T)                  {}\n"+
		"func TestProviderLiveCompatibility(t *testing.T)   {}\n"+
		"func TestProviderLiveChild(t *testing.T)           {}\n"+
		"func TestMain(m *testing.M)                        {}\n"+
		"func TestRenamedDeterministic(t *testing.T)        {}\n")
	write("internal/a/a_default_test.go", "//go:build !providerlivee2e\n\npackage a\n\nfunc TestUntagged(t *testing.T) {}\n")
	write("internal/b/b_test.go", "package b\n\nfunc TestLiveBPlain(t *testing.T) {}\n")
	write("internal/c/c_live_test.go", "//go:build providerlivee2e\n\npackage c\n\nfunc TestLiveAUnlisted(t *testing.T) {}\n")
	write("internal/a/testdata/x_live_test.go", "//go:build providerlivee2e\n\npackage x\n\nfunc TestIgnored(t *testing.T) {}\n")
	write(".hidden/x_live_test.go", "//go:build providerlivee2e\n\npackage x\n\nfunc TestIgnored(t *testing.T) {}\n")

	problems, err := liveSelectionProblems(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"internal/a: TestRenamedDeterministic matches no live-process-control prefix",
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
	} {
		if _, _, err := liveProcessControlSelection(makefile); err == nil {
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
	pattern, pkgs, err := liveProcessControlSelection(string(data))
	if err != nil {
		return nil, err
	}
	tests, err := liveTaggedTests(root)
	if err != nil {
		return nil, err
	}
	listed := map[string]bool{}
	for _, pkg := range pkgs {
		listed[pkg] = true
	}
	selected := map[string]bool{}
	var problems []string
	for pkg, names := range tests {
		for _, name := range names {
			switch {
			case liveOnlyName.MatchString(name):
			case !pattern.MatchString(name):
				problems = append(problems, pkg+": "+name+" matches no live-process-control prefix")
			case !listed[pkg]:
				problems = append(problems, pkg+": "+name+" is in a package live-process-control does not list")
			default:
				selected[pkg] = true
			}
		}
	}
	for _, pkg := range pkgs {
		if !selected[pkg] {
			problems = append(problems, pkg+": listed by live-process-control but has no selected tagged test")
		}
	}
	slices.Sort(problems)
	return problems, nil
}

// liveProcessControlSelection reads the -run pattern and the packages of the target's go test line.
// It refuses a line it cannot model: one without the live tag compiles fewer tagged files than the
// scan assumes, and a -skip drops tests the -run pattern still matches.
func liveProcessControlSelection(makefile string) (*regexp.Regexp, []string, error) {
	_, recipe, ok := strings.Cut("\n"+makefile, "\nlive-process-control:")
	if !ok {
		return nil, nil, errors.New("the Makefile has no live-process-control target")
	}
	run := regexp.MustCompile(`-run '([^']+)'`)
	for _, line := range strings.Split(recipe, "\n")[1:] {
		if !strings.HasPrefix(line, "\t") {
			break // the recipe ends at its first line without a leading tab
		}
		m := run.FindStringSubmatch(line)
		if m == nil || !strings.Contains(line, "go test") {
			continue
		}
		if strings.Contains(line, "-skip") {
			return nil, nil, errors.New("live-process-control's go test line uses -skip: " + line)
		}
		pattern, err := regexp.Compile(strings.ReplaceAll(m[1], "$$", "$"))
		if err != nil {
			return nil, nil, err
		}
		var pkgs []string
		tagged := map[string]bool{}
		fields := strings.Fields(line)
		for i, field := range fields {
			if field == "-tags" && i+1 < len(fields) {
				for _, tag := range strings.Split(fields[i+1], ",") {
					tagged[tag] = true
				}
			}
			if strings.HasPrefix(field, "./") && field != "./..." {
				pkgs = append(pkgs, path.Clean(field))
			}
		}
		if !tagged["providerlivee2e"] {
			return nil, nil, errors.New("live-process-control's go test line lacks -tags providerlivee2e: " + line)
		}
		if len(pkgs) == 0 {
			return nil, nil, errors.New("live-process-control's go test line names no packages: " + line)
		}
		return pattern, pkgs, nil
	}
	return nil, nil, errors.New("live-process-control has no go test -run '…' line")
}

// liveTaggedTests maps each package under root to the tests of its files that build only with the
// live tags, decided the way `go test` decides it on this platform.
func liveTaggedTests(root string) (map[string][]string, error) {
	live := build.Default
	live.BuildTags = []string{"providerlivee2e"}
	testFunc := regexp.MustCompile(`^func (Test\w*)\(\w+ \*testing\.T\)`)
	tests := map[string][]string{}
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
		withTags, err := live.MatchFile(dir, file)
		if err != nil {
			return err
		}
		withoutTags, err := build.Default.MatchFile(dir, file)
		if err != nil {
			return err
		}
		if !withTags || withoutTags {
			return nil // it builds without the live tags, or never on this platform
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(rel) // clean already, like the path.Clean form of the target's list
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			if m := testFunc.FindStringSubmatch(scanner.Text()); m != nil {
				tests[pkg] = append(tests[pkg], m[1])
			}
		}
		return scanner.Err()
	})
	return tests, err
}
