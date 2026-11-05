package box

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestOfflineNativeHomesDoNotLeaseRetiredCredentialProfiles(t *testing.T) {
	for _, name := range agents.Names() {
		t.Run(name, func(t *testing.T) {
			cfg := (&config.Config{ConfigDir: t.TempDir()}).WithNativeHomes(map[string]string{name: t.TempDir()})
			spec := RunSpec{Ctx: t.Context(), Agent: name, Homes: true}
			release, err := runCredentialUseLeases(cfg, runtime.Runtime{Name: "docker"}, &spec)
			if err != nil {
				t.Fatal(err)
			}
			home, err := cfg.NativeHome(name)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			if exclusive, err := credentialUseLease(ctx, cfg, home, true); err == nil {
				exclusive.close()
				t.Error("offline history home has no shared lease")
			}
			cancel()
			release()
			exclusive, err := credentialUseLease(t.Context(), cfg, home, true)
			if err != nil {
				t.Fatal(err)
			}
			exclusive.close()
			if len(spec.ExtraArgs) != 0 {
				t.Fatal("native home inherited a legacy credential lease", spec.ExtraArgs)
			}
		})
	}
}

func TestRunRefusesUnsettledHistoryAndReleasesAllHomes(t *testing.T) {
	for _, test := range []struct {
		name, receipt string
		allowed       bool
	}{
		{"pending", `{"Version":1,"Done":false}`, false}, {"malformed", `{`, false}, {"unsupported", `{"Version":2,"Done":true}`, false}, {"complete", `{"Version":1,"Done":true}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			homes := map[string]string{"codex": filepath.Join(t.TempDir(), "home"), "claude": filepath.Join(t.TempDir(), "home")}
			for _, home := range homes {
				if err := os.MkdirAll(home, 0700); err != nil {
					t.Fatal(err)
				}
			}
			imports := filepath.Join(filepath.Dir(homes["claude"]), "imports")
			if err := os.Mkdir(imports, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(imports, "receipt.json"), []byte(test.receipt), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := (&config.Config{ConfigDir: t.TempDir()}).WithNativeHomes(homes)
			spec := RunSpec{Ctx: t.Context(), Agent: "codex", Homes: true, Peers: []agents.Target{{Provider: "claude"}}}
			release, err := runCredentialUseLeases(cfg, runtime.Runtime{Name: "docker"}, &spec)
			if test.allowed {
				if err != nil || release == nil {
					t.Fatalf("complete history refused: %v", err)
				}
				release()
			} else {
				if release != nil {
					t.Cleanup(release)
				}
				if err == nil || release != nil {
					t.Fatalf("unsettled history admitted: %v", err)
				}
			}
			for provider, home := range homes {
				ctx, cancel := context.WithTimeout(t.Context(), wait.Deadline)
				lease, err := credentialUseLease(ctx, cfg, home, true)
				cancel()
				if err != nil {
					t.Fatalf("%s history lease retained: %v", provider, err)
				}
				lease.close()
			}
		})
	}
}
