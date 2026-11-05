package liveprovider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

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
