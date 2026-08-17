package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// signInCred writes a minimal adapter-valid credential so rotation sees a runnable account, not
// merely a marker file. Keep every registered shape here because this helper is shared by the CLI's
// cross-provider credential matrix.
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
	file, _ := ag.AuthMarker()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// expandLadder: bare model → all signed-in accounts (marked-default first, then the rest);
// pinned → one target (unauthed pinned skipped); empty ladder → default model, all accounts.
func TestExpandLadder(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg, "claude", "work")
	signInCred(t, cfg, "claude", "personal")
	if err := cfg.SetDefaultProfile("claude", "personal"); err != nil { // default first
		t.Fatal(err)
	}

	// Bare model fans out, marked-default (personal) first.
	got, err := expandLadder(cfg, "claude", []agents.Target{{Model: "opus"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude:opus@personal", "claude:opus@work"}; !slices.Equal(members(got), want) {
		t.Errorf("bare model fan-out = %v, want %v (default first)", members(got), want)
	}
	// Ladder: bare then pinned; dedup, order preserved.
	got, _ = expandLadder(cfg, "claude", []agents.Target{{Model: "fable"}, {Model: "opus", Accounts: []string{"work"}}})
	if want := []string{"claude:fable@personal", "claude:fable@work", "claude:opus@work"}; !slices.Equal(members(got), want) {
		t.Errorf("ladder = %v, want %v", members(got), want)
	}
	// Empty ladder → default model (empty) across accounts (provider shown, no model).
	got, _ = expandLadder(cfg, "claude", nil)
	if want := []string{"claude@personal", "claude@work"}; !slices.Equal(members(got), want) {
		t.Errorf("empty ladder = %v, want the accounts on the default model", members(got))
	}
	// A pinned account that isn't signed in is skipped, not an error, as long as something remains.
	got, _ = expandLadder(cfg, "claude", []agents.Target{{Model: "opus", Accounts: []string{"ghost"}}, {Model: "fable"}})
	if slices.Contains(members(got), "claude:opus@ghost") {
		t.Errorf("unsigned pinned account should be skipped: %v", members(got))
	}
	// No signed-in accounts at all → error.
	if _, err := expandLadder(&config.Config{ConfigDir: t.TempDir()}, "claude", nil); err == nil {
		t.Error("no signed-in account should error")
	}
}

func TestExpandLadderSkipsCredentialsThatNeedRelogin(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg, "claude", "personal")
	signInDeadCred(t, cfg, "claude", "personal_backup")
	if err := cfg.SetDefaultProfile("claude", "personal"); err != nil {
		t.Fatal(err)
	}

	if got := accountsFor(cfg, "claude"); !slices.Equal(got, []string{"personal"}) {
		t.Fatalf("runnable Claude accounts = %v, want only personal", got)
	}
	targets, err := expandLadder(cfg, "claude", nil)
	if err != nil || !slices.Equal(members(targets), []string{"claude@personal"}) {
		t.Fatalf("rotation with dead fallback = %v, %v", members(targets), err)
	}

	_, err = expandLadder(cfg, "claude", []agents.Target{{Accounts: []string{"personal_backup"}}})
	if err == nil || !strings.Contains(err.Error(), "credential claude@personal_backup requires re-login") ||
		!strings.Contains(err.Error(), "coop login claude@personal_backup") {
		t.Fatalf("dead pinned credential error = %v, want exact re-login remedy", err)
	}

	_, err = expandLadder(cfg, "claude", []agents.Target{{Accounts: []string{"missing"}}})
	if err == nil || !strings.Contains(err.Error(), "credential claude@missing is not signed in") ||
		!strings.Contains(err.Error(), "coop login claude@missing") || strings.Contains(err.Error(), "personal_backup") {
		t.Fatalf("missing pinned credential error = %v, want only its exact login remedy", err)
	}

	signInDeadCred(t, cfg, "claude", "unsafe;echo nope")
	_, err = expandLadder(cfg, "claude", []agents.Target{{Accounts: []string{"unsafe;echo nope"}}})
	if err == nil || !strings.Contains(err.Error(), "coop login 'claude@unsafe;echo nope'") {
		t.Fatalf("unsafe credential recovery command = %v, want a shell-quoted target", err)
	}

	signInDeadCred(t, cfg, "claude", "line\nnext")
	_, err = expandLadder(cfg, "claude", []agents.Target{{Accounts: []string{"line\nnext"}}})
	if err == nil || strings.Contains(err.Error(), "line\nnext") ||
		!strings.Contains(err.Error(), `"claude@line\nnext"`) ||
		!strings.Contains(err.Error(), `coop login $'claude@line\nnext'`) {
		t.Fatalf("control-character credential recovery = %q, want escaped display and command", err)
	}
}

// Effort rides the ladder into each rotation target, and applyRunTarget lands it in cfg so
// EffortFor (and thus the agent command) sees it — the CLI glue for a /effort target. What the
// loop then does with the rotation's active target is internal/loop's
// TestApplyTargetThreadsEffortToConfig.
func TestEffortThreadsToConfig(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg, "codex", "work")
	a := &app{cfg: cfg}

	// Loop path: the positional target IS the ladder, and expanding it preserves the effort.
	rungs := []agents.Target{{Provider: "codex", Model: "gpt-5.6-sol", Effort: "high", Accounts: []string{"work"}}}
	rot, err := a.buildRotation("codex", rungs)
	if err != nil {
		t.Fatal(err)
	}
	if got := rot.Members(); !slices.Contains(got, "codex:gpt-5.6-sol/high@work") {
		t.Fatalf("rotation targets = %v, want one carrying /high", got)
	}

	// Single-run path: applyRunTarget lands the effort in the top tier, alongside the model.
	cfg2 := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg2, "codex", "work")
	a2 := &app{cfg: cfg2}
	if err := a2.applyRunTarget(agents.Target{Provider: "codex", Model: "gpt-5.6-sol", Effort: "xhigh", Accounts: []string{"work"}}); err != nil {
		t.Fatal(err)
	}
	if got := cfg2.EffortFor("codex"); got != "xhigh" {
		t.Errorf("after applyRunTarget, EffortFor(codex) = %q, want xhigh", got)
	}
	if got := cfg2.ModelFor("codex"); got != "gpt-5.6-sol" {
		t.Errorf("model rides alongside: ModelFor(codex) = %q, want gpt-5.6-sol", got)
	}
}

// A cross-provider ladder fans each rung across ITS OWN provider's accounts, so the loop rotates
// across agents; a rung whose provider has no signed-in account is skipped, not fatal, as long as
// another rung resolves.
func TestExpandLadderCrossProvider(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, cfg, "claude", "work")
	signInCred(t, cfg, "codex", "work")

	got, err := expandLadder(cfg, "claude", []agents.Target{{Model: "opus"}, {Provider: "codex", Model: "gpt-5"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude:opus@work", "codex:gpt-5@work"}; !slices.Equal(members(got), want) {
		t.Errorf("cross-provider ladder = %v, want %v (rotates across agents)", members(got), want)
	}
	// codex signed out → its rung is skipped, claude's remains (not a fatal "nothing signed in").
	noCodex := &config.Config{ConfigDir: t.TempDir()}
	signInCred(t, noCodex, "claude", "work")
	got, err = expandLadder(noCodex, "claude", []agents.Target{{Model: "opus"}, {Provider: "codex", Model: "gpt-5"}})
	if err != nil || !slices.Equal(members(got), []string{"claude:opus@work"}) {
		t.Errorf("unsigned provider rung should be skipped: got %v (%v)", members(got), err)
	}
}

func members(ts []agents.Target) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.String()
	}
	return out
}

// oneOffLadder parses a decomposed one-off (model, effort, account) into a single ladder entry,
// model-first with the model@account shortcut; conflicting accounts error.
func TestOneOffLadder(t *testing.T) {
	if l, _ := oneOffLadder("", "", ""); l != nil {
		t.Errorf("no one-off → nil ladder, got %v", l)
	}
	l, err := oneOffLadder("opus@work", "", "high")
	if err != nil || len(l) != 1 || l[0].Model != "opus" || l[0].Account() != "work" {
		t.Errorf("model opus@work = %+v (%v)", l, err)
	}
	if err != nil || len(l) != 1 || l[0].Effort != "high" {
		t.Fatalf("oneOffLadder effort = %#v, %v", l, err)
	}
	l, _ = oneOffLadder("opus", "work", "")
	if l[0].Model != "opus" || l[0].Account() != "work" {
		t.Errorf("model opus + account work = %+v", l)
	}
	if _, err := oneOffLadder("opus@work", "personal", ""); err == nil {
		t.Error("account given twice (model @work + account personal) should error")
	}
	if _, err := oneOffLadder("opus@", "", ""); err == nil {
		t.Error("empty account after @ should error")
	}
}
