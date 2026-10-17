package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A test that builds only with these tags needs a container runtime (acpe2e real credentials too),
// so no ordinary `go test` runs it: only the make targets passing the tags do, each picking its
// tests with a -run pattern. None of them is in `make check`, so a test none of them runs would go
// unnoticed. Each must be run by one: a target whose -tags build its file, which lists its package
// and whose pattern matches its name; a subprocess helper takes its parent's prefix and skips when
// run alone. The Makefile and the tagged files are read with live_selection_test.go's helpers.
var runtimeSuiteTags = []string{"networkruntimee2e", "boxruntimee2e", "reviewwritee2e", "acpe2e"}

func TestRuntimeTaggedTestsRunUnderTheirTargets(t *testing.T) {
	problems, err := runtimeSelectionProblems(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) > 0 {
		t.Fatalf("a runtime-tagged test would never run — give it a prefix a target selects (a subprocess helper takes its parent's) or a target of its own, whose -tags build its file and which lists its package:\n%s", strings.Join(problems, "\n"))
	}
}

func TestRuntimeSelectionProblemsFindATestNoTargetRuns(t *testing.T) {
	root := t.TempDir()
	write := fixtureWriter(t, root)
	write("Makefile", "acp-e2e: ## a\n"+
		"\t@go test -tags acpe2e -run 'TestACP$$' -count=1 -v ./internal/acp/\n"+
		"review-writes-e2e: ## r\n"+
		"\t@go test -tags reviewwritee2e -run '^TestReviewWrites' -count=1 -v ./internal/box/\n"+
		"native-roles-e2e: ## x\n"+
		"\t@go test -tags boxruntimee2e -run '^TestRuntimeNativeRoles$$' -count=1 -v ./internal/box/\n"+
		"box-runtime-e2e: ## y\n"+
		"\t@test -n \"$$COOP_RUNTIME\" || exit 2\n"+
		"\n"+
		"# GNU Make reads a recipe on past blank and comment lines.\n"+
		"\t@go test -tags boxruntimee2e -run '^TestRuntime(Init|Compose)' -count=1 -v ./internal/box/ ./internal/stale/\n"+
		"network-runtime-e2e: ## z\n"+
		"\t@state=x; \\\n"+
		"\tCOOP_NETWORK_TRIAL_STATE=\"$$state\" go test -tags networkruntimee2e -run '^TestRestrictedNetwork' -count=1 -v ./internal/box/\n"+
		"both-e2e: ## w\n"+
		"\t@go test -tags boxruntimee2e,networkruntimee2e -run '^TestBoth' -count=1 -v ./internal/box/\n"+
		"vet:\n\t@go vet -tags boxruntimee2e,networkruntimee2e ./...\n"+
		"compile:\n\t@go test -c -tags boxruntimee2e -o x ./internal/other/\n")
	write("internal/box/runtime_e2e_test.go", "//go:build boxruntimee2e\n\npackage box\n\n"+
		"func TestRuntimeInitWorks(t *testing.T)           {}\n"+
		"func TestRuntimeNativeRoles(t *testing.T)         {}\n"+
		"func TestRuntimeNativeRolesInDepth(t *testing.T)  {}\n"+
		"func TestPlantedWithANewPrefix(t *testing.T)      {}\n"+
		"func TestMain(m *testing.M)                       {}\n"+
		"func (fixture) TestMethodIsNoTest(t *testing.T)   {}\n"+
		"const source = `\nfunc TestInsideAString(t *testing.T) {}\n`\n")
	write("internal/box/network_live_test.go", "//go:build networkruntimee2e\n\npackage box\n\n"+
		"func TestRestrictedNetworkWorks(t *testing.T) {}\nfunc TestKilledRunRecovers(t *testing.T) {}\n")
	// both-e2e's pattern matches this test, but its tags never build the file; the box targets do.
	write("internal/box/andnot_e2e_test.go", "//go:build boxruntimee2e && !networkruntimee2e\n\npackage box\n\nfunc TestBothAndNot(t *testing.T) {}\n")
	// Only both-e2e builds a file that needs both tags.
	write("internal/box/and_e2e_test.go", "//go:build boxruntimee2e && networkruntimee2e\n\npackage box\n\n"+
		"func TestBothAnd(t *testing.T) {}\nfunc TestAndNeverMatched(t *testing.T) {}\n")
	// Either tag builds it, so one target that runs it is enough.
	write("internal/box/or_e2e_test.go", "//go:build boxruntimee2e || networkruntimee2e\n\npackage box\n\nfunc TestRestrictedNetworkOr(t *testing.T) {}\n")
	write("internal/box/netreview_e2e_test.go", "//go:build networkruntimee2e && reviewwritee2e\n\npackage box\n\nfunc TestNetworkReview(t *testing.T) {}\n")
	write("internal/box/review_e2e_test.go", "//go:build reviewwritee2e\n\npackage box\n\nfunc TestReviewWritesWork(t *testing.T) {}\n")
	write("internal/box/provider_e2e_test.go", "//go:build boxruntimee2e && providere2e\n\npackage box\n\nfunc TestNeedsAnotherTag(t *testing.T) {}\n")
	write("internal/box/plain_test.go", "package box\n\nfunc TestPlain(t *testing.T) {}\n")
	write("internal/acp/acp_e2e_test.go", "//go:build acpe2e\n\npackage acp\n\nfunc TestACP(t *testing.T) {}\n")
	write("internal/other/other_e2e_test.go", "//go:build boxruntimee2e\n\npackage other\n\nfunc TestRuntimeInitElsewhere(t *testing.T) {}\n")
	write("internal/box/testdata/x_test.go", "//go:build boxruntimee2e\n\npackage x\n\nfunc TestIgnored(t *testing.T) {}\n")

	problems, err := runtimeSelectionProblems(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"internal/box: TestAndNeverMatched matches no -run pattern of both-e2e",
		"internal/box: TestBothAndNot matches no -run pattern of box-runtime-e2e, native-roles-e2e",
		"internal/box: TestKilledRunRecovers matches no -run pattern of both-e2e, network-runtime-e2e",
		"internal/box: TestNetworkReview is in netreview_e2e_test.go, which no runtime target's -tags build",
		"internal/box: TestPlantedWithANewPrefix matches no -run pattern of both-e2e, box-runtime-e2e, native-roles-e2e",
		"internal/box: TestRuntimeNativeRolesInDepth matches no -run pattern of both-e2e, box-runtime-e2e, native-roles-e2e",
		"internal/other: TestRuntimeInitElsewhere is in a package none of both-e2e, box-runtime-e2e, native-roles-e2e lists",
		"internal/stale: box-runtime-e2e lists it but selects none of its tagged tests",
	}
	if !slices.Equal(problems, want) {
		t.Fatalf("problems =\n%s\nwant\n%s", strings.Join(problems, "\n"), strings.Join(want, "\n"))
	}
}

func TestRuntimeSelectionRefusesASuiteNoTargetRuns(t *testing.T) {
	root := t.TempDir()
	fixtureWriter(t, root)("Makefile", "box-runtime-e2e:\n\t@go test -tags boxruntimee2e -run '^TestRuntime' ./internal/box/\n")
	if _, err := runtimeSelectionProblems(root); err == nil {
		t.Fatal("a Makefile without a networkruntimee2e target passed, so a mistyped or dropped suite would too")
	}
}

// runtimeSelectionProblems lists the tests that build only with the runtime suites' tags and that no
// target runs, plus the packages a target lists but selects none of the tagged tests in.
func runtimeSelectionProblems(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return nil, err
	}
	makefile := string(data)
	var runs []goTestRun
	for _, tag := range runtimeSuiteTags {
		targets := targetsRunningTag(makefile, tag)
		if len(targets) == 0 {
			return nil, errors.New("no Makefile target runs go test -tags " + tag)
		}
		for _, target := range targets {
			if slices.ContainsFunc(runs, func(run goTestRun) bool { return run.target == target }) {
				continue // it passes an earlier tag too
			}
			run, err := targetSelection(makefile, target, tag)
			if err != nil {
				return nil, err
			}
			runs = append(runs, run)
		}
	}
	slices.SortFunc(runs, func(a, b goTestRun) int { return strings.Compare(a.target, b.target) })
	files, err := taggedTestFiles(root, runtimeSuiteTags)
	if err != nil {
		return nil, err
	}
	var problems []string
	selects := map[string]bool{} // target+" "+package: the target runs one of the package's tests
	for _, f := range files {
		var built, listing []goTestRun
		for _, run := range runs {
			ok, err := run.builds(f)
			if err != nil {
				return nil, err
			}
			if ok {
				built = append(built, run)
				if slices.Contains(run.pkgs, f.pkg) {
					listing = append(listing, run)
				}
			}
		}
		for _, name := range f.tests {
			selected := false
			for _, run := range listing {
				if run.pattern.MatchString(name) {
					selects[run.target+" "+f.pkg], selected = true, true
				}
			}
			switch {
			case len(built) == 0:
				problems = append(problems, f.pkg+": "+name+" is in "+f.name+", which no runtime target's -tags build")
			case len(listing) == 0:
				problems = append(problems, f.pkg+": "+name+" is in a package none of "+targetNames(built)+" lists")
			case !selected:
				problems = append(problems, f.pkg+": "+name+" matches no -run pattern of "+targetNames(listing))
			}
		}
	}
	for _, run := range runs {
		for _, pkg := range run.pkgs {
			if !selects[run.target+" "+pkg] {
				problems = append(problems, pkg+": "+run.target+" lists it but selects none of its tagged tests")
			}
		}
	}
	slices.Sort(problems)
	return problems, nil
}

// targetsRunningTag lists, sorted, the Makefile targets whose recipe runs go test with tag.
func targetsRunningTag(makefile, tag string) []string {
	var targets []string
	for target, recipe := range makeRecipes(makefile) {
		if slices.ContainsFunc(recipe, func(line string) bool {
			return runsGoTests(line) && slices.Contains(goTestTags(line), tag)
		}) {
			targets = append(targets, target)
		}
	}
	slices.Sort(targets)
	return targets
}

// targetNames joins the runs' targets for a message.
func targetNames(runs []goTestRun) string {
	names := make([]string, len(runs))
	for i, run := range runs {
		names[i] = run.target
	}
	return strings.Join(names, ", ")
}
