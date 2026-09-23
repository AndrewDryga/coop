package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestExternalServiceVolumeNeedsTerminalReview(t *testing.T) {
	t.Setenv(box.ServiceStateRootEnv, t.TempDir())
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(repo, ".agent", "compose.yml")
	body := "services:\n  db:\n    image: postgres:18\n    volumes: [customer:/data]\nvolumes:\n  customer:\n    external: true\n    name: customer-data\n"
	if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}, rt: composeShim{services: []string{"db"}}.build(t), rtSet: true}
	var code int
	out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
	if code != 1 || strings.Contains(out, "[Compose output]") || !strings.Contains(out, "terminal") {
		t.Fatalf("nonterminal external-volume start = %d:\n%s", code, out)
	}
	typedAnswers(t, "n")
	out = captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
	if code != 1 || strings.Contains(out, "[Compose output]") || !strings.Contains(out, "customer-data — read/write (db → /data)") {
		t.Fatalf("declined external-volume start = %d:\n%s", code, out)
	}
	typedAnswers(t, "y")
	out = captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
	if code != 0 || !strings.Contains(out, "[Compose output]") || !strings.Contains(out, "Docker-volume access approved") {
		t.Fatalf("reviewed external-volume start = %d:\n%s", code, out)
	}
	// A remembered grant saves a terminal operator the repeated question, but never authorizes a
	// script, agent, or unattended startup to attach this global Docker volume.
	out = captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
	if code != 0 || strings.Contains(out, "Allow these Docker volumes?") {
		t.Fatalf("repeat terminal start = %d:\n%s", code, out)
	}
	initInput = nil
	out = captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
	if code != 1 || strings.Contains(out, "[Compose output]") {
		t.Fatalf("remembered approval leaked into nonterminal start = %d:\n%s", code, out)
	}
}

func TestVolumeApprovalAndSecretDeclineKeepDecoy(t *testing.T) {
	t.Setenv(box.ServiceStateRootEnv, t.TempDir())
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tls.key"), []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(repo, ".agent", "compose.yml")
	body := "services:\n  web:\n    image: nginx:1\n    volumes: [customer:/data, \"../tls.key:/tls.key:ro\"]\nvolumes:\n  customer:\n    external: true\n    name: customer-data\n"
	if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}, rt: composeShim{services: []string{"web"}}.build(t), rtSet: true}
	typedAnswers(t, "y", "n")
	var code int
	out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
	if code != 0 || !strings.Contains(out, "[Compose output]") || !strings.Contains(out, "Services will receive empty files") {
		t.Fatalf("volume approved and secret declined = %d:\n%s", code, out)
	}
	if review, err := box.ReviewServiceSecrets(repo, compose); err != nil || review == nil {
		t.Fatalf("secret was accidentally approved: review=%v err=%v", review, err)
	}
}

func TestVolumeApprovalPromptEscapesRepositoryTerminalControls(t *testing.T) {
	t.Setenv(box.ServiceStateRootEnv, t.TempDir())
	t.Setenv("NO_COLOR", "1")
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(repo, ".agent", "compose.yml")
	body := "services:\n  db:\n    image: postgres:18\n    volumes: [\"customer:/safe\\u001b[2J\\nFAKE\\u202e\"]\nvolumes:\n  customer:\n    external: true\n    name: customer-data\n"
	if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}, rt: composeShim{services: []string{"db"}}.build(t), rtSet: true}
	typedAnswers(t, "n")
	var code int
	out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
	if code != 1 || strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\u202e") ||
		!strings.Contains(out, `\u001B[2J\u000AFAKE\u202E`) {
		t.Fatalf("approval prompt did not render untrusted fields safely (%d):\n%s", code, out)
	}
}
