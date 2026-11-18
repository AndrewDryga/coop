package box

import (
	"os"
	"path/filepath"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestOfflineNativeHomesAdmitAFreshHome(t *testing.T) {
	for _, name := range agents.Names() {
		t.Run(name, func(t *testing.T) {
			cfg := (&config.Config{ConfigDir: t.TempDir()}).WithNativeHomes(map[string]string{name: t.TempDir()})
			spec := RunSpec{Ctx: t.Context(), Agent: name, Homes: true}
			if err := checkRunHistorySettled(cfg, spec); err != nil {
				t.Fatal(err)
			}
			if len(spec.ExtraArgs) != 0 {
				t.Fatal("native home inherited a legacy credential argument", spec.ExtraArgs)
			}
		})
	}
}

func TestRunRefusesUnsettledHistory(t *testing.T) {
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
			err := checkRunHistorySettled(cfg, spec)
			if test.allowed && err != nil {
				t.Fatalf("complete history refused: %v", err)
			}
			if !test.allowed && err == nil {
				t.Fatal("unsettled history admitted")
			}
		})
	}
}
