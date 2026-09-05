package box

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestRepositoryCopiesContainSources(t *testing.T) {
	for _, artifact := range []struct {
		name string
		path string
		dir  bool
	}{
		{"skills", ".agent/skills", true},
		{"settings", ".agent/claude/settings.json", false},
		{"hooks", ".agent/claude/hooks", true},
	} {
		for _, kind := range []string{"outside", "relative inside", "absolute inside"} {
			t.Run(artifact.name+"/"+kind, func(t *testing.T) {
				repo := t.TempDir()
				target := filepath.Join(repo, "source")
				if kind == "outside" {
					target = filepath.Join(t.TempDir(), "source")
				}
				content := target
				if artifact.dir {
					content = filepath.Join(target, "SKILL.md")
				}
				if err := os.MkdirAll(filepath.Dir(content), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(content, []byte("synthetic-source-canary"), 0o600); err != nil {
					t.Fatal(err)
				}
				source := filepath.Join(repo, artifact.path)
				if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
					t.Fatal(err)
				}
				link := target
				if kind == "relative inside" {
					var err error
					link, err = filepath.Rel(filepath.Dir(source), target)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(link, source); err != nil {
					t.Fatal(err)
				}
				var mounts []extraMount
				var dirs []string
				var err error
				if artifact.name == "skills" {
					mounts, dirs, err = synthSkillsMounts(repo, "/home/node", []string{"codex"})
				} else {
					mounts, dirs, err = synthHomeFallbackMounts(repo, "/home/node", []string{"claude"})
				}
				for _, dir := range dirs {
					t.Cleanup(func() { _ = os.RemoveAll(dir) })
				}
				if kind == "outside" {
					if err == nil || len(mounts) != 0 || len(dirs) != 0 {
						t.Fatalf("outside source exposed: mounts=%v dirs=%v err=%v", mounts, dirs, err)
					}
					return
				}
				if err != nil || len(mounts) != 1 {
					t.Fatalf("safe source: mounts=%v err=%v", mounts, err)
				}
				copied := mounts[0].host
				if artifact.dir {
					copied = filepath.Join(copied, "SKILL.md")
				}
				if data, err := os.ReadFile(copied); err != nil || string(data) != "synthetic-source-canary" {
					t.Fatalf("copy = %q, %v", data, err)
				}
			})
		}
	}
}

func writeCopyFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryCopiesRejectAncestorEscape(t *testing.T) {
	repo, outside := t.TempDir(), t.TempDir()
	writeCopyFixture(t, filepath.Join(outside, "skills", "SKILL.md"), "outside-canary")
	writeCopyFixture(t, filepath.Join(outside, "claude", "settings.json"), "outside-canary")
	if err := os.Symlink(outside, filepath.Join(repo, ".agent")); err != nil {
		t.Fatal(err)
	}
	if mounts, _, err := synthSkillsMounts(repo, "/home/node", []string{"codex"}); err == nil || len(mounts) != 0 {
		t.Fatalf("ancestor skills escape: %v, %v", mounts, err)
	}
	if mounts, _, err := synthHomeFallbackMounts(repo, "/home/node", []string{"claude"}); err == nil || len(mounts) != 0 {
		t.Fatalf("ancestor fallback escape: %v, %v", mounts, err)
	}
	if mounts, _, err := synthHomeFallbackMounts(repo, "/home/node", []string{"codex"}); err != nil || len(mounts) != 0 {
		t.Fatalf("inactive provider became a prerequisite: %v, %v", mounts, err)
	}
}

func TestRepositoryCopiesValidateRelocatedLinks(t *testing.T) {
	for _, kind := range []string{"relative", "absolute", "outside", "dangling", "cyclic"} {
		t.Run(kind, func(t *testing.T) {
			repo := t.TempDir()
			source := filepath.Join(repo, ".agent", "skills")
			writeCopyFixture(t, filepath.Join(source, "SKILL.md"), "inside")
			target := "SKILL.md"
			switch kind {
			case "absolute":
				var err error
				target, err = filepath.EvalSymlinks(filepath.Join(source, "SKILL.md"))
				if err != nil {
					t.Fatal(err)
				}
			case "outside":
				target = filepath.Join(t.TempDir(), "canary")
				writeCopyFixture(t, target, "outside")
			case "dangling":
				target = "missing"
			case "cyclic":
				target = "alias"
			}
			if err := os.Symlink(target, filepath.Join(source, "alias")); err != nil {
				t.Fatal(err)
			}
			mounts, dirs, err := synthSkillsMounts(repo, "/home/node", []string{"codex", "gemini"})
			for _, dir := range dirs {
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
			}
			if kind != "relative" && kind != "absolute" {
				if err == nil || len(mounts) != 0 || len(dirs) != 0 {
					t.Fatalf("invalid copied link exposed: %v %v %v", mounts, dirs, err)
				}
				return
			}
			if err != nil || len(mounts) != 2 {
				t.Fatalf("safe copied link refused: %v %v", mounts, err)
			}
			for _, mount := range mounts {
				link := filepath.Join(mount.host, "alias")
				if data, err := os.ReadFile(link); err != nil || string(data) != "inside" {
					t.Fatalf("relocated link = %q, %v", data, err)
				}
				if _, err := os.Readlink(link); err != nil {
					t.Fatalf("safe link was not preserved: %v", err)
				}
			}
		})
	}
}

func TestRepositoryCopiesKeepPinnedSourceAuthority(t *testing.T) {
	repo, outside := t.TempDir(), t.TempDir()
	writeCopyFixture(t, filepath.Join(repo, "source", "SKILL.md"), "inside")
	writeCopyFixture(t, filepath.Join(outside, "SKILL.md"), "outside-canary")
	sources, err := openRepositorySources(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer sources.root.Close()
	tree, err := sources.openTree("source")
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if err := os.Rename(filepath.Join(repo, "source"), filepath.Join(repo, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "source")); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := copySourceTree(dst, tree); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dst, "SKILL.md")); err != nil || string(data) != "inside" {
		t.Fatalf("pinned copy = %q, %v", data, err)
	}
	if data, err := sources.readFile("source/SKILL.md"); err == nil || len(data) != 0 {
		t.Fatalf("replacement source read = %q, %v", data, err)
	}
}

func TestRepositoryCopiesRejectSpecialAndOversizedFiles(t *testing.T) {
	repo := t.TempDir()
	writeCopyFixture(t, filepath.Join(repo, "settings"), "{}")
	sources, err := openRepositorySources(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer sources.root.Close()
	if present, err := sources.exists("settings", false); err != nil || !present {
		t.Fatalf("initial regular file = %v, %v", present, err)
	}
	if err := os.Rename(filepath.Join(repo, "settings"), filepath.Join(repo, "original")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(repo, "settings"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sources.readFile("settings"); err == nil {
		t.Fatal("opened a replacement FIFO as settings")
	}
	if f, err := (sourceCopyFS{root: sources.root}).Open("settings"); err == nil {
		f.Close()
		t.Fatal("opened a replacement FIFO as tree content")
	}
	writeCopyFixture(t, filepath.Join(repo, "large"), strings.Repeat("x", maxFallbackFileBytes+1))
	if _, err := sources.readFile("large"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized settings = %v", err)
	}
	if present, err := sources.exists("missing", false); err != nil || present {
		t.Fatalf("optional missing file = %v, %v", present, err)
	}
	if err := os.Symlink("missing", filepath.Join(repo, "dangling")); err != nil {
		t.Fatal(err)
	}
	if _, err := sources.exists("dangling", false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dangling artifact = %v", err)
	}
}

func TestRepositoryCopiesRequirePrivateDestinations(t *testing.T) {
	repo := t.TempDir()
	writeCopyFixture(t, filepath.Join(repo, ".agent", "skills", "SKILL.md"), "inside")
	writeCopyFixture(t, filepath.Join(repo, ".agent", "claude", "settings.json"), "{}")
	t.Setenv("TMPDIR", repo)
	if mounts, _, err := synthSkillsMounts(repo, "/home/node", []string{"codex"}); err == nil || len(mounts) != 0 {
		t.Fatalf("writable skills copy = %v, %v", mounts, err)
	}
	if mounts, _, err := synthHomeFallbackMounts(repo, "/home/node", []string{"claude"}); err == nil || len(mounts) != 0 {
		t.Fatalf("writable settings copy = %v, %v", mounts, err)
	}
}

func TestRepositoryCopiesCleanEarlierFallbackOnLaterFailure(t *testing.T) {
	repo, temporary, outside := t.TempDir(), t.TempDir(), t.TempDir()
	writeCopyFixture(t, filepath.Join(repo, ".agent", "claude", "settings.json"), "{}")
	if err := os.Symlink(outside, filepath.Join(repo, ".agent", "claude", "hooks")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", temporary)
	if mounts, dirs, err := synthHomeFallbackMounts(repo, "/home/node", []string{"claude"}); err == nil || len(mounts) != 0 || len(dirs) != 0 {
		t.Fatalf("failed fallback exposed copies: %v %v %v", mounts, dirs, err)
	}
	entries, err := os.ReadDir(temporary)
	if err != nil || len(entries) != 0 {
		t.Fatalf("earlier fallback leaked after later failure: %v, %v", entries, err)
	}
}

func TestRepositoryCopiesHandleCaseAliases(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repository")
	writeCopyFixture(t, filepath.Join(repo, "source", "SKILL.md"), "inside")
	alias := filepath.Join(filepath.Dir(repo), "REPOSITORY")
	actual, err := os.Stat(repo)
	if err != nil {
		t.Fatal(err)
	}
	alternate, err := os.Stat(alias)
	if err != nil || !os.SameFile(actual, alternate) {
		t.Skip("filesystem is case-sensitive")
	}
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(alias, "source"), filepath.Join(repo, ".agent", "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(alias, "source", "SKILL.md"), filepath.Join(repo, "source", "alias")); err != nil {
		t.Fatal(err)
	}
	mounts, dirs, err := synthSkillsMounts(repo, "/home/node", []string{"codex"})
	for _, dir := range dirs {
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	if err != nil || len(mounts) != 1 {
		t.Errorf("safe case aliases refused: %v %v", mounts, err)
	} else if data, err := os.ReadFile(filepath.Join(mounts[0].host, "alias")); err != nil || string(data) != "inside" {
		t.Errorf("relocated alias = %q, %v", data, err)
	}
	t.Setenv("TMPDIR", alias)
	if dir, err := privateWorkspaceTempDir(repo, "coop-case-test-"); err == nil {
		_ = os.RemoveAll(dir)
		t.Fatal("case alias placed private output in writable workspace")
	}
}

func TestPrivateCopiesAndComposeRejectExposedRoots(t *testing.T) {
	repo, source := writeCompose(t, "services:\n  db:\n    image: postgres:18\n")
	writeCopyFixture(t, filepath.Join(repo, ".agent", "skills", "SKILL.md"), "inside")
	writeCopyFixture(t, filepath.Join(repo, ".agent", "claude", "settings.json"), "{}")
	exposed := t.TempDir()
	t.Setenv("TMPDIR", exposed)
	assertDenied := func(err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "outside agent-exposed") {
			t.Fatalf("private allocation = %v", err)
		}
	}
	_, _, err := synthSkillsMounts(repo, "/home/node", []string{"codex"}, exposed)
	assertDenied(err)
	_, _, err = synthHomeFallbackMounts(repo, "/home/node", []string{"claude"}, exposed)
	assertDenied(err)
	_, _, err = snapshotComposeArgs(repo, source, exposed)
	assertDenied(err)
	_, _, err = writeServiceOverride([]ServicePort{{Service: "db", ContainerPort: 5432, HostPort: 25432}}, repo, exposed)
	assertDenied(err)
	_, err = EnsureServicesFile(runtime.Runtime{}, repo, source, io.Discard, io.Discard, exposed)
	assertDenied(err)
	assertDenied(DownServicesFile(runtime.Runtime{}, repo, source, false, io.Discard, io.Discard, exposed))
	entries, err := os.ReadDir(exposed)
	if err != nil || len(entries) != 0 {
		t.Fatalf("private artifacts published before refusal: %v %v", entries, err)
	}
}

func TestRepositorySourcesAcceptRelativeRepository(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	writeCopyFixture(t, filepath.Join(repo, ".agent", "skills", "SKILL.md"), "inside")
	t.Chdir(parent)
	mounts, dirs, err := synthSkillsMounts("repo", "/home/node", []string{"codex"})
	for _, dir := range dirs {
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	if err != nil || len(mounts) != 1 {
		t.Fatalf("relative repository = %v, %v", mounts, err)
	}
}

func TestPrivateCopiesResolveRelativeTempDirectory(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(repo, "subdir"))
	t.Setenv("TMPDIR", ".")
	if dir, err := privateWorkspaceTempDir(repo, "coop-relative-"); err == nil {
		_ = os.RemoveAll(dir)
		t.Error("relative TMPDIR exposed private copy inside repository")
	}
	t.Setenv("TMPDIR", "../..")
	dir, err := privateWorkspaceTempDir(repo, "coop-relative-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if !filepath.IsAbs(dir) {
		t.Fatalf("private path retained mutable cwd authority: %q", dir)
	}
}

func TestPrivateCopiesIgnoreNonDirectoryInactiveACPRoot(t *testing.T) {
	repo, temporary := t.TempDir(), t.TempDir()
	cfg := &config.Config{ConfigDir: t.TempDir()}
	writeCopyFixture(t, acpSharedDir(cfg, "claude"), "unused provider artifact")
	t.Setenv("TMPDIR", temporary)
	dir, err := privateWorkspaceTempDir(repo, "coop-inactive-", ConfigExposureRoots(cfg)...)
	if err != nil {
		t.Fatalf("unused non-directory ACP root became a prerequisite: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
}
