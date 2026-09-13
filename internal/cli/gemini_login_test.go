package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestGeminiHostLoginStoresWithoutEchoOrNativeCredentialMutation(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	profile := "personal"
	profileDir := cfg.AgentProfileDir("gemini", profile)
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(profileDir, "gemini-credentials.json")
	if err := os.WriteFile(native, []byte("existing-corrupt-native-store"), 0o600); err != nil {
		t.Fatal(err)
	}
	gemini, _ := agents.Get("gemini")
	old := readLoginSecret
	readLoginSecret = func(prompt string) ([]byte, error) {
		if prompt != "Gemini API key (input hidden): " {
			t.Fatalf("prompt = %q", prompt)
		}
		return []byte("not-printed-secret"), nil
	}
	defer func() { readLoginSecret = old }()

	out := captureStderr(t, func() {
		if code, err := (&app{cfg: cfg}).loginWithHostCredential(gemini, profile); code != 0 || err != nil {
			t.Fatalf("host login = (%d, %v)", code, err)
		}
	})
	if strings.Contains(out, "not-printed-secret") || !strings.Contains(out, "aistudio.google.com/apikey") ||
		!strings.Contains(out, "Signed in to Gemini as personal") {
		t.Fatalf("Gemini login output leaked or omitted guidance:\n%s", out)
	}
	if data, _ := os.ReadFile(native); string(data) != "existing-corrupt-native-store" {
		t.Fatalf("native credential changed: %q", data)
	}
	key, value, found, err := box.LoadHostCredential(cfg, gemini, profile)
	if err != nil || !found || key != "GEMINI_API_KEY" || value != "not-printed-secret" {
		t.Fatalf("stored Gemini credential = (%q, %q, %v, %v)", key, value, found, err)
	}
}

func TestGeminiHostLoginDoesNotSaveOverMalformedSettings(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	profile := "work"
	profileDir := cfg.AgentProfileDir("gemini", profile)
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(profileDir, "settings.json")
	if err := os.WriteFile(settings, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	gemini, _ := agents.Get("gemini")
	old := readLoginSecret
	readLoginSecret = func(string) ([]byte, error) { return []byte("must-not-land"), nil }
	defer func() { readLoginSecret = old }()
	if code, err := (&app{cfg: cfg}).loginWithHostCredential(gemini, profile); code != -1 || err == nil {
		t.Fatalf("malformed settings login = (%d, %v)", code, err)
	}
	if _, _, found, _ := box.LoadHostCredential(cfg, gemini, profile); found {
		t.Fatal("API key landed after settings activation failed")
	}
	if data, _ := os.ReadFile(settings); string(data) != "{" {
		t.Fatalf("malformed settings changed: %q", data)
	}
}
