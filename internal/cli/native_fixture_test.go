package cli

import (
	"context"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/nativeauth"
	"github.com/AndrewDryga/coop/internal/testutil/nativedocker"
	"os"
	"path/filepath"
	"testing"
)

func importNativeFixture(t *testing.T, cfg *config.Config, provider, account string, files map[string][]byte) {
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
	if err := box.ImportNativeSignIn(context.Background(), cfg, provider, account, stage); err != nil {
		t.Fatal(err)
	}
}

func seedNativeFixture(t *testing.T, cfg *config.Config, provider, account string) {
	t.Helper()
	importNativeFixture(t, cfg, provider, account, nativeauth.Files(t, provider, account))
}
func nativeOnlineRuntime(t *testing.T, calls string) runtime.Runtime {
	t.Helper()
	return nativedocker.New(t, calls, filepath.Join(t.TempDir(), "box-env"))
}
func TestNativeOnlineFixtureProcess(t *testing.T) { nativedocker.Process(t) }
