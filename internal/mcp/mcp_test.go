package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func writeTmp(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const sample = `{
  "mcpServers": {
    "ctx7":   { "command": "npx", "args": ["-y", "@upstash/context7-mcp"], "env": { "K": "v" } },
    "sentry": { "type": "http", "url": "https://mcp.sentry.dev/mcp" }
  }
}`

func TestGenerateCodex(t *testing.T) {
	got, err := GenerateCodex(writeTmp(t, "mcp.json", sample), "")
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic, sorted-by-name output — lock the exact format.
	want := `[mcp_servers.ctx7]
command = "npx"
args = ["-y", "@upstash/context7-mcp"]

[mcp_servers.ctx7.env]
K = "v"

[mcp_servers.sentry]
url = "https://mcp.sentry.dev/mcp"
`
	if got != want {
		t.Errorf("GenerateCodex mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestGenerateCodexPreservesExistingConfig(t *testing.T) {
	existing := writeTmp(t, "config.toml",
		"model = \"o3\"\n\n[mcp_servers.stale]\ncommand = \"gone\"\n\n[mcp_servers_backup]\nnote = \"keep me\"\n")
	got, err := GenerateCodex(writeTmp(t, "mcp.json", sample), existing)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `model = "o3"`) {
		t.Error("non-mcp config should be preserved")
	}
	if strings.Contains(got, "stale") {
		t.Error("the user's own [mcp_servers.*] should be replaced, not kept")
	}
	// A lookalike table whose name merely starts with "mcp_servers" is NOT an MCP table and must survive.
	if !strings.Contains(got, "[mcp_servers_backup]") || !strings.Contains(got, "keep me") {
		t.Errorf("a [mcp_servers_backup] table must be preserved (only real [mcp_servers.*] tables are stripped):\n%s", got)
	}
	if !strings.Contains(got, "[mcp_servers.ctx7]") {
		t.Error("shared servers should be present")
	}
}

func TestGenerateCodexRetainsNativeBytesAndRemovesCanonicalMCP(t *testing.T) {
	retained := " \t# keep exact CRLF\r\nmodel = \"o3\"\r\n\r\n[other]\r\nthreshold = nan\r\nlimit = +inf\r\nwhen = 1979-05-27T07:32:00Z\r\n"
	existingBody := " \t# keep exact CRLF\r\nmodel = \"o3\"\r\n\r\n[mcp_servers.\"stale.name\"]\r\ncommand = \"gone\"\r\n\r\n" +
		"[mcp_servers.\"stale.name\".env]\r\nK = \"gone\"\r\n\r\n[other]\r\nthreshold = nan\r\nlimit = +inf\r\nwhen = 1979-05-27T07:32:00Z\r\n"
	existing := writeTmp(t, "config.toml", existingBody)
	got, err := GenerateCodex(writeTmp(t, "mcp.json", sample), existing)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, retained) {
		t.Fatalf("retained native bytes changed:\n--- got ---\n%q\n--- prefix ---\n%q", got, retained)
	}
	if strings.Contains(got, "stale.name") || !strings.Contains(got, "[mcp_servers.ctx7]") {
		t.Fatalf("canonical native MCP was not replaced by shared MCP:\n%s", got)
	}
	if after, err := os.ReadFile(existing); err != nil || !bytes.Equal(after, []byte(existingBody)) {
		t.Fatalf("host native config changed = (%q, %v)", after, err)
	}
}

func TestGenerateCodexRejectsAlternateNativeMCPForms(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "dotted bare key", body: `mcp_servers.stale.command = "gone"`},
		{name: "dotted quoted key", body: `"mcp_servers".stale.command = "gone"`},
		{name: "dotted literal key", body: `'mcp_servers'.stale.command = "gone"`},
		{name: "inline table", body: `mcp_servers = { stale = { command = "gone" } }`},
		{name: "quoted header", body: `["mcp_servers".stale]\ncommand = "gone"`},
		{name: "spaced header", body: `[ mcp_servers . stale ]\ncommand = "gone"`},
		{name: "array table", body: `[[mcp_servers.stale]]\ncommand = "gone"`},
		{name: "header text inside multiline string", body: "note = '''\n[mcp_servers.fake]\nstill text\n'''\n\n[mcp_servers.stale]\ncommand = \"gone\"\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.ReplaceAll(tc.body, `\n`, "\n")
			existing := writeTmp(t, "config.toml", body)
			_, err := GenerateCodex(writeTmp(t, "mcp.json", sample), existing)
			if err == nil || !strings.Contains(err.Error(), "cannot safely remove") || !strings.Contains(err.Error(), "COOP_MCP_FILE") {
				t.Fatalf("GenerateCodex error = %v, want unsupported native MCP refusal", err)
			}
			if after, readErr := os.ReadFile(existing); readErr != nil || !bytes.Equal(after, []byte(body)) {
				t.Fatalf("denied host config changed = (%q, %v), want %q", after, readErr, body)
			}
		})
	}
}

func TestGenerateCodexPreservesMCPLookalikesVerbatim(t *testing.T) {
	existingBody := "note = '''\n[mcp_servers.fake]\nstill text\n'''\n\nmcp_servers_backup = { command = \"keep\" }\n[mcp_serverssettings]\nnote = \"keep too\"\n"
	got, err := GenerateCodex(writeTmp(t, "mcp.json", sample), writeTmp(t, "config.toml", existingBody))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, existingBody) {
		t.Fatalf("lookalike native bytes changed:\n--- got ---\n%q\n--- prefix ---\n%q", got, existingBody)
	}
}

func TestGenerateCodexRejectsInvalidNativeConfigEntries(t *testing.T) {
	mcpFile := writeTmp(t, "mcp.json", sample)
	t.Run("malformed", func(t *testing.T) {
		path := writeTmp(t, "config.toml", `broken = {`)
		if _, err := GenerateCodex(mcpFile, path); err == nil || !strings.Contains(err.Error(), "not valid TOML") {
			t.Fatalf("GenerateCodex malformed error = %v", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		path := t.TempDir()
		if _, err := GenerateCodex(mcpFile, path); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("GenerateCodex directory error = %v", err)
		}
	})
	t.Run("dangling symlink", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
			t.Fatal(err)
		}
		if _, err := GenerateCodex(mcpFile, path); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("GenerateCodex dangling symlink error = %v", err)
		}
	})
	t.Run("outside-root readable symlink", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "secret.toml")
		if err := os.WriteFile(target, []byte("model = \"o3\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if got, err := GenerateCodex(mcpFile, path); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("GenerateCodex readable symlink = (%q, %v), want refusal", got, err)
		}
		if after, err := os.ReadFile(target); err != nil || string(after) != "model = \"o3\"\n" {
			t.Fatalf("symlink target changed = (%q, %v)", after, err)
		}
	})
	t.Run("fifo does not block", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := GenerateCodex(mcpFile, path); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("GenerateCodex fifo error = %v", err)
		}
	})
	t.Run("regular entry replaced before open", func(t *testing.T) {
		path := writeTmp(t, "config.toml", "model = \"o3\"\n")
		_, err := readNativeConfigWith(path, func(path string) (*os.File, error) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		}, io.ReadAll)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("readNativeConfigWith replacement error = %v, want descriptor refusal", err)
		}
	})
	t.Run("regular entry replaced by symlink before open", func(t *testing.T) {
		path := writeTmp(t, "config.toml", "model = \"o3\"\n")
		target := writeTmp(t, "secret.toml", "model = \"secret\"\n")
		_, err := readNativeConfigWith(path, func(path string) (*os.File, error) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		}, io.ReadAll)
		if err == nil || !strings.Contains(err.Error(), "open native MCP config") {
			t.Fatalf("readNativeConfigWith symlink replacement error = %v, want no-follow refusal", err)
		}
	})
	t.Run("opened regular file read error", func(t *testing.T) {
		path := writeTmp(t, "config.toml", "model = \"o3\"\n")
		sentinel := errors.New("fixture read failure")
		_, err := readNativeConfigWith(path, func(path string) (*os.File, error) {
			return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		}, func(io.Reader) ([]byte, error) {
			return nil, sentinel
		})
		if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "read native MCP config") {
			t.Fatalf("readNativeConfigWith read error = %v, want wrapped sentinel", err)
		}
	})
	t.Run("opened regular file exceeds limit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, make([]byte, maxMCPConfigBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := GenerateCodex(mcpFile, path); err == nil || !strings.Contains(err.Error(), "exceeds the 4194304-byte limit") {
			t.Fatalf("GenerateCodex oversized native error = %v", err)
		}
	})
	t.Run("regular file grows past limit after stat", func(t *testing.T) {
		path := writeTmp(t, "config.toml", "model = \"o3\"\n")
		_, err := readNativeConfigWith(path, func(path string) (*os.File, error) {
			return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
		}, func(reader io.Reader) ([]byte, error) {
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return nil, err
			}
			if _, err := file.Write(make([]byte, maxMCPConfigBytes)); err != nil {
				file.Close()
				return nil, err
			}
			if err := file.Close(); err != nil {
				return nil, err
			}
			return io.ReadAll(reader)
		})
		if err == nil || !strings.Contains(err.Error(), "exceeds the 4194304-byte limit") {
			t.Fatalf("readNativeConfigWith grown file error = %v", err)
		}
	})
	t.Run("open failure", func(t *testing.T) {
		path := writeTmp(t, "config.toml", "model = \"o3\"\n")
		sentinel := errors.New("fixture permission failure")
		_, err := readNativeConfigWith(path, func(string) (*os.File, error) {
			return nil, sentinel
		}, io.ReadAll)
		if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "open native MCP config") {
			t.Fatalf("readNativeConfigWith open error = %v, want wrapped sentinel", err)
		}
	})
	t.Run("initially missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.toml")
		got, err := GenerateCodex(mcpFile, path)
		if err != nil || !strings.Contains(got, "[mcp_servers.ctx7]") {
			t.Fatalf("GenerateCodex missing = (%q, %v)", got, err)
		}
	})
}

// A numeric MCP env value (JSON numbers decode to float64) must render as plain digits, never
// scientific notation — "8080", not "8.08e+03"; a big value not "1.23e+19".
func TestGenerateCodexNumericEnvNoScientificNotation(t *testing.T) {
	mcp := `{"mcpServers":{"s":{"command":"x","env":{"PORT":8080,"BIG":12345678901234567890}}}}`
	got, err := GenerateCodex(writeTmp(t, "mcp.json", mcp), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `PORT = "8080"`) {
		t.Errorf(`PORT should render as "8080":\n%s`, got)
	}
	if strings.Contains(got, "e+") || strings.Contains(got, "E+") {
		t.Errorf("numeric env must not render in scientific notation:\n%s", got)
	}
}

// A server name with a dot/space, and an env value with a control char, must produce VALID TOML:
// the name is quoted (else a dot nests the table / a space breaks the parse) and \n is escaped.
func TestGenerateCodexQuotesNonBareNamesAndEscapes(t *testing.T) {
	mcp := `{"mcpServers":{"my.server":{"command":"x","env":{"K":"a\nb"}}}}`
	got, err := GenerateCodex(writeTmp(t, "mcp.json", mcp), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `[mcp_servers."my.server"]`) {
		t.Errorf("a dotted server name must be quoted (else it nests the table):\n%s", got)
	}
	if strings.Contains(got, "a\nb") || !strings.Contains(got, `K = "a\nb"`) {
		t.Errorf("a control char in an env value must be escaped (no raw newline):\n%s", got)
	}
}

func TestGenerateGeminiMerge(t *testing.T) {
	existingBody := `{
		"theme":"dark",
		"context":{
			"includeDirectories":["src"],
			"fileFiltering":{"respectGitIgnore":true,"respectGeminiIgnore":true}
		},
		"mcpServers":{"old":{"command":"true"}}
	}`
	existing := writeTmp(t, "settings.json", existingBody)
	got, err := GenerateGemini(writeTmp(t, "mcp.json", sample), existing)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Theme      string                    `json:"theme"`
		MCPServers map[string]map[string]any `json:"mcpServers"`
		Context    struct {
			IncludeDirectories []string       `json:"includeDirectories"`
			FileFiltering      map[string]any `json:"fileFiltering"`
		} `json:"context"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got)
	}
	if out.Theme != "dark" {
		t.Error("existing top-level setting (theme) must be preserved")
	}
	if len(out.Context.IncludeDirectories) != 1 || out.Context.IncludeDirectories[0] != "src" {
		t.Errorf("existing nested setting must be preserved: %+v", out.Context.IncludeDirectories)
	}
	if got := out.Context.FileFiltering["respectGeminiIgnore"]; got != true {
		t.Errorf("existing file-filtering setting must be preserved, got %v", got)
	}
	if got, ok := out.Context.FileFiltering["respectGitIgnore"]; !ok || got != false {
		t.Errorf("context.fileFiltering.respectGitIgnore = %v, %v; want false, true", got, ok)
	}
	for _, name := range []string{"old", "ctx7", "sentry"} {
		if _, ok := out.MCPServers[name]; !ok {
			t.Errorf("server %q missing from merged settings", name)
		}
	}
	if after, err := os.ReadFile(existing); err != nil {
		t.Fatal(err)
	} else if string(after) != existingBody {
		t.Error("generating box settings must not mutate the user's host settings")
	}
}

func TestGenerateGeminiWithoutMCP(t *testing.T) {
	existingBody := `{"theme":"dark","context":{"includeDirectories":["src"],"fileFiltering":{"respectGitIgnore":true,"custom":"keep"}}}`
	existing := writeTmp(t, "settings.json", existingBody)
	got, err := GenerateGemini("", existing)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Theme      string          `json:"theme"`
		MCPServers json.RawMessage `json:"mcpServers"`
		Context    struct {
			IncludeDirectories []string       `json:"includeDirectories"`
			FileFiltering      map[string]any `json:"fileFiltering"`
		} `json:"context"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got)
	}
	if out.Theme != "dark" || len(out.Context.IncludeDirectories) != 1 || out.Context.IncludeDirectories[0] != "src" {
		t.Errorf("existing settings were not preserved: %+v", out)
	}
	if got := out.Context.FileFiltering["custom"]; got != "keep" {
		t.Errorf("existing file-filtering setting must be preserved, got %v", got)
	}
	if got, ok := out.Context.FileFiltering["respectGitIgnore"]; !ok || got != false {
		t.Errorf("context.fileFiltering.respectGitIgnore = %v, %v; want false, true", got, ok)
	}
	if out.MCPServers != nil {
		t.Errorf("settings-only generation must not add mcpServers: %s", out.MCPServers)
	}
	if after, err := os.ReadFile(existing); err != nil {
		t.Fatal(err)
	} else if string(after) != existingBody {
		t.Error("generating box settings must not mutate the user's host settings")
	}
}

func TestGenerateMalformed(t *testing.T) {
	if _, err := GenerateCodex(writeTmp(t, "mcp.json", "{not json"), ""); err == nil {
		t.Error("malformed mcp.json should error")
	}
	if _, err := GenerateGemini(filepath.Join(t.TempDir(), "missing.json"), ""); err == nil {
		t.Error("missing mcp.json should error")
	}
}

func TestGenerateEmpty(t *testing.T) {
	got, err := GenerateCodex(writeTmp(t, "mcp.json", `{"mcpServers":{}}`), "")
	if err != nil || got != "" {
		t.Errorf("empty servers -> empty codex output; got %q err %v", got, err)
	}
}

func TestReadValidatedSnapshotPreservesExactBytesAndInertStates(t *testing.T) {
	raw := " \n{\"mcpServers\":{\"exact\":{\"command\":\"true\"}}}\n"
	snapshot, active, err := ReadValidatedSnapshot(writeTmp(t, "mcp.json", raw))
	if err != nil || !active || string(snapshot) != raw {
		t.Fatalf("validated snapshot = (%q, %v, %v), want exact active bytes", snapshot, active, err)
	}

	for _, path := range []string{"", filepath.Join(t.TempDir(), "missing.json"), writeTmp(t, "empty.json", `{"mcpServers":{}}`)} {
		snapshot, active, err := ReadValidatedSnapshot(path)
		if err != nil || active || snapshot != nil {
			t.Errorf("inert snapshot %q = (%q, %v, %v), want nil, false, nil", path, snapshot, active, err)
		}
	}
	if _, _, err := ReadValidatedSnapshot(writeTmp(t, "malformed.json", "{not json")); err == nil {
		t.Error("a present malformed MCP snapshot must fail closed")
	}
}

func TestReadValidatedSnapshotRejectsUnsafeHostFiles(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		target := writeTmp(t, "secret.json", `{"mcpServers":{"secret":{"command":"true"}}}`)
		path := filepath.Join(t.TempDir(), "mcp.json")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ReadValidatedSnapshot(path); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("ReadValidatedSnapshot symlink error = %v", err)
		}
	})
	t.Run("fifo", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp.json")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ReadValidatedSnapshot(path); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("ReadValidatedSnapshot fifo error = %v", err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp.json")
		if err := os.WriteFile(path, make([]byte, maxMCPConfigBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ReadValidatedSnapshot(path); err == nil || !strings.Contains(err.Error(), "exceeds the 4194304-byte limit") {
			t.Fatalf("ReadValidatedSnapshot oversized error = %v", err)
		}
	})
}

func TestConfigReadDistinguishesInitialAbsenceFromPostObservationRemoval(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	if data, present, err := readConfigFile(missing, "shared MCP config"); err != nil || present || data != nil {
		t.Fatalf("initial absence = (%q, %v, %v), want nil, false, nil", data, present, err)
	}

	path := writeTmp(t, "mcp.json", `{"mcpServers":{"x":{"command":"true"}}}`)
	data, present, err := readConfigFileWith(path, "shared MCP config", func(path string) (*os.File, error) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	}, io.ReadAll)
	if err == nil || !errors.Is(err, os.ErrNotExist) || present || data != nil {
		t.Fatalf("post-observation removal = (%q, %v, %v), want propagated not-exist error", data, present, err)
	}
}

// A present-but-malformed existing gemini settings file must error (so box.Run skips wiring and
// gemini keeps its real config), not silently produce a settings.json containing only mcpServers.
func TestGenerateGeminiMalformedExistingErrors(t *testing.T) {
	mcpFile := writeTmp(t, "mcp.json", sample)
	if _, err := GenerateGemini(mcpFile, writeTmp(t, "settings.json", `{"theme":"dark", oops`)); err == nil {
		t.Error("malformed existing settings.json should error, not discard the user's settings")
	}
	// Missing/empty existing is fine — nothing to merge onto.
	if _, err := GenerateGemini(mcpFile, filepath.Join(t.TempDir(), "nope.json")); err != nil {
		t.Errorf("missing existing settings should be ok, got %v", err)
	}
	if _, err := GenerateGemini(mcpFile, writeTmp(t, "empty.json", "  \n")); err != nil {
		t.Errorf("empty existing settings should be ok, got %v", err)
	}
	for _, tc := range []struct {
		name string
		make func(*testing.T) string
		want string
	}{
		{
			name: "symlink",
			make: func(t *testing.T) string {
				target := writeTmp(t, "outside-settings.json", `{"theme":"dark"}`)
				path := filepath.Join(t.TempDir(), "settings.json")
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				return path
			},
			want: "symbolic link",
		},
		{
			name: "fifo",
			make: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "settings.json")
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
			want: "not a regular file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := GenerateGemini(mcpFile, tc.make(t)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("GenerateGemini existing %s error = %v, want %q", tc.name, err, tc.want)
			}
		})
	}
}

// A server with neither command nor url is skipped, not emitted as a bodyless table that would
// break Codex's whole config parse.
func TestGenerateCodexSkipsTransportlessServer(t *testing.T) {
	got, err := GenerateCodex(writeTmp(t, "mcp.json", `{"mcpServers":{"good":{"command":"x"},"broken":{}}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "broken") {
		t.Errorf("transport-less server should be skipped, got:\n%s", got)
	}
	if !strings.Contains(got, "[mcp_servers.good]") {
		t.Errorf("valid server should remain, got:\n%s", got)
	}
}

// A canonical HTTP/SSE server ({type, url, headers}) passes through to gemini's settings.json
// verbatim — gemini honors that exact shape (verified against `gemini mcp add -t http/-t sse`), so
// the raw passthrough is gemini-native. Locked in so a future GenerateGemini refactor can't break it.
func TestGenerateGeminiHTTPPassthrough(t *testing.T) {
	src := `{ "mcpServers": {
		"emisar": { "type": "http", "url": "https://emisar.dev/api/mcp/rpc", "headers": { "Authorization": "Bearer emk-x" } },
		"legacy": { "type": "sse",  "url": "https://legacy.example/sse",      "headers": { "X-Api-Key": "k" } }
	} }`
	got, err := GenerateGemini(writeTmp(t, "mcp.json", src), "")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(got), &f); err != nil {
		t.Fatalf("gemini settings not valid JSON: %v\n%s", err, got)
	}
	em := f.MCPServers["emisar"]
	if em["type"] != "http" || em["url"] != "https://emisar.dev/api/mcp/rpc" {
		t.Errorf("emisar type/url not preserved for gemini: %+v", em)
	}
	if h, ok := em["headers"].(map[string]any); !ok || h["Authorization"] != "Bearer emk-x" {
		t.Errorf("emisar headers not preserved for gemini: %+v", em["headers"])
	}
	if f.MCPServers["legacy"]["type"] != "sse" {
		t.Errorf("sse type not preserved for gemini: %+v", f.MCPServers["legacy"])
	}
}

// Codex has no inline-header support, so a url server with headers but no bearer_token_env_var would
// authenticate nowhere — GenerateCodex flags it rather than emit a silent unauthenticated url. A
// bearer_token_env_var (codex's real mechanism) emits cleanly, with no notice.
func TestGenerateCodexHTTPHeaders(t *testing.T) {
	src := `{ "mcpServers": {
		"hdronly": { "type": "http", "url": "https://a.example/mcp", "headers": { "Authorization": "Bearer x" } },
		"bearer":  { "type": "http", "url": "https://b.example/mcp", "bearer_token_env_var": "B_TOKEN" }
	} }`
	got, err := GenerateCodex(writeTmp(t, "mcp.json", src), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `bearer_token_env_var = "B_TOKEN"`) {
		t.Errorf("expected bearer_token_env_var for the bearer server:\n%s", got)
	}
	if n := strings.Count(got, "# coop: codex can't use"); n != 1 {
		t.Errorf("the header-gap notice should fire exactly once (only the headers-only server), got %d:\n%s", n, got)
	}
}

// HTTP header names are case-insensitive. Allowing two spellings, or synthesizing an
// Authorization header beside an inline one, gives providers different credentials for the same
// server. The shared mcp.json boundary rejects both shapes before any renderer can diverge.
func TestMCPConsumersRejectAmbiguousAuthorization(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "case-insensitive duplicate headers",
			src:  `{"mcpServers":{"dupe":{"url":"https://example.test/mcp","headers":{"X-Api-Key":"one","x-api-key":"two"}}}}`,
			want: []string{`MCP server "dupe"`, "duplicate case-insensitive headers", `"X-Api-Key"`, `"x-api-key"`},
		},
		{
			name: "inline and environment authorization",
			src:  `{"mcpServers":{"auth":{"url":"https://example.test/mcp","headers":{"aUtHoRiZaTiOn":"Bearer inline"},"bearer_token_env_var":"TOKEN"}}}`,
			want: []string{`MCP server "auth"`, `"aUtHoRiZaTiOn"`, "bearer_token_env_var", "one Authorization source"},
		},
		{
			name: "duplicate exact JSON header",
			src:  `{"mcpServers":{"auth":{"url":"https://example.test/mcp","headers":{"Authorization":"Bearer one","Authorization":"Bearer two"}}}}`,
			want: []string{`duplicate JSON object key "Authorization"`, "root.mcpServers.auth.headers"},
		},
		{
			name: "noncanonical field alias cannot overwrite auth",
			src:  `{"mcpServers":{"auth":{"url":"https://example.test/mcp","headers":{"Authorization":"Bearer inline"},"bearer_token_env_var":"TOKEN","Headers":{}}}}`,
			want: []string{`MCP server "auth" field "Headers"`, `canonical spelling "headers"`},
		},
		{
			name: "noncanonical root alias",
			src:  `{"MCPServers":{"auth":{"command":"true"}}}`,
			want: []string{`MCP root key "MCPServers"`, `canonical spelling "mcpServers"`},
		},
		{
			name: "invalid UTF-8",
			src:  "{\"mcpServers\":{\"bad\":{\"command\":\"\xff\"}}}",
			want: []string{"invalid UTF-8"},
		},
	}
	consumers := []struct {
		name string
		run  func(string) error
	}{
		{"Codex", func(path string) error { _, err := GenerateCodex(path, ""); return err }},
		{"Gemini", func(path string) error { _, err := GenerateGemini(path, ""); return err }},
		{"ACP", func(path string) error { _, err := ACPServers(path, os.LookupEnv); return err }},
		{"box snapshot", func(path string) error { _, _, err := ReadValidatedSnapshot(path); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeTmp(t, "mcp.json", test.src)
			for _, consumer := range consumers {
				t.Run(consumer.name, func(t *testing.T) {
					err := consumer.run(path)
					if err == nil {
						t.Fatal("ambiguous authorization should be rejected")
					}
					for _, want := range test.want {
						if !strings.Contains(err.Error(), want) {
							t.Errorf("error %q does not contain %q", err, want)
						}
					}
				})
			}
		})
	}
}

// The ACP adapter reads a shape mcp.json does not have: name/value PAIR LISTS for headers and
// env, "type" present on a remote server and ABSENT on a stdio one (it decides which arm to take
// by that key alone, so a stdio server that declares "stdio" is dropped on the floor).
func TestACPServersRenderTheShapeTheClaudeAdapterReads(t *testing.T) {
	env := map[string]string{"EMISAR_API_KEY": "emk-x"}
	lookup := func(key string) (string, bool) { v, ok := env[key]; return v, ok }
	for name, expect := range map[string]struct{ src, want string }{
		"bearer token becomes an Authorization header": {
			`{"mcpServers":{"emisar":{"type":"http","url":"https://emisar.dev/api/mcp/rpc","bearer_token_env_var":"EMISAR_API_KEY"}}}`,
			`[{"headers":[{"name":"Authorization","value":"Bearer emk-x"}],"name":"emisar","type":"http","url":"https://emisar.dev/api/mcp/rpc"}]`,
		},
		"explicit headers become a sorted pair list": {
			`{"mcpServers":{"h":{"type":"http","url":"https://a.example/mcp","headers":{"X-Api-Key":"k","Authorization":"Bearer inline"}}}}`,
			`[{"headers":[{"name":"Authorization","value":"Bearer inline"},{"name":"X-Api-Key","value":"k"}],"name":"h","type":"http","url":"https://a.example/mcp"}]`,
		},
		"sse keeps its transport": {
			`{"mcpServers":{"legacy":{"type":"sse","url":"https://legacy.example/sse"}}}`,
			`[{"name":"legacy","type":"sse","url":"https://legacy.example/sse"}]`,
		},
		"a url without a declared type is http": {
			`{"mcpServers":{"plain":{"url":"https://plain.example/mcp"}}}`,
			`[{"name":"plain","type":"http","url":"https://plain.example/mcp"}]`,
		},
		"stdio carries command, args and a sorted env pair list": {
			`{"mcpServers":{"ctx7":{"command":"npx","args":["-y","@upstash/context7-mcp"],"env":{"PORT":8080,"K":"v"}}}}`,
			`[{"args":["-y","@upstash/context7-mcp"],"command":"npx","env":[{"name":"K","value":"v"},{"name":"PORT","value":"8080"}],"name":"ctx7"}]`,
		},
		"a stdio server that declares its type does not keep it": {
			`{"mcpServers":{"s":{"type":"stdio","command":"x"}}}`,
			`[{"command":"x","name":"s"}]`,
		},
		"a server with no transport is skipped": {
			`{"mcpServers":{"broken":{},"good":{"command":"x"}}}`,
			`[{"command":"x","name":"good"}]`,
		},
		"no servers is an empty list": {`{"mcpServers":{}}`, `[]`},
	} {
		servers, err := ACPServers(writeTmp(t, "mcp.json", expect.src), lookup)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := json.Marshal(servers)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != expect.want {
			t.Errorf("%s:\n--- got ---\n%s\n--- want ---\n%s", name, got, expect.want)
		}
	}
}

// The adapter has no bearer_token_env_var — only inline headers — so a token the host cannot
// resolve cannot be sent at all. Handing over the url anyway would give the model a tool that
// answers every call with a 401, which it spends the turn retrying; the server is dropped instead.
func TestACPServersDropAServerWhoseBearerTokenCannotBeResolved(t *testing.T) {
	env := map[string]string{"PRESENT": "tok", "BLANK": "", "SPACES": "   "}
	src := `{"mcpServers":{
		"absent":  {"url":"https://a.example/mcp","bearer_token_env_var":"NOT_IN_ENV"},
		"blank":   {"url":"https://b.example/mcp","bearer_token_env_var":"BLANK"},
		"spaces":  {"url":"https://c.example/mcp","bearer_token_env_var":"SPACES"},
		"present": {"url":"https://d.example/mcp","bearer_token_env_var":"PRESENT"}
	}}`
	servers, err := ACPServers(writeTmp(t, "mcp.json", src), func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(servers)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"headers":[{"name":"Authorization","value":"Bearer tok"}],"name":"present","type":"http","url":"https://d.example/mcp"}]`
	if string(got) != want {
		t.Errorf("unauthenticatable servers were not dropped:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// A missing mcp.json is not an error here (unlike the file generators): an agent with no MCP still
// runs the turn. It must still be an empty JSON array — a null "mcpServers" is a different
// parameter to the adapter than a list with nothing in it.
func TestACPServersAreAnEmptyListWhenThereIsNoMCPFile(t *testing.T) {
	for name, path := range map[string]string{
		"missing file":   filepath.Join(t.TempDir(), "nope.json"),
		"no file at all": "",
	} {
		servers, err := ACPServers(path, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, _ := json.Marshal(servers); string(got) != "[]" {
			t.Errorf("%s: mcpServers = %s, want []", name, got)
		}
	}
}

// A present-but-malformed mcp.json is an error, so the caller can say the session lost its tools
// instead of starting one that quietly has none.
func TestACPServersRefuseAMalformedMCPFile(t *testing.T) {
	if _, err := ACPServers(writeTmp(t, "mcp.json", "{not json"), nil); err == nil {
		t.Error("malformed mcp.json should error")
	}
}
