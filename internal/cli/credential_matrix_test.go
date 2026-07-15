package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestCredentialSourcesDriveProviderWorkflows(t *testing.T) {
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		marker, _ := ag.AuthMarker()
		sources := append([]string{"file"}, ag.CredentialEnvKeys()...)
		for _, source := range sources {
			t.Run(name+"/"+source, func(t *testing.T) {
				cfg := &config.Config{ConfigDir: t.TempDir()}
				profile := "work"
				if source == "file" {
					dir := cfg.AgentProfileDir(name, profile)
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, marker), []byte("token"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(cfg.EnvFile(), []byte(source+"=token\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					// Box creates the active directory on first use; directory existence alone must not
					// make the env-backed credential look file-backed in the detail view.
					for _, dir := range []string{profile, "personal"} {
						if err := os.MkdirAll(cfg.AgentProfileDir(name, dir), 0o700); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := cfg.SetDefaultProfile(name, profile); err != nil {
					t.Fatal(err)
				}
				a := &app{cfg: cfg}
				if err := a.selectRunProfile(name, profile); err != nil {
					t.Errorf("direct/Fusion account selection rejected %s via %s: %v", name, source, err)
				}

				out := captureStdout(t, func() {
					if code, err := a.cmdCredentials([]string{name}); code != 0 || err != nil {
						t.Errorf("credentials listing = (%d, %v)", code, err)
					}
				})
				var profileLine string
				for _, line := range strings.Split(out, "\n") {
					if strings.Contains(line, profile) {
						profileLine = line
						break
					}
				}
				if !strings.Contains(profileLine, "signed in") || strings.Contains(profileLine, "not signed in") {
					t.Errorf("credentials listing does not recognize %s via %s:\n%s", name, source, out)
				}
				detail := captureStdout(t, func() {
					if code, err := a.showProfile(name, profile); code != 0 || err != nil {
						t.Errorf("credential detail = (%d, %v)", code, err)
					}
				})
				if !strings.Contains(detail, "signed in") || strings.Contains(detail, "not signed in") {
					t.Errorf("credential detail does not recognize %s via %s:\n%s", name, source, detail)
				}
				if source != "file" && strings.Contains(detail, "  dir") {
					t.Errorf("env-only credential detail claims a profile directory:\n%s", detail)
				}

				peers, err := a.resolvePeers("--peer", []string{name})
				if err != nil || len(peers) != 1 || peers[0].Provider != name {
					t.Errorf("peer resolution = (%+v, %v), want %s", peers, err, name)
				}

				targets, err := expandLadder(cfg, name, nil)
				if err != nil || len(targets) != 1 || targets[0].String() != name+"@"+profile {
					t.Errorf("ladder expansion = (%v, %v), want %s@%s", targets, err, name, profile)
				}

				if got := defaultACPProvider(cfg); got != name {
					t.Errorf("ACP default provider = %q, want %q", got, name)
				}
				if !anyAgentSignedIn(cfg) {
					t.Error("first-run help still reports no signed-in provider")
				}
				if unsigned := unsignedFleetAccounts(cfg, []fleetEntry{{name: "matrix", agent: name + "@" + profile}}); len(unsigned) != 0 {
					t.Errorf("fleet rejected signed-in pinned account: %v", unsigned)
				}

				control := newACPControl(cfg, name, "", "", t.TempDir(), acpSelection{}, nil, nil, false)
				if !slices.Contains(control.creds, profile) {
					t.Errorf("ACP account selector omitted %s@%s via %s: %v", name, profile, source, control.creds)
				}
				if next, recognized := control.selectorSelection(coopAccountID, profile); !recognized || next.Account != profile {
					t.Errorf("ACP account selection rejected %s@%s via %s: (%+v, %v)", name, profile, source, next, recognized)
				}
				if next, recognized := control.selectorSelection(coopAccountID, "ghost"); !recognized || next.Account != "" {
					t.Errorf("ACP account selection accepted unknown %s@ghost: (%+v, %v)", name, next, recognized)
				}
				if cands := a.targetCandidates(name+"@", false, true); !slices.Contains(cands, name+"@"+profile) {
					t.Errorf("completion omitted %s@%s via %s: %v", name, profile, source, cands)
				}

				if source != "file" {
					if got := accountsFor(cfg, name); !slices.Equal(got, []string{profile}) {
						t.Errorf("env credential expanded as multiple accounts: %v, want [%s]", got, profile)
					}
					if box.ProfileAuthed(cfg, name, "personal") {
						t.Errorf("env credential authenticated unrelated %s@personal", name)
					}
					if unsigned := unsignedFleetAccounts(cfg, []fleetEntry{{name: "typo", agent: name + "@personal"}}); len(unsigned) != 1 {
						t.Errorf("fleet accepted unrelated env-backed account: %v", unsigned)
					}
				}
			})
		}
	}
}

func TestACPCredentialSourcesFollowProviderSelection(t *testing.T) {
	names := agents.Names()
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		marker, _ := ag.AuthMarker()
		sources := append([]string{"file"}, ag.CredentialEnvKeys()...)
		for _, source := range sources {
			t.Run(name+"/"+source, func(t *testing.T) {
				cfg := &config.Config{ConfigDir: t.TempDir()}
				base := names[0]
				if base == name {
					base = names[1]
				}
				signInCred(t, cfg, base, "base")
				if source == "file" {
					dir := cfg.AgentProfileDir(name, "work")
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, marker), []byte("token"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(cfg.EnvFile(), []byte(source+"=token\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := cfg.SetDefaultProfile(name, "work"); err != nil {
					t.Fatal(err)
				}

				c := newACPControl(cfg, base, "", "", t.TempDir(), acpSelection{}, nil, nil, false)
				if got := c.spawnableProviders(base); !slices.Contains(got, name) {
					t.Fatalf("ACP provider options omitted %s via %s: %v", name, source, got)
				}
				handled, restart, ids := selectorSet(t, c, coopProviderID, name)
				if !handled || !restart || !slices.Equal(ids, []string{coopPresetID, coopProviderID, coopAccountID}) {
					t.Fatalf("ACP provider selection %s via %s = handled %v restart %v options %v", name, source, handled, restart, ids)
				}
				target, presetName, ok := c.spawnTarget()
				if !ok || presetName != "" || target.Provider != name || target.Account() != "work" {
					t.Errorf("ACP spawn target %s via %s = (%s, preset %q, %v), want %s@work", name, source, target.String(), presetName, ok, name)
				}
				c.mu.Lock()
				creds, accounts := slices.Clone(c.creds), slices.Clone(c.accounts)
				c.mu.Unlock()
				if !slices.Equal(creds, []string{"work"}) || !slices.Equal(accounts, []string{"work"}) {
					t.Errorf("ACP retarget %s via %s = creds %v, accounts %v; want [work] for both", name, source, creds, accounts)
				}
			})
		}
	}
}

func TestUnsignedFleetAccountsRejectsMissingCredential(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	got := unsignedFleetAccounts(cfg, []fleetEntry{{name: "api", agent: "grok@work"}})
	if len(got) != 1 || !strings.Contains(got[0], "api/grok") || !strings.Contains(got[0], "work") {
		t.Fatalf("unsigned fleet accounts = %v, want api/grok work", got)
	}
}
