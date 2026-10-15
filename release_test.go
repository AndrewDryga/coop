package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"gopkg.in/yaml.v3"
)

type releaseWorkflow struct {
	On          map[string]any    `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		Uses     string `yaml:"uses"`
		Needs    any    `yaml:"needs"`
		Name     string `yaml:"name"`
		If       string `yaml:"if"`
		Timeout  int    `yaml:"timeout-minutes"`
		Strategy struct {
			FailFast    *bool               `yaml:"fail-fast"`
			MaxParallel int                 `yaml:"max-parallel"`
			Matrix      map[string][]string `yaml:"matrix"`
		} `yaml:"strategy"`
		ContinueOnError any               `yaml:"continue-on-error"`
		Secrets         any               `yaml:"secrets"`
		Permissions     map[string]string `yaml:"permissions"`
		Steps           []releaseStep     `yaml:"steps"`
	} `yaml:"jobs"`
}

type releaseStep struct {
	Name            string            `yaml:"name"`
	Uses            string            `yaml:"uses"`
	Run             string            `yaml:"run"`
	If              string            `yaml:"if"`
	ContinueOnError any               `yaml:"continue-on-error"`
	With            map[string]string `yaml:"with"`
	Env             map[string]string `yaml:"env"`
}

func runReleasePreflight(t *testing.T, repo, command, tag string, env ...string) (string, string, error) {
	t.Helper()
	script, err := filepath.Abs("tools/release_preflight.py")
	if err != nil {
		t.Fatal(err)
	}
	// Go's test cache cannot observe files read inside the Python child.
	if _, err := os.ReadFile(script); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", script, command, tag)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_CONFIG_COUNT=0", "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}

func TestReleaseNotesPreflight(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "fixture")
	for _, tag := range []string{"v1.2.3", "v1.2.3-rc.1+build.007", "v0.0.0", "v1.2.3-0"} {
		git("tag", tag)
	}
	for _, tc := range []struct {
		name, tag, changelog, want string
	}{
		{name: "normal", changelog: "# Changelog\n\n## 1.2.3\n\n- Fixed it.\n\n## 1.2.2\n- Old.\n", want: "- Fixed it.\n"},
		{name: "prerelease build", tag: "v1.2.3-rc.1+build.007", changelog: "## 1.2.3-rc.1+build.007\nPreview.\n", want: "Preview.\n"},
		{name: "zero", tag: "v0.0.0", changelog: "## 0.0.0\nFirst.\n", want: "First.\n"},
		{name: "numeric prerelease zero", tag: "v1.2.3-0", changelog: "## 1.2.3-0\nFirst.\n", want: "First.\n"},
		{name: "CRLF Unicode", changelog: "## 1.2.3\r\n\r\n- 修正: café.\r\n", want: "- 修正: café.\n"},
		{name: "indented ATX", changelog: "   ## 1.2.3 ###\n### Fixed\n- Result.\n", want: "### Fixed\n- Result.\n"},
		{name: "comments", changelog: "<!--\n## Unreleased\n-->\n## 1.2.3\n<!-- private\n## hidden\n-->\n- A <!-- hidden --> B.\n", want: "- A  B.\n"},
		{name: "inline code", changelog: "## 1.2.3\nUse `<!-- literal -->`, `` `<!-- example -->` ``.\n", want: "Use `<!-- literal -->`, `` `<!-- example -->` ``.\n"},
		{name: "multiline inline code", changelog: "## 1.2.3\nUse `<!--\nliteral -->`.\n", want: "Use `<!--\nliteral -->`.\n"},
		{name: "multiline long inline code", changelog: "## 1.2.3\nUse `` `<!--\nliteral -->` `` <!-- remove -->.\n", want: "Use `` `<!--\nliteral -->` `` .\n"},
		{name: "unmatched span before H2", changelog: "## 1.2.3\nLiteral `.\n## 1.2.2\nOlder `.\n", want: "Literal `.\n"},
		{name: "span cannot cross H3", changelog: "## 1.2.3\nLiteral ` <!-- remove -->.\n### Example\nOther `.\n", want: "Literal ` .\n### Example\nOther `.\n"},
		{name: "span cannot cross blank", changelog: "## 1.2.3\nLiteral ` <!-- remove -->.\n\nOther `.\n", want: "Literal ` .\n\nOther `.\n"},
		{name: "span cannot cross fence", changelog: "## 1.2.3\nLiteral ` <!-- remove -->.\n~~~\nOther `.\n~~~\n", want: "Literal ` .\n~~~\nOther `.\n~~~\n"},
		{name: "escaped backticks", changelog: "## 1.2.3\nUse \\` <!-- hidden --> \\`.\n", want: "Use \\`  \\`.\n"},
		{name: "list and thematic break", changelog: "## 1.2.3\n- Fixed it.\n---\n", want: "- Fixed it.\n---\n"},
		{name: "Unicode within line", changelog: "## 1.2.3\n- A\u2028B\u0085C\vD.\n", want: "- A\u2028B\u0085C\vD.\n"},
		{name: "fenced heading", changelog: "## 1.2.3\n```md\n## example\n<!-- literal -->\n```\n## old\nIgnored.\n", want: "```md\n## example\n<!-- literal -->\n```\n"},
		{name: "fenced HTML", changelog: "## 1.2.3\n```html\n<pre>\n## example\n</pre>\n```\n", want: "```html\n<pre>\n## example\n</pre>\n```\n"},
		{name: "inline HTML", changelog: "## 1.2.3\nUse <code>example</code>.\n", want: "Use <code>example</code>.\n"},
		{name: "autolink", changelog: "## 1.2.3\n<https://example.invalid/>\n", want: "<https://example.invalid/>\n"},
		{name: "indented fenced notes", changelog: "## 1.2.3\n \n   ```md\n   literal\n   ```\n \n", want: "   ```md\n   literal\n   ```\n"},
		{name: "long fence", changelog: "## 1.2.3\n````\n```\n## example\n`````\n", want: "````\n```\n## example\n`````\n"},
		{name: "tilde fence", changelog: "## 1.2.3\n~~~text\n```\n## example\n~~~~\n", want: "~~~text\n```\n## example\n~~~~\n"},
		{name: "unreleased", changelog: "## Unreleased\nChanges.\n## 1.2.3\nOlder.\n"},
		{name: "wrong version", changelog: "## 1.2.4\nChanges.\n## 1.2.3\nOlder.\n"},
		{name: "missing heading", changelog: "# Changelog\nChanges.\n"},
		{name: "comment-made heading", changelog: "<!-- hidden -->## 1.2.3\nChanges.\n"},
		{name: "comment-closure heading", changelog: "<!-- hidden\n-->## 1.2.3\nChanges.\n"},
		{name: "HTML pre heading", changelog: "<pre>\n## 1.2.3\nExample only.\n</pre>\n"},
		{name: "HTML script heading", changelog: "<script>\n## 1.2.3\nExample only.\n</script>\n"},
		{name: "HTML div heading", changelog: "<div>\n## 1.2.3\nExample only.\n</div>\n"},
		{name: "HTML custom heading", changelog: "<release-notes>\n## 1.2.3\nExample only.\n</release-notes>\n"},
		{name: "HTML CDATA heading", changelog: "<![CDATA[\n## 1.2.3\nExample only.\n]]>\n"},
		{name: "HTML processing instruction heading", changelog: "<?--\n## 1.2.3\nExample only.\n?>\n"},
		{name: "Unicode-made heading", changelog: "prefix\u2028## 1.2.3\nChanges.\n"},
		{name: "NEL-made heading", changelog: "prefix\u0085## 1.2.3\nChanges.\n"},
		{name: "VT-made heading", changelog: "prefix\v## 1.2.3\nChanges.\n"},
		{name: "prefixed heading", changelog: "## v1.2.3\nChanges.\n"},
		{name: "heading date", changelog: "## 1.2.3 - 2026-09-05\nChanges.\n"},
		{name: "empty", changelog: "## 1.2.3\n\n## 1.2.2\nOlder.\n"},
		{name: "Unicode whitespace only", changelog: "## 1.2.3\n\u00a0\u2003\n"},
		{name: "comment only", changelog: "## 1.2.3\n<!-- invisible\nbut nonempty\n-->\n"},
		{name: "comment-only bullet", changelog: "## 1.2.3\n- <!-- TODO -->\n"},
		{name: "empty numbered item", changelog: "## 1.2.3\n1. <!-- TODO -->\n"},
		{name: "empty quote and task item", changelog: "## 1.2.3\n> - [ ] <!-- TODO -->\n"},
		{name: "quoted heading only", changelog: "## 1.2.3\n> ### Changes\n"},
		{name: "quoted separator only", changelog: "## 1.2.3\n> ---\n"},
		{name: "nested quoted separator only", changelog: "## 1.2.3\n> > ___\n"},
		{name: "link definition only", changelog: "## 1.2.3\n[reference]: https://example.invalid/\n"},
		{name: "headings only", changelog: "## 1.2.3\n### Changes\n"},
		{name: "empty code only", changelog: "## 1.2.3\n```go\n\n```\n"},
		{name: "separators only", changelog: "## 1.2.3\n\n---\n\n***\n"},
		{name: "Setext first", changelog: "Unreleased\n----------\nChanges.\n## 1.2.3\nOlder.\n"},
		{name: "Setext selected", changelog: "## 1.2.3\nChanges.\n\n1.2.2\n-----\nOlder.\n"},
		{name: "unclosed comment", changelog: "## 1.2.3\nChanges.\n<!--\n## 1.2.2\nOlder.\n"},
		{name: "unclosed fence", changelog: "## 1.2.3\nChanges.\n```\n## 1.2.2\nOlder.\n"},
		{name: "wrong closer", changelog: "## 1.2.3\n~~~\nChanges.\n```\n"},
		{name: "short closer", changelog: "## 1.2.3\n````\nChanges.\n```\n"},
		{name: "no v", tag: "1.2.3", changelog: "## 1.2.3\nChanges.\n"},
		{name: "missing patch", tag: "v1.2", changelog: "## 1.2\nChanges.\n"},
		{name: "leading major zero", tag: "v01.2.3", changelog: "## 01.2.3\nChanges.\n"},
		{name: "leading minor zero", tag: "v1.02.3", changelog: "## 1.02.3\nChanges.\n"},
		{name: "leading patch zero", tag: "v1.2.03", changelog: "## 1.2.03\nChanges.\n"},
		{name: "leading prerelease zero", tag: "v1.2.3-01", changelog: "## 1.2.3-01\nChanges.\n"},
		{name: "empty prerelease", tag: "v1.2.3-", changelog: "## 1.2.3-\nChanges.\n"},
		{name: "empty build", tag: "v1.2.3+", changelog: "## 1.2.3+\nChanges.\n"},
		{name: "empty identifier", tag: "v1.2.3-rc..1", changelog: "## 1.2.3-rc..1\nChanges.\n"},
		{name: "shell metacharacters", tag: "v1.2.3;touch injected", changelog: "## 1.2.3;touch injected\nChanges.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tag := tc.tag
			if tag == "" {
				tag = "v1.2.3"
			}
			if err := os.WriteFile(filepath.Join(repo, "CHANGELOG.md"), []byte(tc.changelog), 0o600); err != nil {
				t.Fatal(err)
			}
			out, diagnostic, err := runReleasePreflight(t, repo, "notes", tag)
			if tc.want != "" {
				if err != nil || diagnostic != "" || out != tc.want {
					t.Fatalf("out=%q stderr=%q err=%v; want %q", out, diagnostic, err, tc.want)
				}
			} else if err == nil || out != "" || !strings.Contains(diagnostic, "refusing to publish") {
				t.Fatalf("must refuse without partial notes: out=%q stderr=%q err=%v", out, diagnostic, err)
			}
		})
	}
}

func TestReleaseRemoteLookupFailsClosed(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "fixture")
	git("tag", "v1.2.3")
	oid, err := os.ReadFile(filepath.Join(repo, ".git", "refs", "tags", "v1.2.3"))
	if err != nil {
		t.Fatal(err)
	}
	refLine := strings.TrimSpace(string(oid)) + "\trefs/tags/v1.2.3"
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	shim := `#!/bin/sh
if [ "$1" = ls-remote ]; then
  printf '%s\n' "$COOP_TEST_REMOTE_OUTPUT"
  exit "$COOP_TEST_REMOTE_STATUS"
fi
exec "$COOP_TEST_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, output, status string
		ok                   bool
	}{
		{"valid control", refLine, "0", true},
		{"empty success", "", "0", false},
		{"transport failure with plausible output", refLine, "1", false},
		{"duplicate ref", refLine + "\n" + refLine, "0", false},
		{"unexpected ref", refLine + "\n" + strings.ReplaceAll(refLine, "v1.2.3", "v1.2.4"), "0", false},
		{"invalid object", "not-an-oid\trefs/tags/v1.2.3", "0", false},
		{"malformed delimiter", strings.ReplaceAll(refLine, "\t", " "), "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, diagnostic, err := runReleasePreflight(t, repo, "verify-remote", "v1.2.3",
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"COOP_TEST_REAL_GIT="+realGit, "COOP_TEST_REMOTE_OUTPUT="+tc.output,
				"COOP_TEST_REMOTE_STATUS="+tc.status)
			if out != "" || (tc.ok && (err != nil || diagnostic != "")) ||
				(!tc.ok && (err == nil || !strings.Contains(diagnostic, "refusing to publish"))) {
				t.Fatalf("ok=%v out=%q stderr=%q err=%v", tc.ok, out, diagnostic, err)
			}
		})
	}
}

func TestReleaseTagIdentity(t *testing.T) {
	for _, annotated := range []bool{false, true} {
		name := "lightweight"
		if annotated {
			name = "annotated"
		}
		t.Run(name, func(t *testing.T) {
			repo, git := gitrepo.New(t)
			remote, remoteGit := gitrepo.New(t)
			git("commit", "--allow-empty", "-qm", "fixture")
			if annotated {
				git("tag", "-a", "v1.2.3", "-m", "release")
			} else {
				git("tag", "v1.2.3")
			}
			git("remote", "add", "origin", remote)
			git("push", "origin", "refs/tags/v1.2.3") // only a hermetic filesystem remote
			if err := os.WriteFile(filepath.Join(repo, "CHANGELOG.md"), []byte("## 1.2.3\nChanges.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			check := func(command string, success bool) {
				t.Helper()
				out, diagnostic, err := runReleasePreflight(t, repo, command, "v1.2.3")
				if success {
					if err != nil || diagnostic != "" || (command == "verify-remote" && out != "") {
						t.Fatalf("%s: out=%q stderr=%q err=%v", command, out, diagnostic, err)
					}
				} else if err == nil || out != "" || !strings.Contains(diagnostic, "refusing to publish") {
					t.Fatalf("%s must refuse with no stdout: out=%q stderr=%q err=%v", command, out, diagnostic, err)
				}
			}
			check("notes", true)
			check("verify-remote", true)
			if annotated {
				remoteGit("tag", "-fa", "v1.2.3", "v1.2.3^{commit}", "-m", "replacement annotation")
				check("verify-remote", false)
			}
			remoteGit("tag", "-d", "v1.2.3")
			check("verify-remote", false)
			remoteGit("commit", "--allow-empty", "-qm", "different commit")
			remoteGit("tag", "v1.2.3")
			check("verify-remote", false)
			git("remote", "set-url", "origin", filepath.Join(t.TempDir(), "absent"))
			check("verify-remote", false)
			git("commit", "--allow-empty", "-qm", "HEAD changed")
			check("notes", false)
			check("verify-remote", false)
			git("tag", "-d", "v1.2.3")
			check("notes", false)
		})
	}
}

func TestReleaseModuleHygiene(t *testing.T) {
	data, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "\nCI_OWNERS := check-static ") || !strings.Contains(text, "\ncheck: check-static ") ||
		!strings.Contains(text, "\ncheck-static: mod-check gateway-sources-check ") {
		t.Error("canonical union must begin with static module/freshness checks before behavioral owners")
	}
	if !strings.Contains(text, "\nmod-check: ## Verify module metadata is tidy without changing qualified sources\n\t@go mod tidy -diff\n") {
		t.Error("module check must refuse drift without rewriting qualified sources")
	}
	if !strings.Contains(text, "\n.PHONY: mod-check\n") {
		t.Error("a same-named file must not skip module hygiene")
	}
	if !strings.Contains(text, "\t@go test ./internal/gatewayimage -run '^TestEmbeddedGatewaySourcesExactlyMatchCheckout$$' -count=1\n") ||
		!strings.Contains(text, "\n.PHONY: gateway-sources-check\n") {
		t.Error("canonical gate must verify embedded source freshness without regenerating during qualification")
	}
}

const (
	ciCacheRestore = "restore this owner's Go modules and compiled objects"
	ciCacheSave    = "save this owner's Go modules and compiled objects"
	// Only reviewed Go inputs and the owner name the cache; the commit makes each main push's
	// objects the newest, and the prefix lets every later run start from them.
	ciCacheKey    = "go-${{ runner.os }}-${{ matrix.target }}-${{ hashFiles('go.mod', 'go.sum', 'tools/staticcheck.mod', 'tools/staticcheck.sum') }}-"
	ciCachePaths  = "~/go/pkg/mod\n~/.cache/go-build\n"
	ciCachePinned = "@55cc8345863c7cc4c66a329aec7e433d2d1c52a9"
)

// makeVariable reads one `NAME := value` line of the Makefile, such as CI_OWNERS, the owners CI
// runs one per runner.
func makeVariable(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, name+" := "); ok {
			return value
		}
	}
	t.Fatalf("the Makefile must set %s", name)
	return ""
}

// TestReleaseLocalGateRunsEveryCIOwner holds what lets `make check` leave check-unit to CI: the
// gate runs every other CI owner, and the race owners run check-unit's tests only while no Go file
// builds differently under the race detector.
func TestReleaseLocalGateRunsEveryCIOwner(t *testing.T) {
	raceConstraint := regexp.MustCompile(`(?m)^(//go:build|// \+build) .*\brace\b`)
	ci := strings.Fields(makeVariable(t, "CI_OWNERS"))
	local := append([]string{"check-static", "check-unit"}, strings.Fields(makeVariable(t, "GATE_TEST_OWNERS"))...)
	slices.Sort(ci)
	slices.Sort(local)
	if !slices.Equal(ci, local) {
		t.Errorf("CI_OWNERS %v must be check-static, check-unit and GATE_TEST_OWNERS %v", ci, local)
	}
	// The variables only matter if the check recipe runs them, rather than a list kept by hand.
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"\ncheck: check-static ", "$(addprefix gate-,$(GATE_TEST_OWNERS))"} {
		if !strings.Contains(string(makefile), step) {
			t.Errorf("make check must run check-static, then every GATE_TEST_OWNERS owner: %q is gone", step)
		}
	}
	out, err := exec.Command("git", "ls-files", "-z", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if !slices.Contains(files, "release_test.go") {
		t.Fatalf("git ls-files must list the tracked Go files, got %d entries", len(files))
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted and not yet staged: it builds nothing
		} else if err != nil {
			t.Fatal(err)
		}
		if raceConstraint.Match(data) {
			t.Errorf("%s has a race build constraint, so make check would skip tests only check-unit runs", file)
		}
	}
}

// TestReleaseShardPatternsSplitTheirSuites keeps each shard pair worth its runner. One pattern,
// -run in one shard and -skip in the other, keeps every test in exactly one; a pattern that matched
// all of its suite or none of it would leave one shard empty and the other the whole suite.
func TestReleaseShardPatternsSplitTheirSuites(t *testing.T) {
	testFunc := regexp.MustCompile(`(?m)^func (Test\w*)\(\w+ \*testing\.T\)`)
	for _, shard := range []struct{ variable, dir, suite string }{
		{"SESSIONSVC_SHARD", "internal/sessionsvc", "Test"},
		{"PROVIDER_SHARD", "internal/cli", "TestProviderScripted"},
	} {
		pattern, err := regexp.Compile(makeVariable(t, shard.variable))
		if err != nil {
			t.Fatalf("%s: %v", shard.variable, err)
		}
		files, err := filepath.Glob(filepath.Join(shard.dir, "*_test.go"))
		if err != nil {
			t.Fatal(err)
		}
		tests, matched := 0, 0
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range testFunc.FindAllSubmatch(data, -1) {
				if name := string(m[1]); strings.HasPrefix(name, shard.suite) {
					tests++
					if pattern.MatchString(name) {
						matched++
					}
				}
			}
		}
		if matched == 0 || matched == tests {
			t.Errorf("%s matches %d of the %d %s tests in %s: each shard must run some", shard.variable, matched, tests, shard.suite, shard.dir)
		}
	}
}

// checkCIOwnerCaches holds each free owner to one cache of its own: restored in every run,
// saved only by a main push after its checks pass, never shared with another owner.
func checkCIOwnerCaches(t *testing.T, steps []releaseStep) {
	t.Helper()
	restore, save, run := -1, -1, -1
	for i, step := range steps {
		switch {
		case strings.HasPrefix(step.Uses, "actions/cache/restore"+ciCachePinned):
			restore = i
			if step.Name != ciCacheRestore || step.With["path"] != ciCachePaths ||
				step.With["key"] != ciCacheKey+"${{ github.sha }}" || step.With["restore-keys"] != ciCacheKey+"\n" {
				t.Error("each owner must restore only its own cache of reviewed Go inputs")
			}
		case strings.HasPrefix(step.Uses, "actions/cache/save"+ciCachePinned):
			save = i
			if step.Name != ciCacheSave || step.With["path"] != ciCachePaths || step.With["key"] != ciCacheKey+"${{ github.sha }}" {
				t.Error("each owner must save only its own cache of reviewed Go inputs")
			}
		case strings.HasPrefix(step.Uses, "actions/cache"):
			t.Errorf("free owners use only the pinned cache restore and save, not %s", step.Uses)
		case step.Run == `make "${{ matrix.target }}"`:
			run = i
		}
	}
	if restore < 0 || save < 0 || !(restore < run && run < save) {
		t.Error("each owner must restore its cache before its checks and save it only after they pass")
	}
}

func readReleaseWorkflow(t *testing.T, path string) releaseWorkflow {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var workflow releaseWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

func TestReleaseWorkflowQualification(t *testing.T) {
	ci := readReleaseWorkflow(t, ".github/workflows/ci.yml")
	release := readReleaseWorkflow(t, ".github/workflows/release.yml")
	if len(ci.Jobs) != 4 {
		t.Fatal("CI must contain exactly its free checks, aggregate and two runtime boundaries")
	}
	if len(release.Jobs) != 2 {
		t.Error("release must have only qualification and its dependent publisher")
	}
	if _, ok := ci.On["workflow_call"]; !ok {
		t.Error("canonical CI must be callable by the release workflow")
	}
	readOnly := map[string]string{"contents": "read"}
	for name, workflow := range map[string]releaseWorkflow{"ci": ci, "release": release} {
		if !reflect.DeepEqual(workflow.Permissions, readOnly) {
			t.Errorf("%s workflow must default to contents-read only", name)
		}
		for id, job := range workflow.Jobs {
			expectedIf := ""
			if name == "ci" && id == "check" {
				expectedIf = "always()"
			}
			if job.If != expectedIf || job.ContinueOnError != nil || job.Secrets != nil {
				t.Errorf("%s.%s must not skip qualification, ignore failure, or inherit secrets", name, id)
			}
			if name == "ci" && job.Permissions != nil && !reflect.DeepEqual(job.Permissions, readOnly) {
				t.Errorf("CI job %s widens permissions", id)
			}
			checkouts := 0
			for _, step := range job.Steps {
				if step.ContinueOnError != nil {
					t.Errorf("%s.%s ignores a failed step", name, id)
				}
				stepIf := ""
				if name == "ci" && id == "checks" && step.Name == "install Go analyzers at the repo's pins" {
					stepIf = "matrix.target == 'check-static'"
				}
				if name == "ci" && id == "checks" && step.Name == ciCacheSave {
					stepIf = "github.event_name == 'push' && github.ref == 'refs/heads/main'"
				}
				if step.If != stepIf {
					t.Errorf("%s.%s unexpectedly skips a step: %s", name, id, step.If)
				}
				if strings.HasPrefix(step.Uses, "actions/checkout@") {
					checkouts++
					if step.With["ref"] != "${{ github.sha }}" || step.With["persist-credentials"] != "false" {
						t.Errorf("%s.%s checkout must use exact event SHA without persisted credentials", name, id)
					}
				}
			}
			statusOnly := name == "ci" && id == "check"
			if statusOnly {
				if checkouts != 0 || job.Uses != "" || job.Name != "check" || job.Timeout != 2 {
					t.Error("literal check aggregate must stay status-only and bounded, with no checkout")
				}
				for _, step := range job.Steps {
					if step.Uses != "" {
						t.Error("status aggregate must not use actions, caches or executable artifacts")
					}
				}
			} else if job.Uses == "" && checkouts != 1 {
				t.Errorf("%s.%s needs one exact checkout", name, id)
			}
		}
	}
	for _, job := range []string{"checks", "check", "doctor", "review-writes"} {
		if _, ok := ci.Jobs[job]; !ok {
			t.Errorf("release qualification lost CI job %s", job)
		}
	}
	aggregate := ci.Jobs["check"]
	if !reflect.DeepEqual(aggregate.Needs, []any{"checks", "doctor", "review-writes"}) || len(aggregate.Steps) != 1 {
		t.Fatal("required status must account for every free and runtime job")
	}
	if !reflect.DeepEqual(aggregate.Steps[0].Env, map[string]string{"RESULTS": "${{ join(needs.*.result, ' ') }}"}) {
		t.Error("aggregate must receive only its direct dependency results as data")
	}
	checks := ci.Jobs["checks"]
	owners := strings.Fields(makeVariable(t, "CI_OWNERS"))
	if checks.Needs != nil || checks.Strategy.FailFast == nil || *checks.Strategy.FailFast || checks.Strategy.MaxParallel != len(owners) ||
		!slices.Equal(checks.Strategy.Matrix["target"], owners) ||
		checks.Steps[len(checks.Steps)-2].Run != `make "${{ matrix.target }}"` {
		t.Error("free CI matrix must run exactly the Makefile's CI_OWNERS, each on an isolated runner of its own")
	}
	checkCIOwnerCaches(t, checks.Steps)
	for _, id := range []string{"checks", "doctor", "review-writes"} {
		setups := 0
		for _, step := range ci.Jobs[id].Steps {
			if strings.HasPrefix(step.Uses, "actions/setup-go@") {
				setups++
				if id == "checks" {
					if step.With["cache"] != "false" || step.With["go-version-file"] != "go.mod" {
						t.Error("the free owners must use the repo toolchain and leave caching to their own keys")
					}
				} else if step.With["cache"] != "true" || step.With["go-version-file"] != "go.mod" ||
					step.With["cache-dependency-path"] != "go.mod\ngo.sum\ntools/staticcheck.mod\ntools/staticcheck.sum\n" {
					t.Errorf("low-privilege %s must use the repo toolchain and cache only reviewed Go inputs", id)
				}
			}
		}
		if setups != 1 {
			t.Errorf("low-privilege %s needs exactly one pinned-toolchain setup", id)
		}
	}
	ciGate := release.Jobs["ci"]
	if ciGate.Uses != "./.github/workflows/ci.yml" || !reflect.DeepEqual(ciGate.Permissions, readOnly) {
		t.Error("release must reuse the same-commit CI workflow with read-only permission")
	}
	publish := release.Jobs["goreleaser"]
	if publish.Needs != "ci" {
		t.Error("publication must depend on successful exact-commit CI")
	}
	if !reflect.DeepEqual(publish.Permissions, map[string]string{
		"contents": "write", "id-token": "write", "attestations": "write",
	}) {
		t.Error("publisher permissions must remain explicitly scoped")
	}
	notes, remote, publisher, setups := -1, -1, -1, 0
	for i, step := range publish.Steps {
		if strings.HasPrefix(step.Uses, "actions/setup-go@") {
			setups++
			if step.With["cache"] != "false" || step.With["go-version-file"] != "go.mod" {
				t.Error("privileged publisher must use the repo toolchain without the unprivileged CI cache")
			}
		}
		if strings.HasPrefix(step.Uses, "actions/cache") || strings.HasPrefix(step.Uses, "actions/download-artifact@") {
			t.Error("publisher must never consume cached or downloaded executable build artifacts")
		}
		if strings.HasPrefix(step.Uses, "actions/checkout@") && step.With["fetch-depth"] != "0" {
			t.Error("publisher needs full tag history")
		}
		switch strings.TrimSpace(step.Run) {
		case `python3 tools/release_preflight.py notes "$RELEASE_TAG" > "$RUNNER_TEMP/release-notes.md"`:
			notes = i
		case `python3 tools/release_preflight.py verify-remote "$RELEASE_TAG"`:
			remote = i
		}
		if strings.Contains(step.Run, "tools/release_preflight.py") && step.Env["RELEASE_TAG"] != "${{ github.ref_name }}" {
			t.Error("tag must enter the helper as quoted data, not shell source")
		}
		if strings.HasPrefix(step.Uses, "goreleaser/goreleaser-action@") {
			publisher = i
			if step.Env["GORELEASER_CURRENT_TAG"] != "${{ github.ref_name }}" ||
				step.With["args"] != "release --clean --release-notes ${{ runner.temp }}/release-notes.md" {
				t.Error("GoReleaser must publish the validated event tag and notes, without skipped checks")
			}
		}
	}
	if setups != 1 {
		t.Error("publisher needs exactly one fresh pinned-toolchain setup")
	}
	if notes != 1 || remote < notes || publisher != remote+1 {
		t.Errorf("preflight must follow checkout and remote recheck immediately precede publisher: notes=%d remote=%d publisher=%d", notes, remote, publisher)
	}
	data, err := os.ReadFile(".goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Before    struct{ Hooks []string } `yaml:"before"`
		Changelog struct{ Disable bool }   `yaml:"changelog"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config.Changelog.Disable {
		t.Error("GoReleaser must keep changelog enabled to load --release-notes")
	}
	if len(config.Before.Hooks) != 2 || config.Before.Hooks[0] != "go mod tidy -diff" ||
		!strings.Contains(config.Before.Hooks[1], "go run . completion") {
		t.Error("before hooks must verify, not mutate, the qualified module graph and still generate completions")
	}
}

func TestReleaseCIAggregateRejectsFalseGreenDependencyStates(t *testing.T) {
	aggregate := readReleaseWorkflow(t, ".github/workflows/ci.yml").Jobs["check"]
	needs, _ := aggregate.Needs.([]any)
	if aggregate.If != "always()" || len(aggregate.Steps) != 1 || len(needs) == 0 {
		t.Fatal("aggregate must always execute its one status-only check over the jobs it needs")
	}
	// Actions joins one result per needed job, space-separated; run the step's shell on that.
	passes := func(results ...string) bool {
		cmd := exec.Command("sh", "-c", aggregate.Steps[0].Run)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "RESULTS=" + strings.Join(results, " ")}
		return cmd.Run() == nil
	}
	succeeded := slices.Repeat([]string{"success"}, len(needs))
	if !passes(succeeded...) {
		t.Fatal("aggregate must pass when every needed job succeeded")
	}
	if passes() || passes(succeeded[1:]...) || passes(append(slices.Clone(succeeded), "success")...) {
		t.Error("aggregate must require exactly one success per needed job")
	}
	for i := range succeeded {
		for _, result := range []string{"failure", "cancelled", "skipped", "", "unknown"} {
			results := slices.Clone(succeeded)
			results[i] = result
			if passes(results...) {
				t.Errorf("aggregate passed on results %q", strings.Join(results, " "))
			}
		}
	}
}
