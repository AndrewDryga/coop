package preset

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestScaffoldPreservesEveryExistingDestination(t *testing.T) {
	for _, entry := range []string{"empty", "file", "link", "dangling", "prompt-link", "roles-link", "preset.yaml", "roles/lead.md", "roles/thinker.md", "roles/fast.md"} {
		t.Run(entry, func(t *testing.T) {
			repo, outside := t.TempDir(), t.TempDir()
			dest := filepath.Join(repo, Dir, "custom")
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				t.Fatal(err)
			}
			canary := filepath.Join(outside, "keep.md")
			if err := os.WriteFile(canary, []byte("outside evidence"), 0o600); err != nil {
				t.Fatal(err)
			}
			kept := dest
			switch entry {
			case "file":
				if err := os.WriteFile(dest, []byte("user file"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "link", "dangling":
				target := outside
				if entry == "dangling" {
					target = filepath.Join(outside, "absent")
				}
				if err := os.Symlink(target, dest); err != nil {
					t.Fatal(err)
				}
			case "prompt-link", "roles-link":
				kept = filepath.Join(dest, "roles")
				target := outside
				if entry == "prompt-link" {
					kept = filepath.Join(kept, "lead.md")
					target = canary
				}
				if err := os.MkdirAll(filepath.Dir(kept), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, kept); err != nil {
					t.Fatal(err)
				}
			default:
				if entry != "empty" {
					kept = filepath.Join(dest, filepath.FromSlash(entry))
					if err := os.MkdirAll(filepath.Dir(kept), 0o750); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(kept, []byte("custom prompt"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(dest, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(kept)
			if err != nil {
				t.Fatal(err)
			}
			beforeData, _ := os.ReadFile(kept)
			beforeLink, _ := os.Readlink(kept)
			if path, err := Scaffold(repo, "custom"); path != "" || err == nil || !strings.Contains(err.Error(), "already exists") {
				t.Errorf("existing destination was not refused: %q, %v", path, err)
			}
			after, err := os.Lstat(kept)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("existing entry identity/mode changed: %v", err)
			}
			afterData, _ := os.ReadFile(kept)
			afterLink, _ := os.Readlink(kept)
			if string(afterData) != string(beforeData) || afterLink != beforeLink {
				t.Fatal("existing entry content changed")
			}
			if entry != "preset.yaml" {
				if _, err := os.Lstat(filepath.Join(dest, "preset.yaml")); err == nil {
					t.Fatal("refusal published preset.yaml into an existing destination")
				}
			}
			if data, err := os.ReadFile(canary); err != nil || string(data) != "outside evidence" {
				t.Fatal("outside canary changed")
			}
			if entries, err := os.ReadDir(outside); err != nil || len(entries) != 1 {
				t.Fatalf("outside tree changed: %v, %v", entries, err)
			}
		})
	}
}

// An interrupted scaffold must never look like a complete preset, so preset.yaml is written last.
func TestScaffoldWritesPresetFileLast(t *testing.T) {
	files := scaffoldFiles("custom")
	if last := files[len(files)-1].rel; last != "preset.yaml" {
		t.Fatalf("last scaffold write = %s, want preset.yaml", last)
	}
	for _, f := range files[:len(files)-1] {
		if f.rel == "preset.yaml" {
			t.Fatal("preset.yaml is written before its prompts")
		}
	}
}

// A failed scaffold leaves no folder behind, so the retry is an ordinary create rather than an
// "incomplete preset" refusal.
func TestScaffoldFailuresLeaveNothingBehind(t *testing.T) {
	saved := templateFiles
	t.Cleanup(func() { templateFiles = saved })
	for failure, files := range map[string][]templateFile{
		// A prompt the recipe names is missing: validation refuses before any write.
		"validation": saved[1:],
		// The same prompt twice: its second exclusive create fails after real files were written.
		"write": append(slices.Clone(saved), saved[0]),
	} {
		t.Run(failure, func(t *testing.T) {
			repo := t.TempDir()
			templateFiles = files
			if _, err := Scaffold(repo, "custom"); err == nil {
				t.Fatal("injected failure was lost")
			}
			if _, err := os.Lstat(filepath.Join(repo, Dir, "custom")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed scaffold left the preset folder: %v", err)
			}
			templateFiles = saved
			if _, err := Scaffold(repo, "custom"); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
			if p, err := Load(repo, "", "custom"); err != nil || p.LeadPromptText == "" {
				t.Fatalf("retry did not write a complete preset: %+v, %v", p, err)
			}
		})
	}
}

// Load never follows a link in a preset's path, so Scaffold refuses one too, even a link that stays
// inside the repository; a link that leaves it must not receive a single file.
func TestScaffoldRefusesSymlinkParents(t *testing.T) {
	for _, component := range []string{".agent", Dir} {
		for _, place := range []string{"outside", "inside"} {
			t.Run(component+"/"+place, func(t *testing.T) {
				repo, target := t.TempDir(), t.TempDir()
				link := filepath.Join(repo, component)
				if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
				linkTo := target
				if place == "inside" {
					target = filepath.Join(repo, "elsewhere")
					if err := os.Mkdir(target, 0o755); err != nil {
						t.Fatal(err)
					}
					// os.Root follows a relative link that stays inside it, so only a check refuses it.
					rel, err := filepath.Rel(filepath.Dir(link), target)
					if err != nil {
						t.Fatal(err)
					}
					linkTo = rel
				}
				if err := os.Symlink(linkTo, link); err != nil {
					t.Fatal(err)
				}
				if _, err := Scaffold(repo, "custom"); err == nil {
					t.Error("symlink parent accepted")
				}
				if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
					t.Fatalf("symlink target modified: %v, %v", entries, err)
				}
			})
		}
	}
}
