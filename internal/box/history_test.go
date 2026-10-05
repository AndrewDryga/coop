package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// withHistoryLayout gives claude a history layout for one test; no real provider declares one yet.
func withHistoryLayout(t *testing.T, layout agents.HistoryLayout) {
	t.Helper()
	previous := historyLayout
	historyLayout = func(agent string) agents.HistoryLayout {
		if agent == "claude" {
			return layout
		}
		return agents.HistoryLayout{}
	}
	t.Cleanup(func() { historyLayout = previous })
}

func TestHistoryKeyFollowsTheCanonicalRepository(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "My Repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	key, project, err := HistoryKey(repo)
	if err != nil {
		t.Fatal(err)
	}
	if viaLink, _, err := HistoryKey(link); err != nil || viaLink != key {
		t.Errorf("a symlinked spelling got its own store: %q vs %q (%v)", viaLink, key, err)
	}
	if !strings.HasPrefix(key, "My-Repo-") || len(key) != len("My-Repo-")+16 || strings.ContainsAny(key, "/ ") {
		t.Errorf("key %q should be the sanitized folder name and 16 hex digits", key)
	}
	other := filepath.Join(base, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if otherKey, _, _ := HistoryKey(other); otherKey == key {
		t.Error("two repositories share a store key")
	}
	if !filepath.IsAbs(project) {
		t.Errorf("project %q should be canonical and absolute", project)
	}
	if _, _, err := HistoryKey("relative/path"); err == nil {
		t.Error("a relative repository path must be refused")
	}
}

func TestPrepareHistoryCreatesAPrivateStoreAndTheMountTargets(t *testing.T) {
	withHistoryLayout(t, agents.HistoryLayout{Dirs: []string{"projects"}, Appends: []string{"history.jsonl"}})
	cfg := &config.Config{ConfigDir: t.TempDir()}
	repo := t.TempDir()
	store, err := PrepareHistory(cfg, "claude", "work", repo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(store, filepath.Join(cfg.ConfigDir, "claude", "history", "work")+string(filepath.Separator)) {
		t.Fatalf("store %s is not under the account's history root", store)
	}
	for path, wantDir := range map[string]bool{
		filepath.Join(store, "projects"): true, filepath.Join(store, "history.jsonl"): false,
		filepath.Join(cfg.AgentProfileDir("claude", "work"), "projects"): true, filepath.Join(cfg.AgentProfileDir("claude", "work"), "history.jsonl"): false,
	} {
		info, err := os.Lstat(path)
		if err != nil || info.IsDir() != wantDir || info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s: %v, dir=%t, mode %v; want a private %s", path, err, info != nil && info.IsDir(), info.Mode(), map[bool]string{true: "directory", false: "file"}[wantDir])
		}
	}
	if again, err := PrepareHistory(cfg, "claude", "work", repo); err != nil || again != store {
		t.Errorf("preparing again = %q, %v; want the same store", again, err)
	}
	// A store that names another project is refused, never mixed.
	if err := os.WriteFile(filepath.Join(store, storeRecordName), []byte(`{"project":"/elsewhere"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareHistory(cfg, "claude", "work", repo); err == nil || !strings.Contains(err.Error(), "belongs to another repository") {
		t.Errorf("a store claimed by another project was reused: %v", err)
	}
	// A link a box planted in its store must not redirect what the host creates.
	cfg2 := &config.Config{ConfigDir: t.TempDir()}
	store2, err := PrepareHistory(cfg2, "claude", "work", repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(store2, "projects")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(store2, "projects")); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareHistory(cfg2, "claude", "work", repo); err == nil {
		t.Error("a symlinked history directory in the store was accepted")
	}
}

func TestRunMountsHistoryOverlays(t *testing.T) {
	withHistoryLayout(t, agents.HistoryLayout{Dirs: []string{"projects", "plans"}, Appends: []string{"history.jsonl"}})
	run := func(t *testing.T, spec RunSpec, cfg *config.Config) string {
		t.Helper()
		recorder := filepath.Join(t.TempDir(), "runtime-args")
		spec.Image, spec.Cmd, spec.Agent, spec.Homes, spec.Batch, spec.Quiet = "i", []string{"true"}, "claude", true, true, true
		if code, err := Run(cfg, recorderRuntime(t, recorder), spec); err != nil || code != 0 {
			t.Fatalf("Run = (%d, %v)", code, err)
		}
		args, err := os.ReadFile(recorder)
		if err != nil {
			t.Fatal(err)
		}
		return string(args)
	}
	t.Run("a repository box", func(t *testing.T) {
		cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
		repo := t.TempDir()
		args := run(t, RunSpec{Repo: repo}, cfg)
		key, _, err := HistoryKey(repo)
		if err != nil {
			t.Fatal(err)
		}
		store := filepath.Join(cfg.ConfigDir, "claude", "history", config.DefaultProfile, key)
		for _, want := range []string{
			"-v " + cfg.AgentDir("claude") + ":/home/node/.claude",
			"-v " + filepath.Join(store, "projects") + ":/home/node/.claude/projects",
			"-v " + filepath.Join(store, "plans") + ":/home/node/.claude/plans",
			"--mount type=bind,source=" + filepath.Join(store, "history.jsonl") + ",target=/home/node/.claude/history.jsonl",
		} {
			if !strings.Contains(args, want) {
				t.Errorf("args missing %q:\n%s", want, args)
			}
		}
		if strings.Index(args, ":/home/node/.claude ") > strings.Index(args, ":/home/node/.claude/projects") {
			t.Error("a history overlay is mounted before the home it overlays")
		}
	})
	t.Run("an ACP box takes the lead's ACP directories from the ACP store", func(t *testing.T) {
		cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
		args := run(t, RunSpec{Repo: t.TempDir(), ShareACPSessions: true}, cfg)
		if strings.Count(args, ":/home/node/.claude/projects") != 1 || !strings.Contains(args, filepath.Join(acpSharedDir(cfg, "claude"), "projects")+":/home/node/.claude/projects") {
			t.Errorf("projects must be mounted once, from the ACP store:\n%s", args)
		}
		if !strings.Contains(args, ":/home/node/.claude/plans") || !strings.Contains(args, "target=/home/node/.claude/history.jsonl") {
			t.Errorf("the other history paths still come from the repository's store:\n%s", args)
		}
	})
	t.Run("a login box gets empty stand-ins, removed after the run", func(t *testing.T) {
		cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
		args := run(t, RunSpec{Repo: t.TempDir(), Login: true}, cfg)
		var standIn string
		for _, field := range strings.Fields(args) {
			if host, ok := strings.CutSuffix(field, ":/home/node/.claude/projects"); ok {
				standIn = host
			}
		}
		if !strings.Contains(standIn, "coop-history-claude-") {
			t.Fatalf("a login box should mount a stand-in store, got %q:\n%s", standIn, args)
		}
		if _, err := os.Stat(standIn); !os.IsNotExist(err) {
			t.Errorf("the stand-in outlived the run: %v", err)
		}
		if entries, _ := os.ReadDir(filepath.Join(cfg.ConfigDir, "claude", "history")); len(entries) != 0 {
			t.Error("a login created a repository store")
		}
	})
	t.Run("a remote session keeps its private profile as it is", func(t *testing.T) {
		state := t.TempDir()
		cfg := &config.Config{ConfigDir: filepath.Join(state, "acp", "session-1"), HomeInBox: "/home/node", Egress: "none"}
		args := run(t, RunSpec{Repo: t.TempDir(), RunID: "run-1"}, cfg)
		if strings.Contains(args, "/home/node/.claude/projects") || strings.Contains(args, "history.jsonl") {
			t.Errorf("a remote session's profile got history overlays:\n%s", args)
		}
	})
}

func TestHistoryStoresListsOnlyStoreDirectories(t *testing.T) {
	withHistoryLayout(t, agents.HistoryLayout{Dirs: []string{"projects"}})
	cfg := &config.Config{ConfigDir: t.TempDir()}
	store, err := PrepareHistory(cfg, "claude", "work", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := HistoryAccountRoot(cfg, "claude", "work")
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "planted")); err != nil {
		t.Fatal(err)
	}
	if got := HistoryStores(cfg, "claude", "work"); len(got) != 1 || got[0] != store {
		t.Errorf("HistoryStores = %v, want only %s", got, store)
	}
}
