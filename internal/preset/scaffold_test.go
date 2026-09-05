package preset

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestScaffoldFailuresAreInvisibleAndRetryable(t *testing.T) {
	for _, failure := range []string{"write", "validation", "publish"} {
		t.Run(failure, func(t *testing.T) {
			repo := t.TempDir()
			write := func(root *scaffoldBundle, name string) error {
				if err := writeScaffoldBundle(root, name); err != nil {
					return err
				}
				if got := List(repo, ""); len(got) != 0 {
					t.Fatalf("staging appeared as a preset: %v", got)
				}
				switch failure {
				case "write":
					return errors.New("injected write failure")
				case "validation":
					return root.Remove("roles/lead.md")
				}
				return nil
			}
			publish := func(_, _ *os.Root, _ string) error {
				if failure != "publish" {
					t.Fatal("invalid bundle reached publication")
				}
				return errors.New("injected publish failure")
			}
			// This valid name is also List's presence marker: the staging wrapper
			// must not directly contain a child named after the requested preset.
			if _, err := scaffold(repo, "preset.yaml", write, publish); err == nil {
				t.Fatal("injected failure was lost")
			}
			if entries, err := os.ReadDir(filepath.Join(repo, Dir)); err != nil || len(entries) != 0 {
				t.Fatalf("partial publication or staging remained: %v, %v", entries, err)
			}
			if _, err := Scaffold(repo, "preset.yaml"); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
			p, err := Load(repo, "", "preset.yaml")
			if err != nil || p.Name != "preset.yaml" || p.LeadPromptText == "" {
				t.Fatalf("retry did not publish a complete named preset: %+v, %v", p, err)
			}
		})
	}
}

func TestScaffoldPublishCollisionNeverReplaces(t *testing.T) {
	for _, shape := range []string{"empty", "file", "link"} {
		t.Run(shape, func(t *testing.T) {
			repo := t.TempDir()
			publish := func(stage, parent *os.Root, name string) error {
				switch shape {
				case "empty":
					if err := parent.Mkdir(name, 0o700); err != nil {
						t.Fatal(err)
					}
				case "file":
					if err := parent.WriteFile(name, []byte("other writer"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "link":
					if err := parent.Symlink("absent", name); err != nil {
						t.Fatal(err)
					}
				}
				before, err := parent.Lstat(name)
				if err != nil {
					t.Fatal(err)
				}
				publishErr := publishScaffoldBundle(stage, parent, name)
				after, err := parent.Lstat(name)
				if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
					t.Fatalf("concurrent destination changed: %v", err)
				}
				return publishErr
			}
			if _, err := scaffold(repo, "custom", writeScaffoldBundle, publish); err == nil || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("publish collision not refused: %v", err)
			}
			if entries, err := os.ReadDir(filepath.Join(repo, Dir)); err != nil || len(entries) != 1 || entries[0].Name() != "custom" {
				t.Fatalf("collision cleanup changed inventory: %v, %v", entries, err)
			}
		})
	}
}

func TestScaffoldPostPublicationSyncFailureKeepsPreset(t *testing.T) {
	repo := t.TempDir()
	publish := func(stage, parent *os.Root, name string) error {
		return publishScaffoldBundleSync(stage, parent, name, func(*os.File) error {
			return errors.New("injected directory sync failure")
		})
	}
	if _, err := scaffold(repo, "custom", writeScaffoldBundle, publish); err == nil || !strings.Contains(err.Error(), "was published") {
		t.Fatalf("publication boundary lost: %v", err)
	}
	if _, err := Load(repo, "", "custom"); err != nil {
		t.Fatalf("published preset was rolled back: %v", err)
	}
	if _, err := Scaffold(repo, "custom"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("retry replaced the published bundle: %v", err)
	}
}

func TestScaffoldConcurrentPublishers(t *testing.T) {
	repo := t.TempDir()
	start := make(chan struct{})
	errs := make(chan error, 8)
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			<-start
			_, err := Scaffold(repo, "shared")
			errs <- err
		})
	}
	close(start)
	group.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		} else if !strings.Contains(err.Error(), "already exists") {
			t.Errorf("unexpected concurrent error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("publish winners = %d, want 1", winners)
	}
	if _, err := Load(repo, "", "shared"); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(repo, Dir)); err != nil || len(entries) != 1 {
		t.Fatalf("concurrent stage cleanup: %v, %v", entries, err)
	}
}

func TestScaffoldPinnedValidationAndParentReplacement(t *testing.T) {
	repo, outside := t.TempDir(), t.TempDir()
	write := func(bundle *scaffoldBundle, name string) error {
		if err := writeScaffoldBundle(bundle, name); err != nil {
			return err
		}
		parent := filepath.Join(repo, Dir)
		if err := os.Rename(parent, parent+"-held"); err != nil {
			return err
		}
		if err := os.Symlink(outside, parent); err != nil {
			return err
		}
		data, err := bundle.ReadFile("preset.yaml")
		if err != nil {
			return err
		}
		loaded, err := loadPreset(name, parent, data, bundle.ReadFile)
		if err != nil || loaded.LeadPromptText != strings.TrimSpace(leadPrompt) {
			t.Fatalf("rooted validation followed a replaced parent: %+v, %v", loaded, err)
		}
		return nil
	}
	if _, err := scaffold(repo, "custom", write, publishScaffoldBundle); err == nil || !strings.Contains(err.Error(), "parent changed") {
		t.Fatalf("parent replacement not refused: %v", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("replacement tree modified: %v, %v", entries, err)
	}
	if entries, err := os.ReadDir(filepath.Join(repo, Dir) + "-held"); err != nil || len(entries) != 0 {
		t.Fatalf("owned staging not cleaned through pinned parent: %v, %v", entries, err)
	}
}

func TestScaffoldInterruptedStageDoesNotBlockRetry(t *testing.T) {
	repo := t.TempDir()
	leftover := filepath.Join(repo, Dir, ".coop-preset-interrupted", "bundle")
	if err := os.MkdirAll(leftover, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftover, "preset.yaml"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := List(repo, ""); len(got) != 0 {
		t.Fatalf("interrupted stage became discoverable: %v", got)
	}
	if _, err := Scaffold(repo, "custom"); err != nil {
		t.Fatal(err)
	}
	if got := List(repo, ""); len(got) != 1 || got[0] != "custom" {
		t.Fatalf("retry inventory = %v", got)
	}
	if data, err := os.ReadFile(filepath.Join(leftover, "preset.yaml")); err != nil || string(data) != "partial" {
		t.Fatal("retry erased staging owned by another attempt")
	}
}

func TestScaffoldPreservesReplacedStaging(t *testing.T) {
	for _, replace := range []string{"bundle", "wrapper"} {
		t.Run(replace, func(t *testing.T) {
			repo := t.TempDir()
			var marker string
			write := func(bundle *scaffoldBundle, name string) error {
				if err := writeScaffoldBundle(bundle, name); err != nil {
					return err
				}
				entries, err := os.ReadDir(filepath.Join(repo, Dir))
				if err != nil || len(entries) != 1 {
					t.Fatalf("expected one owned stage: %v, %v", entries, err)
				}
				path := filepath.Join(repo, Dir, entries[0].Name())
				if replace == "bundle" {
					path = filepath.Join(path, "bundle")
				}
				if err := os.Rename(path, path+"-held"); err != nil {
					return err
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					return err
				}
				marker = filepath.Join(path, "foreign.txt")
				return os.WriteFile(marker, []byte("another writer"), 0o600)
			}
			if _, err := scaffold(repo, "custom", write, publishScaffoldBundle); err == nil || !strings.Contains(err.Error(), "staging changed") {
				t.Fatalf("staging replacement accepted: %v", err)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "another writer" {
				t.Fatal("foreign staging replacement was removed")
			}
			if _, err := os.Lstat(filepath.Join(repo, Dir, "custom")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("replacement was published: %v", err)
			}
		})
	}
}

func TestScaffoldReportsLateParentReplacementWithoutRollback(t *testing.T) {
	repo, outside := t.TempDir(), t.TempDir()
	parentPath := filepath.Join(repo, Dir)
	publish := func(stage, parent *os.Root, name string) error {
		if err := os.Rename(parentPath, parentPath+"-held"); err != nil {
			return err
		}
		if err := os.Symlink(outside, parentPath); err != nil {
			return err
		}
		return publishScaffoldBundle(stage, parent, name)
	}
	if _, err := scaffold(repo, "custom", writeScaffoldBundle, publish); err == nil || !strings.Contains(err.Error(), "was published but its path changed") {
		t.Fatalf("stale successful path reported: %v", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("replacement parent modified: %v, %v", entries, err)
	}
	if data, err := os.ReadFile(filepath.Join(parentPath+"-held", "custom", "roles", "lead.md")); err != nil || string(data) != leadPrompt {
		t.Fatal("published bundle was rolled back after its parent moved")
	}
}

func TestScaffoldCleanupPreservesReplacedChildren(t *testing.T) {
	for _, replace := range []string{"preset.yaml", "roles/lead.md", "roles-directory", "roles-link"} {
		t.Run(replace, func(t *testing.T) {
			repo := t.TempDir()
			var marker string
			write := func(bundle *scaffoldBundle, name string) error {
				if err := writeScaffoldBundle(bundle, name); err != nil {
					return err
				}
				entries, err := os.ReadDir(filepath.Join(repo, Dir))
				if err != nil || len(entries) != 1 {
					t.Fatalf("expected one stage: %v, %v", entries, err)
				}
				root := filepath.Join(repo, Dir, entries[0].Name(), "bundle")
				rel := replace
				if strings.HasPrefix(replace, "roles-") {
					rel = "roles"
				}
				path := filepath.Join(root, filepath.FromSlash(rel))
				if err := os.Rename(path, path+"-held"); err != nil {
					return err
				}
				marker = path
				if strings.HasPrefix(replace, "roles-") {
					target := path
					if replace == "roles-link" {
						target = filepath.Join(root, "foreign")
					}
					if err := os.Mkdir(target, 0o700); err != nil {
						return err
					}
					if replace == "roles-link" {
						if err := os.Symlink("foreign", path); err != nil {
							return err
						}
					}
					marker = filepath.Join(path, "lead.md")
				}
				if err := os.WriteFile(marker, []byte("replacement owned by another writer"), 0o600); err != nil {
					return err
				}
				return errors.New("injected failure after child replacement")
			}
			if _, err := scaffold(repo, "custom", write, publishScaffoldBundle); err == nil {
				t.Fatal("injected failure lost")
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "replacement owned by another writer" {
				t.Fatalf("cleanup deleted another writer's child: %v", err)
			}
			if got := List(repo, ""); len(got) != 0 {
				t.Fatalf("failed stage was published: %v", got)
			}
		})
	}
}

func TestScaffoldRefusesSymlinkParents(t *testing.T) {
	for _, component := range []string{".agent", Dir} {
		t.Run(component, func(t *testing.T) {
			repo, outside := t.TempDir(), t.TempDir()
			link := filepath.Join(repo, component)
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			if _, err := Scaffold(repo, "custom"); err == nil {
				t.Error("symlink parent accepted")
			}
			if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
				t.Fatalf("symlink target modified: %v, %v", entries, err)
			}
		})
	}
}
