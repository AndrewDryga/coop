package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/acpctl"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkctl"
)

// selectorSet drives one coop-owned dropdown set through the FromEditor hook — the same wire path
// the proxy uses — and reports what the toolbar ack shows. It cannot call acpctl's unexported
// fromEditor directly (a different package), so it goes through the exported Hooks().FromEditor,
// same shape as production (internal/cli/acp_cmd.go's cmdACPSupervise wires the identical hook).
func selectorSet(t *testing.T, c *acpctl.Control, configID, value string) (handled, restart bool, ids []string) {
	t.Helper()
	line := []byte(`{"jsonrpc":"2.0","id":"selector","method":"session/set_config_option","params":{"sessionId":"s","configId":"` + configID + `","value":"` + value + `"}}` + "\n")
	handled, ack, _, restart := c.Hooks().FromEditor(line)
	var m, res map[string]json.RawMessage
	json.Unmarshal(ack, &m)
	json.Unmarshal(m["result"], &res)
	var opts []map[string]json.RawMessage
	json.Unmarshal(res["configOptions"], &opts)
	for _, o := range opts {
		var id string
		json.Unmarshal(o["id"], &id)
		ids = append(ids, id)
	}
	return handled, restart, ids
}

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
					if err := os.WriteFile(filepath.Join(dir, marker), credentialMatrixMarker(name), 0o600); err != nil {
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

				if !anyAgentSignedIn(cfg) {
					t.Error("first-run help still reports no signed-in provider")
				}
				if unsigned := forkctl.UnsignedFleetAccounts(cfg, []forkctl.FleetEntry{{Name: "matrix", Agent: name + "@" + profile}}); len(unsigned) != 0 {
					t.Errorf("fleet rejected signed-in pinned account: %v", unsigned)
				}

				control := acpctl.New(cfg, name, "", "", t.TempDir(), acpctl.Selection{}, nil, nil, false, nil, acpHost())
				if creds := control.Creds(); !slices.Contains(creds, profile) {
					t.Errorf("ACP account selector omitted %s@%s via %s: %v", name, profile, source, creds)
				}
				if next, recognized := control.SelectorSelection(acpctl.CoopAccountID, profile); !recognized || next.Account != profile {
					t.Errorf("ACP account selection rejected %s@%s via %s: (%+v, %v)", name, profile, source, next, recognized)
				}
				if next, recognized := control.SelectorSelection(acpctl.CoopAccountID, "ghost"); !recognized || next.Account != "" {
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
					if unsigned := forkctl.UnsignedFleetAccounts(cfg, []forkctl.FleetEntry{{Name: "typo", Agent: name + "@personal"}}); len(unsigned) != 1 {
						t.Errorf("fleet accepted unrelated env-backed account: %v", unsigned)
					}
				}
			})
		}
	}
}

func credentialMatrixMarker(provider string) []byte {
	switch provider {
	case "claude":
		return []byte(`{"claudeAiOauth":{"accessToken":"access","expiresAt":4102444800000,"scopes":["user:inference"]}}`)
	case "codex":
		return []byte(`{"auth_mode":"chatgpt","tokens":{"id_token":"identity","access_token":"access","refresh_token":"refresh"},"last_refresh":"2026-08-06T00:00:00Z"}`)
	case "grok":
		return []byte(grokProfileCredential("2099-01-01T00:00:00Z", ""))
	default:
		return []byte(`{"token":"fixture"}`)
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
					if err := os.WriteFile(filepath.Join(dir, marker), credentialMatrixMarker(name), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(cfg.EnvFile(), []byte(source+"=token\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := cfg.SetDefaultProfile(name, "work"); err != nil {
					t.Fatal(err)
				}

				c := acpctl.New(cfg, base, "", "", t.TempDir(), acpctl.Selection{}, nil, nil, false, nil, acpHost())
				if got := c.SpawnableProviders(base); !slices.Contains(got, name) {
					t.Fatalf("ACP provider options omitted %s via %s: %v", name, source, got)
				}
				handled, restart, ids := selectorSet(t, c, acpctl.CoopProviderID, name)
				if !handled || !restart || !slices.Equal(ids, []string{acpctl.CoopPresetID, acpctl.CoopProviderID, acpctl.CoopAccountID}) {
					t.Fatalf("ACP provider selection %s via %s = handled %v restart %v options %v", name, source, handled, restart, ids)
				}
				target, presetName, ok := c.SpawnTarget()
				if !ok || presetName != "" || target.Provider != name || target.Account() != "work" {
					t.Errorf("ACP spawn target %s via %s = (%s, preset %q, %v), want %s@work", name, source, target.String(), presetName, ok, name)
				}
				creds, accounts := c.Creds(), c.Accounts()
				if !slices.Equal(creds, []string{"work"}) || !slices.Equal(accounts, []string{"work"}) {
					t.Errorf("ACP retarget %s via %s = creds %v, accounts %v; want [work] for both", name, source, creds, accounts)
				}
			})
		}
	}
}

func TestUnsignedFleetAccountsRejectsMissingCredential(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	got := forkctl.UnsignedFleetAccounts(cfg, []forkctl.FleetEntry{{Name: "api", Agent: "grok@work"}})
	if len(got) != 1 || !strings.Contains(got[0], "api/grok") || !strings.Contains(got[0], "work") {
		t.Fatalf("unsigned fleet accounts = %v, want api/grok work", got)
	}
}
