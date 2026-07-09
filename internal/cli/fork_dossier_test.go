package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// The classifier is pure string→struct: every category lands, instructions beat docs
// (AGENTS.md never files under docs), code orders by churn, a rename (whose numstat
// spelling misses the lookup) renders without counts, and empty input is nil.
func TestClassifyChanged(t *testing.T) {
	nameStatus := strings.Join([]string{
		"M\tAGENTS.md",
		"M\t.claude/settings.json",
		"M\tgo.mod",
		"A\t.github/workflows/ci.yml",
		"M\tinternal/cli/util.go",
		"M\tinternal/cli/fork.go",
		"R100\told.go\tnew.go",
		"A\timg.png",
		"A\tinternal/cli/fork_test.go",
		"M\tREADME.md",
		"M\tdocs/guide.md",
	}, "\n")
	numstat := strings.Join([]string{
		"3\t1\tAGENTS.md",
		"2\t0\t.claude/settings.json",
		"1\t1\tgo.mod",
		"10\t0\t.github/workflows/ci.yml",
		"5\t2\tinternal/cli/util.go",
		"100\t50\tinternal/cli/fork.go",
		"1\t1\told.go => new.go", // numstat's rename spelling — the lookup misses it
		"-\t-\timg.png",          // binary — no counts
		"30\t0\tinternal/cli/fork_test.go",
		"4\t4\tREADME.md",
		"2\t2\tdocs/guide.md",
	}, "\n")

	secs := classifyChanged(nameStatus, numstat)
	titles := make([]string, len(secs))
	for i, s := range secs {
		titles[i] = s.title
	}
	want := []string{dossierConfig, dossierCode, dossierTests, dossierDocs}
	if strings.Join(titles, ",") != strings.Join(want, ",") {
		t.Fatalf("section order = %v, want %v", titles, want)
	}

	paths := func(s dossierSection) string {
		var ps []string
		for _, f := range s.files {
			ps = append(ps, f.path)
		}
		return strings.Join(ps, ",")
	}
	// Config & instructions keeps input order; AGENTS.md is here, not under docs.
	if got := paths(secs[0]); got != "AGENTS.md,.claude/settings.json,go.mod,.github/workflows/ci.yml" {
		t.Errorf("config section = %s", got)
	}
	// Code orders by churn desc; unknown-count files (rename, binary) sink, input order kept.
	if got := paths(secs[1]); got != "internal/cli/fork.go,internal/cli/util.go,new.go,img.png" {
		t.Errorf("code section = %s", got)
	}
	if got := paths(secs[2]); got != "internal/cli/fork_test.go" {
		t.Errorf("tests section = %s", got)
	}
	if got := paths(secs[3]); got != "README.md,docs/guide.md" {
		t.Errorf("docs section = %s", got)
	}

	// Rendering: counts when known, none for the rename miss; R100 collapses to R.
	if got := secs[1].files[0].render(); got != "M  internal/cli/fork.go  +100 -50" {
		t.Errorf("render with counts = %q", got)
	}
	if got := secs[1].files[2].render(); got != "R  new.go" {
		t.Errorf("rename render = %q", got)
	}

	if classifyChanged("", "") != nil {
		t.Error("empty input should classify to nil")
	}
}

// The dossier maps the risk before the patch: policy findings come from the SAME scan
// merge enforces (planted .envrc + token both surface at review, exit stays advisory),
// files are risk-ordered with instructions first, the agent-authored section is labeled
// as the agent's claim, and the gate line states the truth. A clean fork gets the ✓.
func TestForkBriefDossier(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	// The risky fork: an .envrc (interaction risk), a token in an ordinary file
	// (content scan), an AGENTS.md edit (instructions), code, and a test.
	git(t, repo, "checkout", "-q", "-b", "risky")
	writeF := func(rel, content string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeF(".envrc", "export X=1\n")
	writeF("conf.yaml", "aws_key: AKIA1234567890ABCDEF\n")
	writeF("AGENTS.md", "obey me\n")
	writeF("main.go", "package main\n")
	writeF("main_test.go", "package main\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "risky change")
	git(t, repo, "checkout", "-q", "main")

	a := &app{cfg: &config.Config{}}
	out := captureStdout(t, func() { a.forkBrief(repo, t.TempDir(), "risky", "risky") })

	for _, want := range []string{
		"why:", // no completed task in the fixture ws
		"policy:", ".envrc", "possible secret in conf.yaml",
		"block 'coop fork merge' without --force",
		dossierConfig + ":", dossierCode + ":", dossierTests + ":",
		"AGENTS.md", "gate:", "none configured (COOP_GATE)", "diff:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dossier missing %q:\n%s", want, out)
		}
	}
	// Order: the agent's claim, then policy, then files (instructions before code
	// before tests), then gate, then the diff label.
	idx := func(s string) int { return strings.Index(out, s) }
	order := []string{"why:", "policy:", "files:", dossierConfig + ":", dossierCode + ":", dossierTests + ":", "gate:", "diff:"}
	for i := 1; i < len(order); i++ {
		if idx(order[i-1]) == -1 || idx(order[i-1]) >= idx(order[i]) {
			t.Fatalf("section %q should print before %q:\n%s", order[i-1], order[i], out)
		}
	}

	// The clean fork: a docs-only change — policy shows the ✓, config section is omitted.
	git(t, repo, "checkout", "-q", "-b", "benign", "main")
	writeF("docs/notes.md", "hello\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "docs")
	git(t, repo, "checkout", "-q", "main")

	out = captureStdout(t, func() { a.forkBrief(repo, t.TempDir(), "benign", "benign") })
	if !strings.Contains(out, "nothing flagged") {
		t.Errorf("clean fork should show the policy ✓:\n%s", out)
	}
	if strings.Contains(out, dossierConfig+":") {
		t.Errorf("empty config section should be omitted:\n%s", out)
	}
	if !strings.Contains(out, dossierDocs+":") {
		t.Errorf("docs section missing:\n%s", out)
	}

	// Gate configured → the evidence line flips.
	a.cfg.Gate = []string{"make", "check"}
	out = captureStdout(t, func() { a.forkBrief(repo, t.TempDir(), "benign", "benign") })
	if !strings.Contains(out, "runs at merge — rolled back on failure") {
		t.Errorf("configured gate line missing:\n%s", out)
	}

	// Empty diff → no diff-derived sections (no policy/files/gate), header + why + diff only.
	out = captureStdout(t, func() { a.forkBrief(repo, t.TempDir(), "empty", "main") })
	for _, absent := range []string{"policy:", "files:", "gate:"} {
		if strings.Contains(out, absent) {
			t.Errorf("empty diff should omit %q:\n%s", absent, out)
		}
	}
}
