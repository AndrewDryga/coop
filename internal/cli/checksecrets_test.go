package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

	findings, err := scanVisibleTree(repo)
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
