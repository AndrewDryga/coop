package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeDefaultsPreserveReviewedPreferencesNotAuthority(t *testing.T) {
	for _, test := range []struct{ provider, file, raw, want string }{
		{"claude", "settings.json", `{"theme":"light","hooks":{"unsafe":true},"env":{"KEY":"secret"},"projects":{"foreign":{}},"sandbox":{"enabled":true}}`, "light"},
		{"codex", "config.toml", "model = 'preferred'\n[projects.foreign]\ntrust_level='trusted'\n[model_providers.custom]\napi_key='secret'\n", "preferred"},
		{"gemini", "settings.json", `{"security":{"auth":{"selectedType":"oauth-personal"},"folderTrust":{"enabled":true}},"ui":{"theme":"light"},"mcpServers":{"foreign":{}}}`, "light"},
		{"grok", "config.toml", "model = 'preferred'\n[projects.foreign]\napi_key='secret'\n", "preferred"},
	} {
		t.Run(test.provider, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, test.file), []byte(test.raw), 0600); err != nil {
				t.Fatal(err)
			}
			ag, _ := Get(test.provider)
			files, err := ag.NativeCredentials().Defaults(home)
			if err != nil {
				t.Fatal(err)
			}
			value := string(files[test.file])
			if !strings.Contains(value, test.want) {
				t.Fatal("reviewed preference lost", value)
			}
			for _, forbidden := range []string{"foreign", "secret", "hooks", "selectedType", "mcpServers", "api_key"} {
				if strings.Contains(value, forbidden) {
					t.Fatal("non-default authority crossed home boundary", value)
				}
			}
		})
	}
}
