package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitServicesRefusesComposeSymlinks(t *testing.T) {
	hermeticGit(t)
	for _, kind := range []string{"leaf", "parent", "absent child"} {
		t.Run(kind, func(t *testing.T) {
			repo := initializedProject(t)
			outside := t.TempDir()
			protected := filepath.Join(outside, "dev-compose.yml")
			const original = "# outside synthetic file\nservices: {}\n"
			if kind != "absent child" {
				if err := os.WriteFile(protected, []byte(original), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			parent := filepath.Join(repo, "infra")
			if kind == "leaf" {
				if err := os.Mkdir(parent, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(protected, filepath.Join(parent, "dev-compose.yml")); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(outside, parent); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("box:\n  compose: infra/dev-compose.yml\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			a, _ := initApp(t, repo)
			var code int
			out := captureTerminal(t, func() { code = reportExit(a.cmdInit([]string{"--services", "postgres"})) })
			if code != 1 {
				t.Fatalf("unsafe Compose exited %d:\n%s", code, out)
			}
			for _, want := range []string{"Could not add services to infra/dev-compose.yml", "No services were changed."} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "Service added") || strings.Contains(out, "Starting services") {
				t.Errorf("refusal claims success or startup:\n%s", out)
			}
			if kind == "absent child" {
				if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
					t.Fatalf("outside directory changed: %v, %v", entries, err)
				}
			} else if got, err := os.ReadFile(protected); err != nil || string(got) != original {
				t.Fatalf("outside target changed: %q, %v", got, err)
			}
		})
	}
}

// A box can write box.compose, so it can point coop init --services at a file hidden from it. The
// refusal names the file and sends the person to box.compose, not to the hidden file.
func TestInitServicesRefusesHiddenCompose(t *testing.T) {
	hermeticGit(t)
	repo := initializedProject(t)
	secret := filepath.Join(repo, "config", "database.yml")
	if err := os.Mkdir(filepath.Dir(secret), 0o755); err != nil {
		t.Fatal(err)
	}
	const body = "production:\n  password: hunter2\n"
	if err := os.WriteFile(secret, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("box:\n  compose: config/database.yml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, _ := initApp(t, repo)
	var code int
	out := captureTerminal(t, func() { code = reportExit(a.cmdInit([]string{"--services", "postgres"})) })
	if code != 1 {
		t.Fatalf("hidden Compose exited %d:\n%s", code, out)
	}
	for _, want := range []string{
		"Could not add services to config/database.yml",
		"Coop will not edit config/database.yml because it is hidden from the box.",
		"Point box.compose in .agent/project.yaml at a file the box can see.",
		"No services were changed.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("refusal printed the hidden file:\n%s", out)
	}
	if entries, err := os.ReadDir(filepath.Dir(secret)); err != nil || len(entries) != 1 {
		t.Fatalf("config holds %v after the refusal, %v", entries, err)
	}
	if got, err := os.ReadFile(secret); err != nil || string(got) != body {
		t.Fatalf("hidden file changed: %q, %v", got, err)
	}
}
