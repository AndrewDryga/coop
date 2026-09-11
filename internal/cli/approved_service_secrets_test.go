package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
)

// The approved `coop up` transcripts for the one question a start can ask: a service wants to read
// a file that looks like a secret. Declining is not cancellation — the services start with empty
// files, which is what they would have got had nobody asked.

// secretServiceProject is a project whose web service binds the named key files, with `requested`
// listed in project.yaml as files the repo genuinely asks for. The approval store is redirected to
// this test's own directory, so nothing here reads or writes the developer's real approvals.
func secretServiceProject(t *testing.T, binds []string, requested []string) string {
	t.Helper()
	t.Setenv(box.ServiceStateRootEnv, t.TempDir())
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  web:\n    image: nginx:1\n    volumes:\n"
	for _, b := range binds {
		compose += "      - \"../" + b + ":/run/" + filepath.Base(b) + ":ro\"\n"
		key := "-----BEGIN PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Qu\n-----END PRIVATE KEY-----\n"
		if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(b)), []byte(key), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	project := "services:\n  require_real_files:\n"
	for _, p := range requested {
		project += "    - " + p + "\n"
	}
	if len(requested) == 0 {
		project = "services: {}\n"
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte(project), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func secretServiceApp(t *testing.T, repo string) *app {
	t.Helper()
	shim := composeShim{services: []string{"web"}}
	return &app{cfg: &config.Config{RepoOverride: repo}, rt: shim.build(t), rtSet: true}
}

func TestApprovedServiceSecretApproval(t *testing.T) {
	// Two files, and the prompt says which the repo asked for and which is new since the last
	// approval — the two facts that decide whether a reader should say yes.
	t.Run("28i-secret-approval-accepted", func(t *testing.T) {
		repo := secretServiceProject(t, []string{"certs/dev.key"}, []string{"certs/dev.key"})
		a := secretServiceApp(t, repo)
		typedAnswers(t, "y")
		if code, _ := a.cmdUp(nil); code != 0 {
			t.Fatalf("first approval = %d", code)
		}
		// A second key arrives. The compose content changed, so the whole set needs approving
		// again — and only the newcomer is marked as new.
		compose := filepath.Join(repo, ".agent", "compose.yml")
		body, err := os.ReadFile(compose)
		if err != nil {
			t.Fatal(err)
		}
		key := "-----BEGIN PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Qu\n-----END PRIVATE KEY-----\n"
		if err := os.WriteFile(filepath.Join(repo, "certs", "extra.key"), []byte(key), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(compose, append(body, []byte("      - \"../certs/extra.key:/run/extra.key:ro\"\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		typedAnswers(t, "y")
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
		if code != 0 {
			t.Fatalf("second approval = %d:\n%s", code, out)
		}
		assertApprovedSession(t, "28i-secret-approval-accepted", out)
	})

	// No is an answer, not a cancellation: the services start, and they get empty files.
	t.Run("28j-secret-approval-declined", func(t *testing.T) {
		repo := secretServiceProject(t, []string{"certs/dev.key"}, []string{"certs/dev.key"})
		a := secretServiceApp(t, repo)
		typedAnswers(t, "n")
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
		if code != 0 {
			t.Fatalf("a declined approval exited %d:\n%s", code, out)
		}
		assertApprovedSession(t, "28j-secret-approval-declined", out)
	})

	// Nobody can answer without a terminal, so coop says which files stay empty and how to
	// approve them, and starts anyway — the same decision a box auto-start makes.
	t.Run("28k-secret-files-nonterminal", func(t *testing.T) {
		repo := secretServiceProject(t, []string{"certs/dev.key"}, []string{"certs/dev.key"})
		a := secretServiceApp(t, repo)
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
		if code != 0 {
			t.Fatalf("a nonterminal start exited %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "28k-secret-files-nonterminal", out)
	})

	// An approval coop cannot record is a stop, not a silent start: the next run would ask again
	// while the services had already read the file.
	t.Run("28l-secret-approval-write-failed", func(t *testing.T) {
		repo := secretServiceProject(t, []string{"certs/dev.key"}, []string{"certs/dev.key"})
		blocked := filepath.Join(t.TempDir(), "state")
		if err := os.MkdirAll(blocked, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
		t.Setenv(box.ServiceStateRootEnv, blocked)
		a := secretServiceApp(t, repo)
		typedAnswers(t, "y")
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
		if code != 1 {
			t.Errorf("an unrecordable approval exited %d, want 1", code)
		}
		assertApprovedSession(t, "28l-secret-approval-write-failed", out)
	})

	// The same compose content, already approved: no question, and no warning either.
	t.Run("28m-secret-approval-unchanged", func(t *testing.T) {
		repo := secretServiceProject(t, []string{"certs/dev.key"}, []string{"certs/dev.key"})
		a := secretServiceApp(t, repo)
		typedAnswers(t, "y")
		if code, _ := a.cmdUp(nil); code != 0 {
			t.Fatalf("first approval failed")
		}
		var code int
		out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
		if code != 0 {
			t.Fatalf("second start = %d:\n%s", code, out)
		}
		assertApprovedOutput(t, "28m-secret-approval-unchanged", out)
	})
}
