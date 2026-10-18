package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
)

// On a disk that ignores letter case, coop up refuses services that bind a hidden file whose name
// is too long to cover under every spelling, as a box launch does, unless the owner accepted that
// risk. On a disk that keeps case the name has no other spelling, so the services start.
func TestUpRefusesAHiddenFileServicesCouldReadUnderAnotherCase(t *testing.T) {
	t.Setenv(box.ServiceStateRootEnv, t.TempDir())
	repo := t.TempDir()
	for rel, body := range map[string]string{
		".agent/compose.yml":      "services:\n  web:\n    image: nginx:1\n    volumes: [\"../:/app:ro\"]\n",
		"config/credentials.json": `{"token":"inert"}`,
	} {
		path := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.Lstat(filepath.Join(repo, "config", "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.Lstat(filepath.Join(repo, "config", "CREDENTIALS.JSON"))
	folds := err == nil && os.SameFile(want, other)

	a := secretServiceApp(t, repo)
	var code int
	out := captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
	refused := strings.Contains(out, "Coop cannot hide config/credentials.json from your services") &&
		strings.Contains(out, "COOP_ALLOW_CASE_EXPOSED_SECRETS=1")
	if refused != folds || (code == 1) != folds {
		t.Fatalf("coop up = %d (folds case: %v):\n%s", code, folds, out)
	}
	if !folds {
		return
	}
	a.cfg.AllowCaseExposedSecrets = true
	out = captureTerminal(t, func() { code, _ = a.cmdUp(nil) })
	if code != 0 || !strings.Contains(out, "config/credentials.json is readable by your services under another letter case") {
		t.Fatalf("coop up with the setting = %d:\n%s", code, out)
	}
}
