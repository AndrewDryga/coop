package liveprovider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestPrepareCopiesOnlyAdapterCredentialArtifacts(t *testing.T) {
	source := t.TempDir()
	selections := []Selection{
		{Provider: "claude", Account: "default", SourceDefault: true},
		{Provider: "codex", Account: "work", SourceDefault: true},
		{Provider: "gemini", Account: "default", SourceDefault: true},
		{Provider: "grok", Account: "default", SourceDefault: true},
	}
	files := map[string]string{
		"claude/profiles/default/.credentials.json":       `{"claudeAiOauth":{"accessToken":"claude-secret","refreshToken":"REFRESH_CANARY","expiresAt":9999999999999,"scopes":["user:inference"]}}`,
		"codex/profiles/work/auth.json":                   `{"auth_mode":"chatgpt","tokens":{"id_token":"identity","access_token":"codex-secret","refresh_token":"REFRESH_CANARY"},"last_refresh":"2026-07-15T00:00:00Z"}`,
		"gemini/profiles/default/gemini-credentials.json": `{"oauth":"gemini-secret"}`,
		"gemini/profiles/default/google_accounts.json":    `{"active":"user"}`,
		"gemini/profiles/default/settings.json":           `{"security":{"auth":{"selectedType":"gemini-api-key","hidden":"drop"},"other":"drop"},"hooks":{"before":"drop"}}`,
		"grok/profiles/default/auth.json":                 `{"issuer::id":{"key":"grok-secret","refresh_token":"REFRESH_CANARY","expires_at":"2999-01-01T00:00:00Z","auth_mode":"oauth","oidc_issuer":"issuer","oidc_client_id":"client","principal_id":"principal","principal_type":"user","user_id":"user","team_id":"team","create_time":"2026-07-16T01:00:00Z"}}`,
	}
	for name, content := range files {
		writeSource(t, filepath.Join(source, name), content, 0o600)
	}
	for _, name := range []string{
		"claude/profiles/default/sessions/history.jsonl",
		"codex/profiles/work/AGENTS.md",
		"gemini/profiles/default/hooks/before.sh",
		"grok/profiles/default/config.toml",
	} {
		writeSource(t, filepath.Join(source, name), "FORBIDDEN_CANARY", 0o600)
	}
	writeSource(t, filepath.Join(source, "env"), strings.Join([]string{
		"ANTHROPIC_AUTH_TOKEN=claude-env-secret",
		"OPENAI_API_KEY=codex-env-secret",
		"GEMINI_API_KEY=gemini-env-secret",
		"XAI_API_KEY=grok-env-secret",
		"UNRELATED_SECRET=must-not-copy",
		"GOOGLE_API_KEY", // A bare assignment must never import the ambient process value.
	}, "\n")+"\n", 0o600)

	destination := filepath.Join(t.TempDir(), "isolated")
	prepared, err := Prepare(source, destination, selections)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.VerifySources(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"claude/profiles/default/.credentials.json",
		"codex/profiles/work/auth.json",
		"gemini/profiles/default/settings.json",
		"grok/profiles/default/auth.json",
	} {
		destinationPath := filepath.Join(destination, name)
		data, err := os.ReadFile(destinationPath)
		if err != nil {
			t.Fatalf("read copied %s: %v", name, err)
		}
		if name == "gemini/profiles/default/settings.json" {
			if got, want := string(data), `{"security":{"auth":{"selectedType":"gemini-api-key"}}}`+"\n"; got != want {
				t.Errorf("Gemini selector = %s, want %s", got, want)
			}
		} else if strings.Contains(string(data), "REFRESH_CANARY") || !strings.Contains(string(data), "secret") {
			t.Errorf("copied %s did not retain access-only authority: %s", name, data)
		}
		assertPrivateCopy(t, filepath.Join(source, name), destinationPath)
	}
	for _, name := range []string{
		"gemini/profiles/default/gemini-credentials.json",
		"gemini/profiles/default/google_accounts.json",
	} {
		if _, err := os.Lstat(filepath.Join(destination, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("host-bound Gemini artifact %s was copied", name)
		}
	}
	for _, name := range []string{
		"claude/profiles/default/sessions",
		"codex/profiles/work/AGENTS.md",
		"gemini/profiles/default/hooks",
		"grok/profiles/default/config.toml",
	} {
		if _, err := os.Lstat(filepath.Join(destination, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("forbidden artifact %s was copied", name)
		}
	}
	env, err := os.ReadFile(filepath.Join(destination, "env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "GEMINI_API_KEY=") {
		t.Errorf("isolated env missing selected Gemini API key")
	}
	for _, forbidden := range []string{
		"ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "XAI_API_KEY", "UNRELATED_SECRET", "GOOGLE_API_KEY",
	} {
		if strings.Contains(string(env), forbidden) {
			t.Errorf("isolated env copied %s", forbidden)
		}
	}
	for _, selection := range selections {
		if !prepared.CredentialPresent(selection.Provider, selection.Account) {
			t.Errorf("%s@%s not detected as configured", selection.Provider, selection.Account)
		}
		if prepared.Account(selection.Provider) != selection.Account {
			t.Errorf("%s destination default = %q, want %q", selection.Provider, prepared.Account(selection.Provider), selection.Account)
		}
	}
	defaults, err := os.ReadFile(filepath.Join(destination, "defaults"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(defaults), "claude=default\ncodex=work\ngemini=default\ngrok=default\n"; got != want {
		t.Errorf("defaults = %q, want %q", got, want)
	}
}

func TestPrepareEnvOnlyBelongsToSourceDefault(t *testing.T) {
	source := t.TempDir()
	writeSource(t, filepath.Join(source, "env"), "OPENAI_API_KEY=env-secret\n", 0o600)

	for _, tc := range []struct {
		name          string
		sourceDefault bool
		wantPresent   bool
		wantEnv       bool
	}{
		{name: "source default", sourceDefault: true, wantPresent: true, wantEnv: true},
		{name: "named account", sourceDefault: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "isolated")
			prepared, err := Prepare(source, destination, []Selection{{
				Provider: "codex", Account: "work", SourceDefault: tc.sourceDefault,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if got := prepared.CredentialPresent("codex", "work"); got != tc.wantPresent {
				t.Errorf("CredentialPresent = %v, want %v", got, tc.wantPresent)
			}
			if got := prepared.SafeThrough("codex", "work", time.Now().Add(time.Hour)); got != tc.wantPresent {
				t.Errorf("SafeThrough = %v, want %v", got, tc.wantPresent)
			}
			_, err = os.Stat(filepath.Join(destination, "env"))
			if got := err == nil; got != tc.wantEnv {
				t.Errorf("isolated env exists = %v, want %v (err %v)", got, tc.wantEnv, err)
			}
		})
	}
}

func TestPrepareClaudeRefreshOnlyNeedsAuthentication(t *testing.T) {
	source := t.TempDir()
	credential := filepath.Join(source, "claude", "profiles", "default", ".credentials.json")
	writeSource(t, credential,
		`{"claudeAiOauth":{"accessToken":"","expiresAt":0,"scopes":["user:inference"],"refreshToken":"REFRESH_CANARY"}}`, 0o600)
	destination := filepath.Join(t.TempDir(), "isolated")
	prepared, err := Prepare(source, destination, []Selection{{
		Provider: "claude", Account: "default", SourceDefault: true,
	}})
	if err != nil {
		t.Fatalf("refresh-only Claude credential should reach preflight: %v", err)
	}
	if got := prepared.PreflightReason("claude", "default", time.Now().Add(time.Hour)); got != ReasonCredentialRefresh {
		t.Fatalf("PreflightReason = %q, want %q", got, ReasonCredentialRefresh)
	}
	projected, err := os.ReadFile(filepath.Join(destination, "claude", "profiles", "default", ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(projected), "REFRESH_CANARY") {
		t.Fatal("refresh-only Claude projection retained refresh authority")
	}
}

func TestPrepareRejectsAmbiguousActiveEnvironmentCredentials(t *testing.T) {
	tests := []struct {
		provider string
		env      string
	}{
		{provider: "claude", env: "ANTHROPIC_API_KEY=CLAUDE_ONE_CANARY\nANTHROPIC_AUTH_TOKEN=CLAUDE_TWO_CANARY\n"},
		{provider: "gemini", env: "GEMINI_API_KEY=GEMINI_ONE_CANARY\nGOOGLE_API_KEY=GEMINI_TWO_CANARY\n"},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			source := t.TempDir()
			writeSource(t, filepath.Join(source, "env"), tt.env, 0o600)
			destination := filepath.Join(t.TempDir(), "isolated")
			_, err := Prepare(source, destination, []Selection{{
				Provider: tt.provider, Account: "default", SourceDefault: true,
			}})
			if err == nil || !strings.Contains(err.Error(), "ambiguous active environment credentials") {
				t.Fatalf("ambiguous %s credentials = %v", tt.provider, err)
			}
			for _, canary := range []string{"CLAUDE_ONE_CANARY", "CLAUDE_TWO_CANARY", "GEMINI_ONE_CANARY", "GEMINI_TWO_CANARY"} {
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("ambiguous credential error exposed %s", canary)
				}
			}
			if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("ambiguous credentials published destination: %v", statErr)
			}
		})
	}
}

func TestPrepareGeminiAuthFollowsSelectedAuthority(t *testing.T) {
	tests := []struct {
		name          string
		selected      string
		env           string
		withMarker    bool
		wantPresent   bool
		wantPreflight string
	}{
		{
			name: "stale marker cannot satisfy mismatched API key", selected: "gemini-api-key",
			env: "GOOGLE_API_KEY=token\n", withMarker: true, wantPreflight: ReasonMissingCredential,
		},
		{
			name: "selected API key is portable", selected: "gemini-api-key",
			env: "GEMINI_API_KEY=token\n", withMarker: true, wantPresent: true,
		},
		{
			name: "selected OAuth marker stays host bound", selected: "oauth-personal",
			env: "GEMINI_API_KEY=token\n", withMarker: true, wantPresent: true,
			wantPreflight: ReasonCredentialNotPortable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := t.TempDir()
			profileDir := filepath.Join(source, "gemini", "profiles", "default")
			writeSource(t, filepath.Join(profileDir, "settings.json"),
				`{"security":{"auth":{"selectedType":"`+tt.selected+`"}}}`, 0o600)
			if tt.withMarker {
				writeSource(t, filepath.Join(profileDir, "gemini-credentials.json"), `{"encrypted":"host-bound"}`, 0o600)
			}
			writeSource(t, filepath.Join(source, "env"), tt.env, 0o600)
			prepared, err := Prepare(source, filepath.Join(t.TempDir(), "isolated"), []Selection{{
				Provider: "gemini", Account: "default", SourceDefault: true,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if got := prepared.CredentialPresent("gemini", "default"); got != tt.wantPresent {
				t.Errorf("CredentialPresent = %v, want %v", got, tt.wantPresent)
			}
			if got := prepared.PreflightReason("gemini", "default", time.Now().Add(time.Hour)); got != tt.wantPreflight {
				t.Errorf("PreflightReason = %q, want %q", got, tt.wantPreflight)
			}
		})
	}
}

func TestPrepareGeminiHostBoundMarkerWithoutSelectorDoesNotFallBackToEnv(t *testing.T) {
	source := t.TempDir()
	profileDir := filepath.Join(source, "gemini", "profiles", "default")
	writeSource(t, filepath.Join(profileDir, "gemini-credentials.json"), `{"encrypted":"host-bound"}`, 0o600)
	writeSource(t, filepath.Join(source, "env"), "GEMINI_API_KEY=UNRELATED_KEY_CANARY\n", 0o600)
	destination := filepath.Join(t.TempDir(), "isolated")
	prepared, err := Prepare(source, destination, []Selection{{
		Provider: "gemini", Account: "default", SourceDefault: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := prepared.PreflightReason("gemini", "default", time.Now().Add(time.Hour)); got != ReasonCredentialNotPortable {
		t.Errorf("Gemini host-bound reason = %q, want %q", got, ReasonCredentialNotPortable)
	}
	if _, err := os.Lstat(filepath.Join(destination, "env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unrelated Gemini API key was copied: %v", err)
	}
}

func TestPrepareRejectsUnsafeCredentialSourcesWithoutDisclosure(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, root, path string)
	}{
		{name: "final symlink", make: func(t *testing.T, root, path string) {
			outside := filepath.Join(t.TempDir(), "TOKEN_CANARY")
			writeSource(t, outside, "TOKEN_CANARY", 0o600)
			mustMkdir(t, filepath.Dir(path))
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "ancestor symlink", make: func(t *testing.T, root, path string) {
			outside := t.TempDir()
			writeSource(t, filepath.Join(outside, "auth.json"), "TOKEN_CANARY", 0o600)
			mustMkdir(t, filepath.Join(root, "codex", "profiles"))
			if err := os.Symlink(outside, filepath.Dir(path)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hardlink", make: func(t *testing.T, root, path string) {
			writeSource(t, path, "TOKEN_CANARY", 0o600)
			if err := os.Link(path, filepath.Join(root, "TOKEN_CANARY-link")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "fifo", make: func(t *testing.T, root, path string) {
			mustMkdir(t, filepath.Dir(path))
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "group writable", make: func(t *testing.T, root, path string) {
			writeSource(t, path, "TOKEN_CANARY", 0o620)
		}},
		{name: "oversize", make: func(t *testing.T, root, path string) {
			writeSource(t, path, strings.Repeat("T", maxCredentialBytes+1), 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "SENSITIVE_PATH_CANARY")
			source := filepath.Join(parent, "source")
			mustMkdir(t, source)
			path := filepath.Join(source, "codex", "profiles", "default", "auth.json")
			tc.make(t, source, path)
			destination := filepath.Join(parent, "isolated")
			_, err := Prepare(source, destination, []Selection{{Provider: "codex", Account: "default", SourceDefault: true}})
			if err == nil {
				t.Fatal("unsafe source was accepted")
			}
			for _, canary := range []string{"SENSITIVE_PATH_CANARY", "TOKEN_CANARY", source, path} {
				if strings.Contains(err.Error(), canary) {
					t.Errorf("error disclosed %q: %v", canary, err)
				}
			}
			if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("partial destination remains: %v", statErr)
			}
		})
	}
}

func TestCredentialDetailCodeIsRedactedAndActionable(t *testing.T) {
	cases := map[string]string{
		"claude credential artifact 1 projection failed":                 "credential_projection",
		"codex credential artifact 1 has unsafe permissions":             "credential_permissions",
		"grok credential artifact 1 exceeds the size limit":              "credential_size",
		"gemini credential artifact 1 is not a regular single-link file": "credential_file_type",
	}
	for message, want := range cases {
		if got := CredentialDetailCode(errors.New(message)); got != want {
			t.Errorf("CredentialDetailCode(%q) = %q, want %q", message, got, want)
		}
	}
}

func TestReadSourceRejectsReplacementDuringRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "auth.json")
	writeSource(t, path, "old", 0o600)
	_, err := readSource(root, path, "codex", 0, func() {
		if renameErr := os.Rename(path, path+".old"); renameErr != nil {
			t.Fatal(renameErr)
		}
		writeSource(t, path, "new", 0o600)
	})
	if err == nil || !strings.Contains(err.Error(), "changed while reading") {
		t.Fatalf("replacement error = %v", err)
	}
}

func TestPrepareRejectsMalformedProjectionAndCleansStaging(t *testing.T) {
	source := t.TempDir()
	writeSource(t, filepath.Join(source, "gemini", "profiles", "default", "gemini-credentials.json"), `{}`, 0o600)
	writeSource(t, filepath.Join(source, "gemini", "profiles", "default", "settings.json"), `{`, 0o600)
	parent := t.TempDir()
	destination := filepath.Join(parent, "isolated")
	_, err := Prepare(source, destination, []Selection{{Provider: "gemini", Account: "default", SourceDefault: true}})
	if err == nil || !strings.Contains(err.Error(), "projection failed") {
		t.Fatalf("malformed projection error = %v", err)
	}
	entries, readErr := os.ReadDir(parent)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("partial staging state remains: %v", entries)
	}
}

func TestLiveCredentialExtensionContractFailsClosed(t *testing.T) {
	if _, err := projectCredential("codex", 0, agents.CredentialArtifact{Name: "auth.json"}, []byte(`{}`)); err == nil ||
		!strings.Contains(err.Error(), "projection failed") {
		t.Fatalf("nil credential projector error = %v", err)
	}
	for status, want := range map[agents.CredentialPortability]string{
		agents.CredentialPortable:        "",
		agents.CredentialRefreshRequired: ReasonCredentialRefresh,
		agents.CredentialNotPortable:     ReasonCredentialNotPortable,
		agents.CredentialUnknown:         ReasonUnsafeCredential,
		agents.CredentialPortability(99): ReasonUnsafeCredential,
	} {
		if got := portabilityReason(status); got != want {
			t.Errorf("portabilityReason(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestPreparedFingerprintDetectsCredentialChanges(t *testing.T) {
	mutations := map[string]func(t *testing.T, source, primary string){
		"rewrite": func(t *testing.T, _, primary string) { writeSource(t, primary, `{"changed":true}`, 0o600) },
		"chmod": func(t *testing.T, _, primary string) {
			if err := os.Chmod(primary, 0o400); err != nil {
				t.Fatal(err)
			}
		},
		"hardlink": func(t *testing.T, source, primary string) {
			if err := os.Link(primary, filepath.Join(source, "new-link")); err != nil {
				t.Fatal(err)
			}
		},
		"add companion": func(t *testing.T, source, _ string) {
			writeSource(t, filepath.Join(source, "gemini", "profiles", "default", "google_accounts.json"), `{}`, 0o600)
		},
		"delete": func(t *testing.T, _, primary string) {
			if err := os.Remove(primary); err != nil {
				t.Fatal(err)
			}
		},
		"same-content replacement": func(t *testing.T, _, primary string) {
			data, err := os.ReadFile(primary)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(primary)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(primary); err != nil {
				t.Fatal(err)
			}
			writeSource(t, primary, string(data), info.Mode().Perm())
			if err := os.Chtimes(primary, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			source := t.TempDir()
			primary := filepath.Join(source, "gemini", "profiles", "default", "gemini-credentials.json")
			writeSource(t, primary, `{}`, 0o600)
			prepared, err := Prepare(source, filepath.Join(t.TempDir(), "isolated"), []Selection{{Provider: "gemini", Account: "default", SourceDefault: true}})
			if err != nil {
				t.Fatal(err)
			}
			mutate(t, source, primary)
			if err := prepared.VerifySources(); err == nil {
				t.Fatal("source credential mutation was not detected")
			} else if strings.Contains(err.Error(), source) {
				t.Fatalf("integrity error disclosed source path: %v", err)
			}
		})
	}
}

func TestPreparedDistinguishesHostBoundCredentials(t *testing.T) {
	source := t.TempDir()
	writeSource(t, filepath.Join(source, "gemini", "profiles", "default", "gemini-credentials.json"), `{"encrypted":"host-bound"}`, 0o600)
	writeSource(t, filepath.Join(source, "gemini", "profiles", "default", "settings.json"), `{"security":{"auth":{"selectedType":"oauth-personal"}}}`, 0o600)
	prepared, err := Prepare(source, filepath.Join(t.TempDir(), "isolated"), []Selection{{
		Provider: "gemini", Account: "default", SourceDefault: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := prepared.PreflightReason("gemini", "default", time.Now().Add(time.Hour)); got != ReasonCredentialNotPortable {
		t.Errorf("Gemini host-bound reason = %q, want %q", got, ReasonCredentialNotPortable)
	}
}

func TestPreparedSafetyUsesTheIsolatedCopy(t *testing.T) {
	source := t.TempDir()
	primary := filepath.Join(source, "claude", "profiles", "default", ".credentials.json")
	writeSource(t, primary, `{"claudeAiOauth":{"accessToken":"copied-expired","expiresAt":1,"scopes":["user:inference"]}}`, 0o600)
	prepared, err := Prepare(source, filepath.Join(t.TempDir(), "isolated"), []Selection{{
		Provider: "claude", Account: "default", SourceDefault: true,
	}})
	if err != nil {
		t.Fatal(err)
	}

	future := time.Now().Add(2 * time.Hour).UnixMilli()
	writeSource(t, primary, fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"new-source-token","expiresAt":%d,"scopes":["user:inference"]}}`, future), 0o600)
	if prepared.SafeThrough("claude", "default", time.Now().Add(time.Hour)) {
		t.Fatal("source mutation changed the isolated credential safety classification")
	}
}

func TestSelectionResolutionAndValidation(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	bare, err := SelectionForTarget(cfg, agents.Target{Provider: "claude"})
	if err != nil || bare.Account != config.DefaultProfile || !bare.SourceDefault {
		t.Fatalf("bare selection = %+v, %v", bare, err)
	}
	named, err := SelectionForTarget(cfg, agents.Target{Provider: "codex", Accounts: []string{"work"}})
	if err != nil || named.Account != "work" || named.SourceDefault {
		t.Fatalf("named selection = %+v, %v", named, err)
	}
	if _, err := SelectionForTarget(cfg, agents.Target{Provider: "codex", Accounts: []string{"work", "personal"}}); err == nil {
		t.Fatal("multi-account live target was accepted")
	}
	if _, err := Prepare(t.TempDir(), filepath.Join(t.TempDir(), "isolated"), []Selection{
		{Provider: "codex", Account: "default"}, {Provider: "codex", Account: "default"},
	}); err == nil {
		t.Fatal("duplicate selection was accepted")
	}
	selections, err := SelectionsForTargets(cfg, []agents.Target{
		{Provider: "codex", Accounts: []string{"work", "personal"}},
		{Provider: "codex", Accounts: []string{"work"}},
		{Provider: "gemini"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(selections))
	for _, selection := range selections {
		got = append(got, selection.Provider+"@"+selection.Account)
	}
	if want := []string{"codex@work", "codex@personal", "gemini@default"}; !slices.Equal(got, want) {
		t.Errorf("expanded live selections = %v, want %v", got, want)
	}
}

func TestDefaultSelectionsIsRegistryCompleteAndNarrow(t *testing.T) {
	root := t.TempDir()
	for _, provider := range agents.Names() {
		mustMkdir(t, filepath.Join(root, provider, "profiles", "work"))
	}
	cfg := &config.Config{ConfigDir: root}
	got, err := DefaultSelections(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var providers []string
	for _, selection := range got {
		if !selection.SourceDefault || selection.Account != config.DefaultProfile {
			t.Errorf("default selection = %+v", selection)
		}
		providers = append(providers, selection.Provider)
	}
	if !slices.Equal(providers, agents.Names()) {
		t.Errorf("default selections = %v, want registry %v", providers, agents.Names())
	}
}

func TestPreparedRevokeIsIdempotentAndDoesNotFollowReplacementSymlink(t *testing.T) {
	parent := t.TempDir()
	vault := filepath.Join(parent, "isolated")
	mustMkdir(t, vault)
	writeSource(t, filepath.Join(vault, "credential"), "projected", 0o600)
	prepared := &Prepared{ConfigDir: vault}
	if err := prepared.Revoke(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(vault); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published credential path survived revoke: %v", err)
	}
	if err := prepared.Revoke(); err != nil {
		t.Fatalf("idempotent revoke failed: %v", err)
	}

	outside := filepath.Join(t.TempDir(), "outside")
	writeSource(t, outside, "source-must-survive", 0o600)
	if err := os.Symlink(outside, vault); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Revoke(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "source-must-survive" {
		t.Fatalf("revoke followed replacement symlink: data=%q err=%v", data, err)
	}
}

func TestPreparedRevokeRetriesChildTombstoneDeletion(t *testing.T) {
	parent := t.TempDir()
	vault := filepath.Join(parent, "config")
	mustMkdir(t, vault)
	writeSource(t, filepath.Join(vault, "credential"), "projected", 0o600)
	prepared := &Prepared{ConfigDir: vault}
	tombstone, err := prepared.RevocationPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(vault, tombstone); err != nil {
		t.Fatal(err)
	}

	originalRemove := removeRevokedCredentialTree
	removeRevokedCredentialTree = func(string) error { return errors.New("injected removal failure") }
	t.Cleanup(func() { removeRevokedCredentialTree = originalRemove })
	if err := prepared.Revoke(); err == nil {
		t.Fatal("child-side tombstone deletion failure was accepted")
	}
	if _, err := os.Stat(tombstone); err != nil {
		t.Fatalf("retryable credential tombstone missing: %v", err)
	}
	removeRevokedCredentialTree = originalRemove
	if err := prepared.Revoke(); err != nil {
		t.Fatalf("retry credential tombstone deletion: %v", err)
	}
	if _, err := os.Lstat(tombstone); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential tombstone survived retry: %v", err)
	}
}

func writeSource(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func assertPrivateCopy(t *testing.T, source, destination string) {
	t.Helper()
	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	destinationInfo, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(sourceInfo, destinationInfo) {
		t.Errorf("destination aliases source inode")
	}
	if got := destinationInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("destination mode = %o, want 600", got)
	}
	if links, ok := linkCount(destinationInfo); !ok || links != 1 {
		t.Errorf("destination link count = %d, %v; want 1, true", links, ok)
	}
}
