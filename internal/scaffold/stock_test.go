package scaffold

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Every file and link coop init writes is stock, whatever a person answered — agents or none,
// languages in the detected or a typed order, services, a .tool-versions, monorepo members — and
// the moment one byte or one link target changes it is not. check-secrets relies on both halves:
// the first keeps Coop's own files out of the review list, the second keeps every edit in it.
func TestStockMatchesEveryFileInitWrites(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		langs, agents, services []string
		toolVersions            string
		member                  string
	}{
		{name: "every agent, detected languages", langs: []string{"go", "rust"}, agents: []string{"claude", "codex", "gemini"}},
		{name: "no agent, typed languages and services", langs: []string{"rust", "go"}, services: []string{"redis", "postgres"},
			toolVersions: "erlang 29.0.1\nelixir 1.20.0-otp-29\n"},
		{name: "neutral gate, one service, a member", agents: []string{"claude"}, services: []string{"postgres"}, member: "api"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if tc.toolVersions != "" {
				if err := os.WriteFile(filepath.Join(repo, ".tool-versions"), []byte(tc.toolVersions), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.member != "" {
				if err := os.MkdirAll(filepath.Join(repo, tc.member, ".agent"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Init(repo, "", tc.langs, tc.agents, tc.services...); err != nil {
				t.Fatal(err)
			}
			if err := WriteCompose(repo, tc.services); err != nil {
				t.Fatal(err)
			}
			written := 0
			err := filepath.WalkDir(repo, func(path string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				rel, err := filepath.Rel(repo, path)
				if err != nil {
					return err
				}
				rel = filepath.ToSlash(rel)
				// .gitignore is edited in place around a project's own lines, and .tool-versions is
				// the project's; neither is a file that runs commands.
				if rel == ".gitignore" || rel == ".tool-versions" {
					return nil
				}
				written++
				if !Stock(repo, rel) {
					t.Errorf("%s was written by init but is not stock", rel)
					return nil
				}
				if entry.Type()&fs.ModeSymlink != 0 {
					target, err := os.Readlink(path)
					if err != nil {
						return err
					}
					if err := os.Remove(path); err != nil {
						return err
					}
					if err := os.Symlink("../elsewhere", path); err != nil {
						return err
					}
					if Stock(repo, rel) {
						t.Errorf("%s still reads as stock after its link moved", rel)
					}
					if err := os.Remove(path); err != nil {
						return err
					}
					return os.Symlink(target, path)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
					return err
				}
				if Stock(repo, rel) {
					t.Errorf("%s still reads as stock after an edit", rel)
				}
				return os.WriteFile(path, data, 0o644)
			})
			if err != nil {
				t.Fatal(err)
			}
			if written < 10 {
				t.Fatalf("init wrote only %d files; the walk did not see the scaffold", written)
			}
		})
	}
}

// A file init never writes, a missing one and a stock file's bytes at another path are not stock:
// identity is the path and its bytes together.
func TestStockNeedsThePathAndTheBytes(t *testing.T) {
	repo := t.TempDir()
	if _, err := Init(repo, "", nil, []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	settings, err := os.ReadFile(filepath.Join(repo, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".codex", "settings.json"), settings, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".codex/settings.json", ".claude/missing.json", "Makefile"} {
		if Stock(repo, rel) {
			t.Errorf("%s reads as stock", rel)
		}
	}
}
