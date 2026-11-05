package liveprovider

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/testutil/nativeauth"
)

func TestPrepareCanonicalAccountsNeverCopiesRenewalAuthority(t *testing.T) {
	for _, provider := range agents.Names() {
		t.Run(provider, func(t *testing.T) {
			source := &config.Config{ConfigDir: t.TempDir()}
			home := t.TempDir()
			if err := os.Chmod(home, 0700); err != nil {
				t.Fatal(err)
			}
			for name, data := range nativeauth.Files(t, provider, "work") {
				writeSource(t, filepath.Join(home, name), string(data), 0600)
			}
			if err := box.ImportNativeSignIn(t.Context(), source, provider, "work", home); err != nil {
				t.Fatal(err)
			}
			prepared, err := Prepare(source.ConfigDir, filepath.Join(t.TempDir(), "isolated"), []Selection{{Provider: provider, Account: "work"}})
			if err != nil {
				t.Fatal(err)
			}
			if !prepared.CredentialPresent(provider, "work") || !prepared.SafeThrough(provider, "work", time.Now().Add(time.Hour)) {
				t.Fatal("canonical access was not usable")
			}
			copied, found, err := box.SnapshotNativeAccess(t.Context(), &config.Config{ConfigDir: prepared.ConfigDir}, provider, "work")
			if err != nil || !found || !copied.Ready {
				t.Fatalf("isolated canonical account: %v, %v", found, err)
			}
			ag, _ := agents.Get(provider)
			state, err := ag.NativeCredentials().Inspect(copied.Files, time.Now())
			if err != nil || state.Refreshable {
				t.Fatalf("isolated account retains renewal authority: %v", err)
			}
			if err := prepared.VerifySources(); err != nil {
				t.Fatal(err)
			}
			writeSource(t, filepath.Join(source.ConfigDir, provider, "credentials", "work", "renewal.json"), `{"intent":"inert"}`, 0600)
			if err := prepared.VerifySources(); err == nil {
				t.Fatal("renewal intent was not part of source integrity")
			}
		})
	}
}
