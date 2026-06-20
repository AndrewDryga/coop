package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanVisibleTreeSkipsGitignored(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	token := `api_key = "aB3xK9mP2qL7vR4tY8wZ1cF6nH5jD0sUvWx"`
	mk := func(rel, content string) {
		t.Helper()
		full := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk(".gitignore", "serviceAccount.json\n")
	mk("serviceAccount.json", token) // gitignored but not shadowed → visible to the agent
	mk("config.tf", token)           // tracked-able source → must be scanned

	// Default mode: the gitignored file is out of the commit-candidate set.
	findings, err := scanVisibleTree(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(findings, "\n")
	if strings.Contains(joined, "serviceAccount.json") {
		t.Errorf("default scan should skip the gitignored file:\n%s", joined)
	}
	if !strings.Contains(joined, "config.tf") {
		t.Errorf("missed the secret in the tracked file:\n%s", joined)
	}

	// --include-ignored: the box can see the gitignored file (whole tree is mounted), so flag it.
	ignored, err := scanVisibleTree(repo, true)
	if err != nil {
		t.Fatal(err)
	}
	if j := strings.Join(ignored, "\n"); !strings.Contains(j, "serviceAccount.json") {
		t.Errorf("--include-ignored should scan the gitignored file:\n%s", j)
	}
}

// TestScanVisibleTreeIncludeIgnoredSkipsShadowedAndDeps: even in --include-ignored mode, a
// shadowed secret (.env) stays skipped and dependency dirs aren't walked.
func TestScanVisibleTreeIncludeIgnoredSkipsShadowedAndDeps(t *testing.T) {
	repo := t.TempDir()
	token := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	mk := func(rel, content string) {
		t.Helper()
		full := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk(".env", "GH="+token+"\n")                // shadowed → never scanned, either mode
	mk("node_modules/pkg/index.js", "x="+token) // dependency dir → pruned
	mk("secrets.txt", "token: "+token+"\n")     // plain visible file → flagged

	findings, err := scanVisibleTree(repo, true)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(findings, "\n")
	if strings.Contains(joined, ".env") {
		t.Errorf("shadowed .env must stay skipped in --include-ignored mode:\n%s", joined)
	}
	if strings.Contains(joined, "node_modules") {
		t.Errorf("dependency dirs must be pruned:\n%s", joined)
	}
	if !strings.Contains(joined, "secrets.txt") {
		t.Errorf("missed the token in a visible file:\n%s", joined)
	}
}

func TestScanVisibleTree(t *testing.T) {
	repo := t.TempDir()
	mk := func(rel, content string) {
		t.Helper()
		full := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A token in an ordinary (non-shadowed) file → the box sees it, so flag it.
	mk("config/prod.yml", "host: db\ntoken: ghp_abcdefghijklmnopqrstuvwxyz0123456789\n")
	// The same shape in a shadowed .env → the box never sees it, so don't flag it.
	mk(".env", "GH=ghp_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz\n")
	// A clean file → no finding.
	mk("README.md", "nothing secret here\n")

	findings, err := scanVisibleTree(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(findings, "\n")
	if !strings.Contains(joined, "config/prod.yml") {
		t.Errorf("missed the token in the visible config/prod.yml:\n%s", joined)
	}
	if strings.Contains(joined, ".env") {
		t.Errorf("flagged a secret in the shadowed .env (the box can't see it):\n%s", joined)
	}
	if strings.Contains(joined, "README.md") {
		t.Errorf("false positive on a clean file:\n%s", joined)
	}
}

func TestReadScannable(t *testing.T) {
	dir := t.TempDir()
	text := filepath.Join(dir, "a.txt")
	os.WriteFile(text, []byte("plain text\n"), 0o644)
	if s, ok := readScannable(text); !ok || s == "" {
		t.Errorf("text file should be scannable, got ok=%v", ok)
	}
	bin := filepath.Join(dir, "b.bin")
	os.WriteFile(bin, []byte("PNG\x00\x01\x02binary"), 0o644)
	if _, ok := readScannable(bin); ok {
		t.Error("binary file (NUL byte) should be skipped")
	}
}
