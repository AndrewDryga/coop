package agent

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/crypto/scrypt"
)

func TestNativeFamiliesKeepOneSharedHomeSeed(t *testing.T) {
	for _, pair := range []struct{ provider, a, b string }{{"codex", "chatgpt", "apikey"}, {"gemini", "oauth-personal", "gemini-api-key"}} {
		ag, _ := Get(pair.provider)
		native := ag.NativeCredentials()
		a, err := native.Broker.Seed(pair.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := native.Broker.Seed(pair.b)
		if err != nil {
			t.Fatal(err)
		}
		left, _ := json.Marshal(a.Files)
		right, _ := json.Marshal(b.Files)
		if !bytes.Equal(left, right) {
			t.Fatalf("%s rewrites shared native home on family switch", pair.provider)
		}
		if err := native.Broker.Check(a.Files, b); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodexNativeFamilySetup(t *testing.T) {
	controls := (codexAgent{}).UpdateControls()
	for _, file := range controls.Files {
		if file.Path != codexNativeCLISetup && file.Path != codexNativeACPSetup {
			continue
		}
		for _, family := range []string{"chatgpt", "apikey"} {
			// Source the exact embedded setup text, then observe argv/env without
			// starting a provider or authenticating a real account.
			script := "set -eu\nset -- app-server\n" + file.Content + "printf '%s\\n' \"$@\" \"${CODEX_PATH:-}\" \"${DEFAULT_AUTH_REQUEST:-}\"\n"
			command := exec.Command("sh", "-c", script)
			command.Env = []string{"PATH=/usr/bin:/bin", "COOP_NATIVE_CODEX_FAMILY=" + family}
			out, err := command.CombinedOutput()
			if err != nil {
				t.Fatal(err, string(out))
			}
			if family == "apikey" {
				want := "cli_auth_credentials_store=\"ephemeral\""
				if file.Path == codexNativeACPSetup {
					want = "/opt/coop/bin/codex\n{\"methodId\":\"api-key\"}"
				}
				if !strings.Contains(string(out), want) {
					t.Fatal("missing per-process family selector", string(out))
				}
			} else if strings.Contains(string(out), "ephemeral") || strings.Contains(string(out), "api-key") {
				t.Fatal("ChatGPT process switched family", string(out))
			}
		}
	}
}

func TestGeminiNativeFamilySettingsComposeThinkingAndUpdates(t *testing.T) {
	for _, family := range []string{"oauth-personal", "gemini-api-key"} {
		for _, effort := range []string{"", "low", "high"} {
			env, err := geminiBrokerEnv(family, effort)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, file := range (geminiAgent{}).UpdateControls().Files {
				if file.Path == env["GEMINI_CLI_SYSTEM_SETTINGS_PATH"] {
					found = true
					var doc map[string]any
					if err := json.Unmarshal([]byte(file.Content), &doc); err != nil {
						t.Fatal(err)
					}
					if doc["security"].(map[string]any)["auth"].(map[string]any)["selectedType"] != family {
						t.Fatal("wrong family")
					}
					if doc["general"].(map[string]any)["enableAutoUpdate"] != false {
						t.Fatal("update guard lost")
					}
					if effort != "" && doc["modelConfigs"] == nil {
						t.Fatal("thinking settings lost")
					}
				}
			}
			if !found {
				t.Fatal("system selector names missing image file")
			}
		}
	}
}

func TestGeminiEncryptedCacheChecksProviderServicesOnly(t *testing.T) {
	seed, err := seedGeminiBroker("oauth-personal")
	if err != nil {
		t.Fatal(err)
	}
	seed.StorageHostname, seed.StorageUsername = "coop-native-fixture", "node"
	key, err := scrypt.Key([]byte("gemini-cli-oauth"), []byte(seed.StorageHostname+"-node-gemini-cli"), 16384, 8, 1, 32)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, 16)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(doc any) []byte {
		t.Helper()
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		iv := bytes.Repeat([]byte{1}, 16)
		sealed := gcm.Seal(nil, iv, raw, nil)
		return []byte(hex.EncodeToString(iv) + ":" + hex.EncodeToString(sealed[len(sealed)-16:]) + ":" + hex.EncodeToString(sealed[:len(sealed)-16]))
	}
	credential := func(token map[string]string) string {
		data, _ := json.Marshal(map[string]any{"serverName": "main-account", "token": token})
		return string(data)
	}
	for _, test := range []struct {
		name  string
		doc   any
		valid bool
	}{
		{"MCP only", map[string]any{"mcp-server": map[string]string{"any": "unrelated MCP credential"}}, true},
		{"public provider", map[string]any{"gemini-cli-oauth": map[string]string{"main-account": credential(map[string]string{"accessToken": seed.Marker, "tokenType": "Bearer"})}}, true},
		{"real access", map[string]any{"gemini-cli-oauth": map[string]string{"main-account": credential(map[string]string{"accessToken": "real-local-access"})}}, false},
		{"refresh", map[string]any{"gemini-cli-oauth": map[string]string{"main-account": credential(map[string]string{"accessToken": seed.Marker, "refreshToken": "real-local-refresh"})}}, false},
		{"unknown account", map[string]any{"gemini-cli-oauth": map[string]string{"other": "{}"}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := cloneNativeFiles(seed.Files)
			files["gemini-credentials.json"] = encode(test.doc)
			before := bytes.Clone(files["gemini-credentials.json"])
			err := checkGeminiBroker(files, seed)
			if (err == nil) != test.valid {
				t.Fatal(err)
			}
			if !bytes.Equal(before, files["gemini-credentials.json"]) {
				t.Fatal("cache mutated")
			}
		})
	}
	data := encode(map[string]any{})
	wrong := seed
	wrong.StorageHostname = "different"
	if checkGeminiEncryptedProviderServices(data, wrong) == nil {
		t.Fatal("unknown storage identity accepted")
	}
	data[len(data)-1] ^= 1
	if checkGeminiEncryptedProviderServices(data, seed) == nil {
		t.Fatal("corrupt ciphertext accepted")
	}
}
