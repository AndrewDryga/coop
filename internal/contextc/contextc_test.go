package contextc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/project"
)

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"portal/**", "portal/lib/x.ex", true},
		{"portal/**", "portal", true},
		{"portal/**", "runner/x", false},
		{"**/*.ex", "a.ex", true},
		{"**/*.ex", "a/b/c.ex", true},
		{"**/*.ex", "a/b.go", false},
		{"*.go", "main.go", true},
		{"*.go", "a/main.go", false},
		{"lib/*.go", "lib/a.go", true},
		{"lib/*.go", "lib/a/b.go", false},
		{"docs/**/*.md", "docs/a/b.md", true},
		{"docs/**/*.md", "docs/a.md", true},
	}
	for _, c := range cases {
		if got := matchGlob(c.pat, c.name); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pat, c.name, got, c.want)
		}
	}
}

func writeRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for rel, body := range map[string]string{
		"AGENTS.md":              "agents",
		".agent/kb/portal.md":    "portal kb",
		".agent/rules/elixir.md": "elixir rule",
	} {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func fileList(sel []Selected) []string {
	out := make([]string, len(sel))
	for i, s := range sel {
		out[i] = s.File
	}
	return out
}

// Overlapping routes + ordering + dedup: canonical first, routes in declaration order, a doc
// reached twice appears once at its earliest position, and each carries the route that selected it.
func TestCompileOrderingAndDedup(t *testing.T) {
	repo := writeRepo(t)
	routes := []project.Route{
		{Paths: []string{"portal/**"}, Include: []string{".agent/kb/portal.md"}},
		{Paths: []string{"**/*.ex"}, Include: []string{".agent/rules/elixir.md", ".agent/kb/portal.md"}},
	}
	sel, err := Compile(repo, routes, []string{"portal/lib/user.ex"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"AGENTS.md", ".agent/kb/portal.md", ".agent/rules/elixir.md"}
	if got := fileList(sel); !equal(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
	if sel[0].Reason != "canonical" {
		t.Errorf("first file must be canonical, got %q", sel[0].Reason)
	}
	if sel[1].Reason != "route portal/** → portal/lib/user.ex" {
		t.Errorf("route reason = %q", sel[1].Reason)
	}
}

// No scope → no route fires; only the canonical instruction file(s), still whole.
func TestCompileCanonicalOnly(t *testing.T) {
	repo := writeRepo(t)
	routes := []project.Route{{Paths: []string{"portal/**"}, Include: []string{".agent/kb/portal.md"}}}
	sel, err := Compile(repo, routes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileList(sel); !equal(got, []string{"AGENTS.md"}) {
		t.Errorf("empty scope should give only canonical, got %v", got)
	}
}

// A CLAUDE.md that symlinks to AGENTS.md is the same file — dedup by real path keeps just one.
func TestCompileCanonicalSymlinkDedup(t *testing.T) {
	repo := writeRepo(t)
	if err := os.Symlink(filepath.Join(repo, "AGENTS.md"), filepath.Join(repo, "CLAUDE.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	sel, err := Compile(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileList(sel); !equal(got, []string{"AGENTS.md"}) {
		t.Errorf("CLAUDE.md symlinked to AGENTS.md should dedup to one, got %v", got)
	}
}

// A selected include that doesn't exist is a hard error (missing required file).
func TestCompileMissingInclude(t *testing.T) {
	repo := writeRepo(t)
	routes := []project.Route{{Paths: []string{"**"}, Include: []string{".agent/kb/nope.md"}}}
	if _, err := Compile(repo, routes, []string{"x"}); err == nil {
		t.Error("a missing route include must error")
	}
}

// An include reached through a symlink that escapes the repo is rejected (never pull a host file in).
func TestCompileEscapingSymlink(t *testing.T) {
	repo := writeRepo(t)
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".agent", "evil.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	routes := []project.Route{{Paths: []string{"**"}, Include: []string{".agent/evil.md"}}}
	if _, err := Compile(repo, routes, []string{"x"}); err == nil {
		t.Error("an escaping-symlink include must be rejected")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
