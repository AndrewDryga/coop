package box

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/nativeauth"
)

func canonicalFixtureJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func canonicalFixtureFiles(t *testing.T, provider, account string) map[string][]byte {
	return nativeauth.Files(t, provider, account)
}

func importCanonicalFixture(t *testing.T, cfg *config.Config, provider, account string, files map[string][]byte) {
	t.Helper()
	stage := t.TempDir()
	if err := os.Chmod(stage, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(stage, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := ImportNativeSignIn(t.Context(), cfg, provider, account, stage); err != nil {
		t.Fatal(err)
	}
}

func seedCanonicalFixture(t *testing.T, cfg *config.Config, provider, account string) {
	t.Helper()
	importCanonicalFixture(t, cfg, provider, account, canonicalFixtureFiles(t, provider, account))
}

func scopedFixtureHome(t *testing.T, cfg *config.Config, provider, repo string, acp bool) string {
	t.Helper()
	home, err := PrepareNativeHome(t.Context(), cfg, runtime.Runtime{}, provider, cfg.ActiveProfile(provider), repo, acp)
	if err != nil {
		t.Fatal(err)
	}
	return home
}
