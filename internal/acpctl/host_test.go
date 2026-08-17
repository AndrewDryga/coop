package acpctl

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
)

// signInCred writes a minimal adapter-valid credential. Duplicated from
// internal/cli/rotation_test.go (a small test fixture, genuinely shared, too small to be worth a
// testutil leaf — same treatment as the sessions extraction's gitOut/pathExists).
func signInCred(t *testing.T, cfg *config.Config, agent, name string) {
	t.Helper()
	dir := cfg.AgentProfileDir(agent, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ag, ok := agents.Get(agent)
	if !ok {
		t.Fatalf("unknown agent %q", agent)
	}
	file, _ := ag.AuthMarker()
	body := map[string]string{
		"claude": `{"claudeAiOauth":{"refreshToken":"refresh","scopes":["user:inference"]}}`,
		"codex":  `{"auth_mode":"chatgpt","tokens":{"refresh_token":"refresh"}}`,
		"gemini": `{"encrypted":"opaque"}`,
		"grok":   `{"issuer::id":{"key":"access","refresh_token":"refresh","expires_at":"2000-01-01T00:00:00Z","auth_mode":"oauth","oidc_issuer":"issuer","oidc_client_id":"client","principal_id":"principal","principal_type":"user","user_id":"user","team_id":"team","create_time":"2000-01-01T00:00:00Z"}}`,
	}[agent]
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func signInDeadCred(t *testing.T, cfg *config.Config, agent, name string) {
	t.Helper()
	dir := cfg.AgentProfileDir(agent, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ag, _ := agents.Get(agent)
	marker, _ := ag.AuthMarker()
	if err := os.WriteFile(filepath.Join(dir, marker), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// testHost returns a Host with real (duplicated, not injected) rotation/fusion-council policy for
// this package's OWN tests — acpctl must not import internal/cli (see
// .agent/kb/rules/internal-import-dag.md), so the functions below mirror, byte-for-byte, internal/cli's
// rotation.go (accountsFor/expandLadder) and fusion_council.go (resolveFusionCouncil/
// resolveACPFusionCouncil). Production wires the REAL cli functions through this same Host shape
// (see internal/cli's acpHost()); keep these test copies in sync if that policy changes — same
// duplication the sessions extraction used for its own test-only signInCred.
// WriteModelsCache defaults to a no-op: the on-disk cache format is cli-owned
// (internal/cli/modelscache.go) and unreachable here, so a test that cares about a cache write uses
// testHostCapturingModels instead (see control_test.go's two opportunistic-cache tests).
func testHost() Host {
	return Host{
		ExpandLadder:         testExpandLadder,
		AccountsFor:          testAccountsFor,
		ResolveFusionCouncil: testResolveACPFusionCouncil,
		WriteModelsCache:     func(*config.Config, string, []Model) error { return nil },
	}
}

// testHostCapturingModels is testHost with WriteModelsCache replaced by a fake that records what
// was asked to be cached, per agent — see control_test.go's TestACPControlOpportunisticModelCache /
// TestACPControlOpportunisticGeminiCache (RISK 2 in spec.md).
func testHostCapturingModels(captured map[string][]Model) Host {
	h := testHost()
	h.WriteModelsCache = func(_ *config.Config, agent string, models []Model) error {
		captured[agent] = models
		return nil
	}
	return h
}

// testAccountsFor mirrors internal/cli/rotation.go's accountsFor.
func testAccountsFor(cfg *config.Config, agent string) []string {
	def := cfg.DefaultProfileOf(agent)
	now := time.Now()
	var out []string
	if box.ProfileCredentialReady(cfg, agent, def, now) {
		out = append(out, def)
	}
	for _, c := range box.EffectiveProfiles(cfg, agent) {
		if c != def && box.ProfileCredentialReady(cfg, agent, c, now) {
			out = append(out, c)
		}
	}
	return out
}

// testExpandLadder mirrors internal/cli/rotation.go's expandLadder.
func testExpandLadder(cfg *config.Config, defaultAgent string, rungs []agents.Target) ([]agents.Target, error) {
	if len(rungs) == 0 {
		rungs = []agents.Target{{}} // defaultAgent, default model, all accounts
	}
	var out []agents.Target
	seen := map[string]bool{}
	add := func(t agents.Target) {
		if !seen[t.String()] {
			seen[t.String()] = true
			out = append(out, t)
		}
	}
	var missing []string // fan-out providers with no runnable account (reported only if NOTHING resolves)
	type repair struct {
		target  string
		relogin bool
	}
	var repairs []repair // selected or fan-out credentials whose exact login fixes an empty ladder
	recordRepair := func(agent, account string, pinned bool) {
		if box.ProfileCredentialReady(cfg, agent, account, time.Now()) {
			return
		}
		present := box.ProfileAuthed(cfg, agent, account)
		if !present && !pinned {
			return
		}
		target := agent + "@" + account
		if !slices.ContainsFunc(repairs, func(r repair) bool { return r.target == target }) {
			repairs = append(repairs, repair{target: target, relogin: present})
		}
	}
	for _, e := range rungs {
		agent := e.Provider
		if agent == "" {
			agent = defaultAgent
		}
		runnable := testAccountsFor(cfg, agent)
		accounts := e.Accounts
		if len(accounts) == 0 {
			if len(runnable) == 0 {
				for _, account := range box.EffectiveProfiles(cfg, agent) {
					recordRepair(agent, account, false)
				}
				if !slices.Contains(missing, agent) {
					missing = append(missing, agent)
				}
				continue
			}
			accounts = runnable
		}
		for _, acct := range accounts {
			if box.ProfileCredentialReady(cfg, agent, acct, time.Now()) {
				add(agents.Target{Provider: agent, Model: e.Model, Effort: e.Effort, Accounts: []string{acct}})
			} else {
				recordRepair(agent, acct, true)
			}
		}
	}
	if len(out) == 0 {
		if len(repairs) == 1 {
			state := "is not signed in"
			if repairs[0].relogin {
				state = "requires re-login"
			}
			return nil, fmt.Errorf("credential %s %s — run: %s", agents.DisplayTarget(repairs[0].target), state, agents.LoginCommand(repairs[0].target))
		}
		if len(repairs) > 1 {
			targets, commands := make([]string, len(repairs)), make([]string, len(repairs))
			for i, repair := range repairs {
				targets[i] = agents.DisplayTarget(repair.target)
				commands[i] = agents.LoginCommand(repair.target)
			}
			return nil, fmt.Errorf("credentials need login (%s) — run: %s", strings.Join(targets, ", "), strings.Join(commands, "; "))
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("no signed-in account for %s — run: coop login %s[@account]", strings.Join(missing, ", "), missing[0])
		}
		return nil, fmt.Errorf("%s: none of the ladder's accounts are signed in — run 'coop login', or edit the preset", defaultAgent)
	}
	return out, nil
}

// testFusionSelfPolicy mirrors internal/cli/fusion_council.go's fusionSelfPolicy.
type testFusionSelfPolicy int

const (
	testFusionRejectSelf testFusionSelfPolicy = iota
	testFusionExcludeSelf
)

// testResolveFusionCouncil mirrors internal/cli/fusion_council.go's resolveFusionCouncil, returning
// this package's own FusionCouncil instead of cli's unexported fusionCouncil.
func testResolveFusionCouncil(governor string, peers []agents.Target, p *preset.Preset, self testFusionSelfPolicy, available []string) (FusionCouncil, error) {
	var roles []preset.Role
	if p != nil {
		roles = p.ConsultRoles(governor)
	}
	roleNames := make(map[string]bool, len(roles))
	for _, role := range roles {
		roleNames[role.Name] = true
	}

	seen := make(map[string]bool, len(peers))
	var out FusionCouncil
	for _, peer := range peers {
		provider := peer.Provider
		if seen[provider] {
			return FusionCouncil{}, fmt.Errorf("fusion: --peer %s appears more than once; name each provider once", provider)
		}
		seen[provider] = true
		if roleNames[provider] {
			return FusionCouncil{}, fmt.Errorf("fusion: --peer %s conflicts with preset role %q; rename the role or drop the peer", provider, provider)
		}
		if provider == governor {
			if self == testFusionRejectSelf {
				return FusionCouncil{}, fmt.Errorf("fusion: governor %s cannot also be an explicit --peer", governor)
			}
			continue
		}
		out.Peers = append(out.Peers, peer)
		out.Members = append(out.Members, provider)
	}

	for _, role := range roles {
		usable := false
		for _, target := range role.TargetLadder() {
			if target.Provider == governor || slices.Contains(available, target.Provider) {
				usable = true
				break
			}
		}
		if usable {
			out.Members = append(out.Members, role.Name)
		} else {
			out.UnavailableRoles = append(out.UnavailableRoles, role.Name)
		}
	}
	if len(out.Members) == 0 {
		if len(out.UnavailableRoles) > 0 {
			return FusionCouncil{}, fmt.Errorf("fusion: preset council role(s) %s have no target with mounted credentials", strings.Join(out.UnavailableRoles, ", "))
		}
		return FusionCouncil{}, fmt.Errorf("fusion needs its council: name an explicit --peer or use a preset with an effective consult role")
	}
	return out, nil
}

// testResolveACPFusionCouncil mirrors internal/cli/fusion_council.go's resolveACPFusionCouncil —
// the function production wires as Host.ResolveFusionCouncil (see internal/cli's acpHost()).
func testResolveACPFusionCouncil(governor string, peers []agents.Target, p *preset.Preset, available []string, reachable []agents.Target) (FusionCouncil, error) {
	providers := []string{governor}
	if p != nil {
		providers = nil
		for _, target := range reachable {
			if !slices.Contains(providers, target.Provider) {
				providers = append(providers, target.Provider)
			}
		}
		if len(providers) == 0 {
			return FusionCouncil{}, fmt.Errorf("coop acp fusion: preset %s has no reachable lead target", p.Name)
		}
	}

	var current FusionCouncil
	var first FusionCouncil
	var unavailable []string
	for _, provider := range providers {
		council, err := testResolveFusionCouncil(provider, peers, p, testFusionExcludeSelf, available)
		if err != nil {
			if p != nil {
				return FusionCouncil{}, fmt.Errorf("coop acp fusion: preset %s lead provider %s: %w", p.Name, provider, err)
			}
			return FusionCouncil{}, fmt.Errorf("coop acp fusion: %w", err)
		}
		if len(first.Members) == 0 {
			first = council
		}
		for _, role := range council.UnavailableRoles {
			if !slices.Contains(unavailable, role) {
				unavailable = append(unavailable, role)
			}
		}
		if provider == governor {
			current = council
		}
	}
	if len(current.Members) == 0 {
		current = first // outer supervisor may begin on a skipped declared lead; its child uses rung one.
	}
	current.UnavailableRoles = unavailable
	return current, nil
}
