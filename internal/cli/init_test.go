package cli

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/scaffold"
)

func TestPromptGateLangs(t *testing.T) {
	// Recognized tokens are kept in order (deduped); unknown ignored.
	if got := promptGateLangs(strings.NewReader("terraform go go bogus\n")); !slices.Equal(got, []string{"terraform", "go"}) {
		t.Errorf("prompt = %v, want [terraform go]", got)
	}
	// Blank / unknown-only / no input → nil (no gate imposed).
	for _, in := range []string{"\n", "nonsense\n", ""} {
		if got := promptGateLangs(strings.NewReader(in)); got != nil {
			t.Errorf("%q → %v, want nil", in, got)
		}
	}
}

type initTreeEntry struct {
	info os.FileInfo
	data string
	link string
}

func snapshotInitTree(t *testing.T, root string) map[string]initTreeEntry {
	t.Helper()
	entries := map[string]initTreeEntry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := initTreeEntry{info: info}
		switch {
		case info.Mode().IsRegular():
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			entry.data = string(data)
		case info.Mode()&os.ModeSymlink != 0:
			entry.link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestInitStackPreflightPreservesTree(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "none"))
	for _, api := range []string{"scaffold", "cli"} {
		for _, state := range []string{"fresh", "initialized"} {
			for _, failure := range []string{"unknown", "unknown-with-tools", "asdf-missing-tools"} {
				t.Run(api+"/"+state+"/"+failure, func(t *testing.T) {
					root := t.TempDir()
					repo, cfgDir := filepath.Join(root, "repo"), filepath.Join(root, "config")
					write := func(rel, data string, mode os.FileMode) {
						t.Helper()
						path := filepath.Join(root, rel)
						if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte(data), mode); err != nil {
							t.Fatal(err)
						}
					}
					write("repo/README.md", "project\n", 0o644)
					write("config/keep.conf", "preserve config\n", 0o600)
					write("outside/canary", "outside data\n", 0o600)
					if err := os.Symlink("../outside", filepath.Join(repo, "outside-link")); err != nil {
						t.Fatal(err)
					}
					if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
						t.Fatalf("git init: %v, %s", err, out)
					}
					if state == "initialized" {
						if err := scaffold.Init(repo, "", nil, []string{"claude", "codex", "gemini"}); err != nil {
							t.Fatal(err)
						}
						write("repo/AGENTS.md", "custom instructions\n", 0o600)
						write("repo/.githooks/pre-commit", "#!/bin/sh\nexit 0\n", 0o750)
						write("repo/.agent/Dockerfile", "FROM custom\n", 0o640)
					}
					if failure == "unknown-with-tools" {
						write("repo/.tool-versions", "golang 1.26.4\n", 0o644)
					}
					stack := "not-a-stack"
					if failure == "asdf-missing-tools" {
						stack = "asdf"
					}
					a := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: cfgDir, MCPFile: filepath.Join(cfgDir, "mcp.json")}}
					run := func() error {
						if api == "scaffold" {
							return scaffold.Init(repo, stack, []string{"go"}, []string{"claude", "codex", "gemini"})
						}
						_, err := a.cmdInit([]string{"--stack", stack, "--services", "postgres", "--agents", "all"})
						return err
					}
					before := snapshotInitTree(t, root)
					// A file keeps the old implementation's long progress output from
					// filling a pipe while this regression is expected to fail.
					log, err := os.CreateTemp(t.TempDir(), "init-output-")
					if err != nil {
						t.Fatal(err)
					}
					old := os.Stderr
					os.Stderr = log
					defer func() { os.Stderr = old; _ = log.Close() }()
					runErr := run()
					os.Stderr = old
					if err := log.Close(); err != nil {
						t.Fatal(err)
					}
					if runErr == nil || !strings.Contains(runErr.Error(), "--stack") {
						t.Fatalf("invalid stack not refused: %v", runErr)
					}
					if data, err := os.ReadFile(log.Name()); err != nil || len(data) != 0 {
						t.Errorf("denied init produced %d progress bytes: %v", len(data), err)
					}
					after := snapshotInitTree(t, root)
					if len(before) != len(after) {
						t.Fatalf("denied init changed tree inventory: %d -> %d entries", len(before), len(after))
					}
					for name, want := range before {
						got, ok := after[name]
						if !ok || !os.SameFile(want.info, got.info) || want.info.Mode() != got.info.Mode() || want.data != got.data || want.link != got.link {
							t.Fatalf("denied init changed %s", name)
						}
					}
					if failure == "asdf-missing-tools" {
						write("repo/.tool-versions", "golang 1.26.4\n", 0o644)
					} else {
						stack = ""
					}
					if err := run(); err != nil {
						t.Fatalf("corrected retry failed: %v", err)
					}
					if !scaffold.Initialized(repo) {
						t.Fatal("corrected retry did not initialize the repo")
					}
					if api == "cli" {
						for _, path := range []string{a.cfg.MCPFile, filepath.Join(repo, ".agent", "compose.yml")} {
							if _, err := os.Stat(path); err != nil {
								t.Fatalf("valid retry did not write %s: %v", path, err)
							}
						}
					}
				})
			}
		}
	}
}
