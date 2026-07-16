package box

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestBoxCommitTrailer(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	// A raw run (no agent) makes no attributed commits → no trailer.
	if got := boxCommitTrailer(cfg, RunSpec{}); got != "" {
		t.Errorf("no-agent run should have no trailer, got %q", got)
	}
	// Provider + resolved model + account → the full attribution.
	cfg.SetActiveModel("codex", "gpt-5.6-luna")
	cfg.SetActiveProfile("codex", "personal")
	got := boxCommitTrailer(cfg, RunSpec{Agent: "codex"})
	if want := "coop (codex:gpt-5.6-luna@personal) <noreply@coop.dev>"; got != want {
		t.Errorf("trailer = %q, want %q", got, want)
	}
	// A fusion governor is the committing agent, not spec.Agent.
	if got := boxCommitTrailer(cfg, RunSpec{Agent: "claude", FusionGovernor: "codex"}); !strings.Contains(got, "(codex:") {
		t.Errorf("fusion trailer should attribute the governor, got %q", got)
	}
}

func TestPrepareCommitMsgHook(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir, err := gitHookDir()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	repo := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "noglobal"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "nosystem"))
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Box")
	git("config", "user.email", "box@example.com")
	git("config", "core.hooksPath", dir)
	git("config", "coop.trailer", "coop (codex:gpt-5.6-luna@personal) <noreply@coop.dev>")
	commit := func(args ...string) string {
		if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte(strings.Join(args, "|")), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git(append([]string{"commit", "-q"}, args...)...)
		cmd := exec.Command("git", "log", "-1", "--format=%B")
		cmd.Dir, cmd.Env = repo, env
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}

	// (1) The agent's machine co-author line is REPLACED by coop's.
	msg := commit("-m", "did work\n\nCo-Authored-By: Claude <noreply@anthropic.com>")
	if strings.Contains(msg, "noreply@anthropic.com") {
		t.Errorf("agent co-author not stripped:\n%s", msg)
	}
	if !strings.Contains(msg, "coop (codex:gpt-5.6-luna@personal)") {
		t.Errorf("coop trailer missing:\n%s", msg)
	}
	// (2) A HUMAN co-author survives alongside coop's.
	msg = commit("-m", "pair work\n\nCo-Authored-By: Jane Dev <jane@example.com>")
	if !strings.Contains(msg, "jane@example.com") {
		t.Errorf("human co-author wrongly stripped:\n%s", msg)
	}
	if !strings.Contains(msg, "coop (codex:") {
		t.Errorf("coop trailer missing when a human co-author is present:\n%s", msg)
	}
	// (3) A message with no trailer gains exactly one coop line, and --amend keeps it to one.
	msg = commit("-m", "plain work")
	if n := strings.Count(msg, "coop (codex:"); n != 1 {
		t.Errorf("want exactly one coop trailer, got %d:\n%s", n, msg)
	}
	git("commit", "-q", "--amend", "--no-edit")
	cmd := exec.Command("git", "log", "-1", "--format=%B")
	cmd.Dir, cmd.Env = repo, env
	out, _ := cmd.Output()
	if n := strings.Count(string(out), "coop (codex:"); n != 1 {
		t.Errorf("amend should stay idempotent (one coop trailer), got %d:\n%s", n, out)
	}
	// (4) A merge/squash message source is left untouched.
	mf := filepath.Join(t.TempDir(), "MERGE_MSG")
	if err := os.WriteFile(mf, []byte("Merge branch 'x'\n\nCo-Authored-By: Claude <noreply@anthropic.com>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hook := exec.Command("sh", filepath.Join(dir, "prepare-commit-msg"), mf, "merge")
	hook.Dir, hook.Env = repo, env
	if out, err := hook.CombinedOutput(); err != nil {
		t.Fatalf("hook on a merge source errored: %v\n%s", err, out)
	}
	if data, _ := os.ReadFile(mf); !strings.Contains(string(data), "noreply@anthropic.com") || strings.Contains(string(data), "coop (") {
		t.Errorf("merge message must be left untouched:\n%s", data)
	}
}

func TestBuildGitConfig(t *testing.T) {
	// Signing is always disabled — the box holds no GPG/SSH key.
	if !strings.Contains(buildGitConfig("", ""), "gpgsign = false") {
		t.Error("buildGitConfig must always disable gpgsign")
	}
	// Identity is included when present.
	gc := buildGitConfig("Ada Lovelace", "ada@example.com")
	if !strings.Contains(gc, "name = Ada Lovelace") || !strings.Contains(gc, "email = ada@example.com") {
		t.Errorf("buildGitConfig identity missing:\n%s", gc)
	}
	// No [user] block when there is no identity to write.
	if strings.Contains(buildGitConfig("", ""), "[user]") {
		t.Error("buildGitConfig should omit [user] when no identity is set")
	}
}

func TestGitConfigForBoxUsesDirectHomeMounts(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "system"))
	gc := gitConfigForBox(
		"coop (claude) <noreply@coop.dev>",
		"/home/node/"+boxGitHooksName,
		"/home/node/"+boxGitIgnoreName,
	)
	if strings.Count(gc, "[core]\n") != 1 {
		t.Fatalf("git config must have one core block:\n%s", gc)
	}
	for _, want := range []string{
		"hooksPath = /home/node/.coop-git-hooks",
		"excludesFile = /home/node/.coop-gitignore",
		"trailer = coop (claude) <noreply@coop.dev>",
	} {
		if !strings.Contains(gc, want) {
			t.Errorf("git config missing %q:\n%s", want, gc)
		}
	}
}

func TestGitConfigForBoxPreservesGlobalIgnoreBehavior(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "empty-global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "empty-system"))
	ignore := filepath.Join(t.TempDir(), boxGitIgnoreName)
	if err := os.WriteFile(ignore, []byte("coop-generated.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte(gitConfigForBox("", "", ignore)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "coop-generated.txt"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("check-ignore", "--quiet", "coop-generated.txt")
}
