package cli

// The review dossier's file classifier: parent-computed, deterministic risk ordering
// for `coop fork review`. Everything is derived from git facts (--name-status +
// --numstat output strings) so a fork can't steer its own review via its narrative;
// the only agent-authored section of the brief (the task log) is labeled as the
// agent's claim in forkBrief. Pure functions over strings — no git calls — so the
// classifier table-tests without a repo.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Dossier section titles, in the order they print: what steers agents and toolchains
// first (the top prompt-injection/review surface), then code by churn, tests, docs.
const (
	dossierConfig = "config & instructions"
	dossierCode   = "code"
	dossierTests  = "tests"
	dossierDocs   = "docs"
)

// dossierFile is one changed file under a dossier section.
type dossierFile struct {
	status     string // the --name-status letter (R100 → R)
	path       string // a rename keeps the NEW path (the last tab field, as policyScan does)
	adds, dels int    // -1 when unknown (binary blob, or a rename the numstat lookup misses)
}

// render is the printed dossier line: status, path, and +N -N when known.
func (f dossierFile) render() string {
	s := f.status + "  " + f.path
	if f.adds >= 0 {
		s += fmt.Sprintf("  +%d -%d", f.adds, f.dels)
	}
	return s
}

// dossierSection is one risk-ordered group of changed files.
type dossierSection struct {
	title string
	files []dossierFile
}

// classifyChanged buckets `git diff --name-status` lines into review-risk order —
// config & instructions → code (churn desc) → tests → docs — attaching per-file
// +N -N from `git diff --numstat`. Empty categories are omitted; empty input is nil.
func classifyChanged(nameStatus, numstat string) []dossierSection {
	nameStatus = strings.TrimSpace(nameStatus)
	if nameStatus == "" {
		return nil
	}
	// numstat lookup by path. A rename is spelled "a => b" there, so its lookup below
	// misses and the line prints without counts — no rename-tracking cleverness.
	counts := map[string][2]int{}
	for _, line := range strings.Split(strings.TrimSpace(numstat), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			continue
		}
		a, aerr := strconv.Atoi(f[0])
		d, derr := strconv.Atoi(f[1])
		if aerr != nil || derr != nil {
			continue // "-" on a binary blob
		}
		counts[f[2]] = [2]int{a, d}
	}
	buckets := map[string][]dossierFile{}
	for _, line := range strings.Split(nameStatus, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 2 || f[0] == "" {
			continue
		}
		path := f[len(f)-1]
		df := dossierFile{status: f[0][:1], path: path, adds: -1, dels: -1}
		if c, ok := counts[path]; ok {
			df.adds, df.dels = c[0], c[1]
		}
		cat := dossierCategory(path)
		buckets[cat] = append(buckets[cat], df)
	}
	// Code orders by churn (added+deleted desc); unknown counts sink. Other sections
	// keep git's path order. Stable, so ties keep the input order.
	if code := buckets[dossierCode]; len(code) > 1 {
		sort.SliceStable(code, func(i, j int) bool { return code[i].adds+code[i].dels > code[j].adds+code[j].dels })
	}
	var out []dossierSection
	for _, title := range []string{dossierConfig, dossierCode, dossierTests, dossierDocs} {
		if fs := buckets[title]; len(fs) > 0 {
			out = append(out, dossierSection{title: title, files: fs})
		}
	}
	return out
}

// dossierCategory places one changed path. Instruction and build-config surfaces are
// checked FIRST — and in particular before docs, so AGENTS.md never files under docs.
func dossierCategory(path string) string {
	p := strings.ReplaceAll(path, "\\", "/")
	base := p[strings.LastIndex(p, "/")+1:]
	switch {
	case isInstructionPath(p, base) || isBuildConfigPath(p, base):
		return dossierConfig
	case isTestPath(p, base):
		return dossierTests
	case strings.HasSuffix(base, ".md") || hasSegment(p, "docs") || hasSegment(p, "man"):
		return dossierDocs
	}
	return dossierCode
}

// hasSegment reports whether p contains seg as a whole path segment (any depth).
func hasSegment(p, seg string) bool {
	return strings.HasPrefix(p, seg+"/") || strings.Contains(p, "/"+seg+"/")
}

// isInstructionPath: files that steer agents — in coop, a fork editing its own
// instructions is the top prompt-injection review surface, so these lead the dossier.
func isInstructionPath(p, base string) bool {
	if base == "AGENTS.md" || base == "CLAUDE.md" {
		return true
	}
	for _, d := range []string{".agent", ".claude", ".codex", ".gemini"} {
		if hasSegment(p, d) {
			return true
		}
	}
	return false
}

// isBuildConfigPath: dependency manifests and lockfiles, container/build files, CI —
// changes that alter what runs on install/build rather than the code itself.
func isBuildConfigPath(p, base string) bool {
	switch base {
	case "go.mod", "go.sum", "package.json", "package-lock.json", "yarn.lock",
		"pnpm-lock.yaml", "Cargo.toml", "Cargo.lock", "Gemfile", "Gemfile.lock",
		"pyproject.toml", "poetry.lock", "Makefile", "GNUmakefile", "Jenkinsfile":
		return true
	}
	return strings.HasPrefix(base, "Dockerfile") ||
		(strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt")) ||
		strings.HasPrefix(p, ".github/workflows/") || strings.HasPrefix(base, ".gitlab-ci")
}

// isTestPath: test files and trees (*_test.*, test/, tests/, __tests__/, spec/).
func isTestPath(p, base string) bool {
	if strings.Contains(base, "_test.") {
		return true
	}
	for _, d := range []string{"test", "tests", "__tests__", "spec"} {
		if hasSegment(p, d) {
			return true
		}
	}
	return false
}
