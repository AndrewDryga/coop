package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestForkWorkspace(t *testing.T) {
	repo := "/home/me/proj"
	if got, want := forkHome(repo), "/home/me/proj-forks"; got != want {
		t.Errorf("forkHome = %q, want %q", got, want)
	}
	if got, want := forkWorkspace(repo, "perf"), "/home/me/proj-forks/perf"; got != want {
		t.Errorf("forkWorkspace = %q, want %q", got, want)
	}
}

func TestValidForkName(t *testing.T) {
	for _, n := range []string{"perf", "deps", "fix-1", "a.b"} {
		if !validForkName(n) {
			t.Errorf("validForkName(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"", "ls", "review", "merge", "rm", "open", "path", "acp", "a/b", `a\b`, "..", ".", "-x",
		"my fork", "a\tb", "a\nb", "a=b"} { // whitespace / '=' break the git branch + fleet-file round-trip
		if validForkName(n) {
			t.Errorf("validForkName(%q) = true, want false", n)
		}
	}
}

// A typo'd --profile must fail (exit 2) before any image/clone work, so it never leaves a stray
// fork behind. The check runs before resolveImage, so it returns without a runtime.
func TestForkCreateRejectsUnknownProfileBeforeClone(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}} // no profiles signed in
	if code, err := a.forkCreate([]string{"scratchfork", "claude", "--profile", "ghost"}); code != 2 || err == nil {
		t.Fatalf("forkCreate with a bad --profile = (%d, %v), want (2, error before any clone)", code, err)
	}
}

func TestForkAgentMemory(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A fork with no memory yet.
	if got := readForkAgent(ws); got != "" {
		t.Errorf("readForkAgent(fresh) = %q, want empty", got)
	}
	// Persist, read back, and confirm it's git-excluded so it never lands.
	saveForkAgent(ws, "codex")
	if got := readForkAgent(ws); got != "codex" {
		t.Errorf("readForkAgent after save = %q, want codex", got)
	}
	excl, _ := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if !strings.Contains(string(excl), ".coop/") {
		t.Errorf(".git/info/exclude missing .coop/: %q", excl)
	}
	// An explicit switch updates the memory; the exclude isn't duplicated.
	saveForkAgent(ws, "gemini")
	if got := readForkAgent(ws); got != "gemini" {
		t.Errorf("readForkAgent after switch = %q, want gemini", got)
	}
	excl2, _ := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if strings.Count(string(excl2), ".coop/") != 1 {
		t.Errorf("exclude duplicated .coop/: %q", excl2)
	}
	// A garbage value is ignored (not a known agent).
	if err := os.WriteFile(forkAgentFile(ws), []byte("bogus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readForkAgent(ws); got != "" {
		t.Errorf("readForkAgent(bogus) = %q, want empty", got)
	}
}

func TestParseForkCreateAgentSet(t *testing.T) {
	if fa, _ := parseForkCreate([]string{"perf"}); fa.agentSet {
		t.Error("parseForkCreate(perf): agentSet should be false (defaulted)")
	}
	if fa, _ := parseForkCreate([]string{"perf", "codex"}); !fa.agentSet || fa.agent != "codex" {
		t.Errorf("parseForkCreate(perf codex): agentSet=%v agent=%q, want true/codex", fa.agentSet, fa.agent)
	}
}

func TestParseForkCreate(t *testing.T) {
	tests := []struct {
		args      []string
		wantAgent string
		wantFresh bool
		wantErr   bool
	}{
		{[]string{"perf"}, "claude", false, false},
		{[]string{"perf", "codex"}, "codex", false, false},
		{[]string{"perf", "gemini", "--fresh"}, "gemini", true, false},
		{[]string{}, "", false, true},
		{[]string{"perf", "bogus"}, "", false, true},
		{[]string{"ls"}, "", false, true}, // reserved name
	}
	for _, tc := range tests {
		fa, err := parseForkCreate(tc.args)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseForkCreate(%v) err = %v, wantErr %v", tc.args, err, tc.wantErr)
			continue
		}
		if tc.wantErr {
			continue
		}
		if fa.agent != tc.wantAgent || fa.fresh != tc.wantFresh {
			t.Errorf("parseForkCreate(%v) = {agent:%q fresh:%v}, want {agent:%q fresh:%v}",
				tc.args, fa.agent, fa.fresh, tc.wantAgent, tc.wantFresh)
		}
	}
}

func TestForkRmSafe(t *testing.T) {
	tests := []struct {
		unmerged, dirty, force bool
		wantErr                bool
	}{
		{false, false, false, false}, // clean & merged → ok
		{true, false, false, true},   // unmerged → blocked
		{false, true, false, true},   // dirty → blocked
		{true, true, true, false},    // force overrides everything
	}
	for _, tc := range tests {
		err := forkRmSafe(tc.unmerged, tc.dirty, tc.force)
		if (err != nil) != tc.wantErr {
			t.Errorf("forkRmSafe(unmerged=%v dirty=%v force=%v) err = %v, wantErr %v",
				tc.unmerged, tc.dirty, tc.force, err, tc.wantErr)
		}
	}
}

func TestParseShortstat(t *testing.T) {
	ins, del := parseShortstat(" 3 files changed, 42 insertions(+), 7 deletions(-)")
	if ins != 42 || del != 7 {
		t.Errorf("parseShortstat = (%d, %d), want (42, 7)", ins, del)
	}
	if ins, del := parseShortstat(""); ins != 0 || del != 0 {
		t.Errorf("parseShortstat(empty) = (%d, %d), want (0, 0)", ins, del)
	}
}

func TestIndentLastLines(t *testing.T) {
	if got := indent("a\nb"); got != "  a\n  b" {
		t.Errorf("indent = %q, want %q", got, "  a\n  b")
	}
	if got := lastLines("a\nb\nc\nd", 2); got != "c\nd" {
		t.Errorf("lastLines(.., 2) = %q, want %q", got, "c\nd")
	}
	if got := lastLines("a\nb", 5); got != "a\nb" {
		t.Errorf("lastLines short = %q, want %q", got, "a\nb")
	}
}

// --- git-backed lifecycle ---

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	git(t, repo, "checkout", "-q", "-b", "main")
	git(t, repo, "config", "user.email", "t@t") // so merge-commits work without ambient identity
	git(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "init")
	return repo
}

func TestForkLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)

	// setupFork clones + branches.
	ws, err := setupFork(repo, "perf")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	if !pathExists(ws) {
		t.Fatalf("workspace %s not created", ws)
	}
	if got := gitBranch(ws); got != "perf" {
		t.Errorf("fork branch = %q, want %q", got, "perf")
	}
	// The fork must carry the parent's git identity so an agent can commit in it.
	if got := gitOut(ws, "config", "user.email"); got != "t@t" {
		t.Errorf("fork git identity not propagated: user.email = %q, want %q", got, "t@t")
	}

	// A commit in the fork is "unmerged" from the parent's point of view.
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "do the work")
	if !forkUnmerged(repo, ws) {
		t.Error("fork with new commit should be unmerged")
	}

	// review fetches the branch into review/perf.
	if err := gitFetchInto(repo, ws, "perf"); err != nil {
		t.Fatalf("gitFetchInto: %v", err)
	}
	if gitOut(repo, "rev-parse", "--verify", "-q", "review/perf") == "" {
		t.Error("review/perf ref not created")
	}

	// merge lands it; now it's merged.
	git(t, repo, "merge", "--no-edit", "review/perf")
	if forkUnmerged(repo, ws) {
		t.Error("fork should be merged after git merge")
	}
	if !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("merged file not present in parent repo")
	}

	// destroyFork removes the workspace and the review ref.
	if err := destroyFork(repo, "perf"); err != nil {
		t.Fatalf("destroyFork: %v", err)
	}
	if pathExists(ws) {
		t.Error("workspace not removed")
	}
	if gitOut(repo, "rev-parse", "--verify", "-q", "review/perf") != "" {
		t.Error("review/perf ref not removed")
	}
}

func TestForkCarriesGlobalIgnore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo := initRepo(t)
	// Your real GLOBAL gitignore is carried into the fork; a repo-local core.excludesfile is
	// IGNORED — it's agent-writable, so reading it would let a poisoned repo point us at a host
	// secret (e.g. ~/.ssh/id_rsa) and copy its content into the fork. (`--global` ignores -C.)
	ignore := filepath.Join(t.TempDir(), "ignore")
	if err := os.WriteFile(ignore, []byte("*.tmp\n.DS_Store\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "--global", "core.excludesfile", ignore)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("SECRET_TOKEN_xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "core.excludesfile", secret) // repo-local poison: must NOT be read

	ws, err := setupFork(repo, "ig")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	excl, err := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(excl), "*.tmp") || !strings.Contains(string(excl), ".DS_Store") {
		t.Errorf("global ignore not carried into the fork's .git/info/exclude:\n%s", excl)
	}
	if strings.Contains(string(excl), "SECRET_TOKEN") {
		t.Fatalf("a repo-local core.excludesfile (a host secret) was read and copied into the fork:\n%s", excl)
	}
}

func TestForkCarriesSigningMaterials(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	git(t, repo, "config", "gpg.format", "ssh")
	git(t, repo, "config", "user.signingkey", "/home/me/.ssh/id_ed25519.pub")
	git(t, repo, "config", "commit.gpgsign", "true")

	ws, err := setupFork(repo, "s")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	// The key + format travel so the rebase-on-land can sign.
	if got := gitOut(ws, "config", "--get", "gpg.format"); got != "ssh" {
		t.Errorf("gpg.format not propagated: %q", got)
	}
	if gitOut(ws, "config", "--get", "user.signingkey") == "" {
		t.Error("user.signingkey not propagated")
	}
	// commit.gpgsign must NOT be in the fork's local config, or the keyless box would
	// try to sign and every agent commit would fail.
	if got := gitOut(ws, "config", "--local", "--get", "commit.gpgsign"); got != "" {
		t.Errorf("commit.gpgsign must not be copied to the fork's local config, got %q", got)
	}
}

func TestHelpRequested(t *testing.T) {
	if !helpRequested([]string{"review", "demo", "--help"}) || !helpRequested([]string{"-h"}) {
		t.Error("helpRequested should detect -h/--help anywhere in args")
	}
	if helpRequested([]string{"demo", "codex"}) {
		t.Error("helpRequested should be false with no help flag")
	}
}

func TestForkHelp(t *testing.T) {
	if code, err := forkHelp(); code != 0 || err != nil {
		t.Errorf("forkHelp = (%d, %v), want (0, nil)", code, err)
	}
}

func TestParseForkContinue(t *testing.T) {
	if fa, err := parseForkCreate([]string{"demo", "-c"}); err != nil || !fa.cont {
		t.Errorf("parseForkCreate(demo -c) = {cont:%v}, err=%v; want cont", fa.cont, err)
	}
	if fa, err := parseForkCreate([]string{"demo", "codex", "--continue"}); err != nil || !fa.cont || fa.agent != "codex" {
		t.Errorf("parseForkCreate(demo codex --continue) = {cont:%v agent:%q}, err=%v", fa.cont, fa.agent, err)
	}
}

func TestForkLaunchCmd(t *testing.T) {
	cfgDir := t.TempDir()
	ws := filepath.Join(t.TempDir(), "myrepo-forks", "demo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{ConfigDir: cfgDir}}

	// First launch of a preset agent (claude): start under a fresh coop-owned id and
	// persist it; the command carries --session-id <uuid>.
	cmd := a.forkLaunchCmd(forkArgs{name: "demo", agent: "claude"}, ws, false)
	id := readForkSession(ws, "claude")
	if id == "" {
		t.Fatal("first launch did not persist a session id")
	}
	if !slices.Contains(cmd, "--session-id") || !slices.Contains(cmd, id) {
		t.Errorf("first launch cmd = %v, want --session-id %s", cmd, id)
	}

	// Re-entry once the session exists: resume exactly that id (not --continue/latest).
	claudeKey := strings.ReplaceAll(ws, "/", "-")
	sess := filepath.Join(cfgDir, "claude", "projects", claudeKey, id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sess), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sess, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = a.forkLaunchCmd(forkArgs{name: "demo", agent: "claude"}, ws, true)
	if !slices.Contains(cmd, "--resume") || !slices.Contains(cmd, id) {
		t.Errorf("re-entry cmd = %v, want --resume %s", cmd, id)
	}
	if readForkSession(ws, "claude") != id {
		t.Error("session id changed on re-entry")
	}

	// Re-entry when the session id was persisted but never materialized (e.g. quit
	// before a turn): fall back to a fresh start under the SAME id, not a resume error.
	ws2 := filepath.Join(t.TempDir(), "myrepo-forks", "ghost")
	if err := os.MkdirAll(ws2, 0o755); err != nil {
		t.Fatal(err)
	}
	saveForkSession(ws2, "claude", id)
	cmd = a.forkLaunchCmd(forkArgs{name: "ghost", agent: "claude"}, ws2, true)
	if !slices.Contains(cmd, "--session-id") || slices.Contains(cmd, "--resume") {
		t.Errorf("ghost re-entry cmd = %v, want a fresh --session-id (no live session)", cmd)
	}

	// codex can't preset an id: no session file, and a fresh start is plain Interactive.
	cmd = a.forkLaunchCmd(forkArgs{name: "demo", agent: "codex"}, ws, false)
	if readForkSession(ws, "codex") != "" {
		t.Error("codex must not get a coop-owned session id")
	}
	if slices.Contains(cmd, "--session-id") {
		t.Errorf("codex launch must not preset a session id: %v", cmd)
	}
}

func TestDetectEditor(t *testing.T) {
	// With no GUI editor reachable on PATH, fall back to $VISUAL, then $EDITOR.
	t.Setenv("PATH", t.TempDir()) // empty dir → none of code/cursor/zed/idea/subl found
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "myeditor")
	if got := detectEditor(); got != "myeditor" {
		t.Errorf("detectEditor() = %q, want %q ($EDITOR fallback)", got, "myeditor")
	}
	t.Setenv("VISUAL", "myvisual")
	if got := detectEditor(); got != "myvisual" {
		t.Errorf("detectEditor() = %q, want %q ($VISUAL beats $EDITOR)", got, "myvisual")
	}
}

func TestResolveEditor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Isolate from the host's global/system git config so core.editor is only what we set.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo := initRepo(t)
	// Your GLOBAL core.editor is honored; a repo-local one is IGNORED — the repo is agent-writable,
	// so reading core.editor from it would let a poisoned repo point the editor at a planted binary
	// that runs on `coop fork review --open`. (`git config --global` ignores -C, writing the env file.)
	git(t, repo, "config", "--global", "core.editor", "zed --wait")
	git(t, repo, "config", "core.editor", "/tmp/evil --pwn") // repo-local: must NEVER be used

	// $COOP_EDITOR wins over everything.
	if got := resolveEditor("nvim"); got != "nvim" {
		t.Errorf("resolveEditor(COOP_EDITOR) = %q, want %q", got, "nvim")
	}
	// With no $COOP_EDITOR, the GLOBAL core.editor is honored — never the repo-local one.
	if got := resolveEditor(""); got != "zed --wait" {
		t.Errorf("resolveEditor = %q, want %q (must ignore the repo-local editor)", got, "zed --wait")
	}
	// With neither set, fall through to detection ($VISUAL; PATH has no GUI editor).
	git(t, repo, "config", "--global", "--unset", "core.editor")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("EDITOR", "")
	t.Setenv("VISUAL", "myvisual")
	if got := resolveEditor(""); got != "myvisual" {
		t.Errorf("resolveEditor(fallback) = %q, want %q", got, "myvisual")
	}
}

func TestRunReviewCmd(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	// COOP_REVIEW_CMD runs via sh -c and must see the fork path/name/ref in env.
	a := &app{cfg: &config.Config{ReviewCmd: `printf '%s|%s' "$COOP_FORK_NAME" "$COOP_FORK_PATH" > ` + out}}
	if code, err := a.runReviewCmd(dir, "/the/fork", "demo", "review/demo"); err != nil || code != 0 {
		t.Fatalf("runReviewCmd = (%d, %v), want (0, nil)", code, err)
	}
	if data, _ := os.ReadFile(out); string(data) != "demo|/the/fork" {
		t.Errorf("COOP_REVIEW_CMD env not passed: got %q, want %q", data, "demo|/the/fork")
	}
}

// `coop fork logs <unknown>` must error like `fork path`/`fork review`, not exit 0 silently.
func TestForkLogsUnknownErrors(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkLogs([]string{"nope"})
	if err == nil || code == 0 {
		t.Fatalf("forkLogs(unknown) = (%d, %v), want a no-such-fork error", code, err)
	}
	if !strings.Contains(err.Error(), "no such fork") {
		t.Errorf("error = %q, want it to mention 'no such fork'", err)
	}
}
