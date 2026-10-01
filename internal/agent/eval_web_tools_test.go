package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestEvalWebToolsCommandsAndRoleShell(t *testing.T) {
	cleanCmdEnv(t)
	for _, tc := range []struct {
		provider string
		flags    []string
	}{
		{"claude", []string{"--disallowedTools", "WebSearch,WebFetch"}},
		{"codex", []string{"-c", "web_search=disabled"}},
		{"grok", []string{"--disable-web-search"}},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			a, _ := Get(tc.provider)
			for _, enabled := range []bool{false, true} {
				cfg := &config.Config{EvalDisableWebTools: enabled}
				cmds := [][]string{a.Interactive(cfg), a.Headless(cfg, "prompt")}
				for _, resume := range []bool{false, true} {
					cmd, ok := a.HeadlessSession(cfg, "prompt", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", resume)
					if !ok {
						t.Fatal("valid native session rejected")
					}
					cmds = append(cmds, cmd)
				}
				for _, cmd := range cmds {
					assertEvalWebFlags(t, cmd, tc.flags, enabled)
				}

				dir := t.TempDir()
				argsfile := filepath.Join(dir, "args")
				mustWrite(t, filepath.Join(dir, tc.provider), "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGSFILE\"\n")
				if err := os.Chmod(filepath.Join(dir, tc.provider), 0o755); err != nil {
					t.Fatal(err)
				}
				for _, fragment := range []string{a.ConsultFresh(), a.ConsultResume(), a.DelegateExec()} {
					script := a.UsagePrelude() + "\n" + tc.provider + "_run() { \"$@\"; }\n" +
						"prompt='prompt with spaces'; id=aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa; model=; effort=; " +
						"candidate_idfile=$ARGSFILE.id; codex_raw=/dev/null; " +
						"invoke() {\n" + fragment + "\n}\ninvoke\n"
					cmd := exec.Command("sh", "-c", script)
					policy := "0"
					if enabled {
						policy = "1"
					}
					cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "ARGSFILE="+argsfile,
						"COOP_EVAL_DISABLE_WEB_TOOLS="+policy, "COOP_MCP_ACTIVE=0", "COOP_RUN_ID=")
					if output, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("role shell: %v: %s", err, output)
					}
					data, err := os.ReadFile(argsfile)
					if err != nil {
						t.Fatal(err)
					}
					argv := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
					assertEvalWebFlags(t, argv, tc.flags, enabled)
					if !slices.Contains(argv, "prompt with spaces") {
						t.Fatalf("prompt argv was split: %q", argv)
					}
				}
			}
		})
	}
}

func assertEvalWebFlags(t *testing.T, argv, flags []string, enabled bool) {
	t.Helper()
	found := false
	for i := 0; i+len(flags) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(flags)], flags) {
			found = true
			break
		}
	}
	if found != enabled {
		t.Fatalf("eval policy %t: expected flags %q in %q", enabled, flags, argv)
	}
}

func TestEvalWebToolsGeminiPreservesSettingsAndEffort(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node"}
	settingsPath := filepath.Join(cfg.AgentDir("gemini"), "settings.json")
	original := `{"tools":{"exclude":["custom_tool","web_fetch"],"sandbox":false},"model":{"name":"gemini-3.5-flash"}}`
	mustWrite(t, settingsPath, original)
	a, _ := Get("gemini")
	for _, effort := range []string{"", "low", "high"} {
		cfg.SetActiveEffort("gemini", effort)
		for _, enabled := range []bool{false, true} {
			cfg.EvalDisableWebTools = enabled
			wiring, err := a.MCP(cfg, "/app")
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Tools struct {
					Exclude []string `json:"exclude"`
					Sandbox bool     `json:"sandbox"`
				} `json:"tools"`
				Model struct{ Name string } `json:"model"`
			}
			if err := json.Unmarshal([]byte(wiring.Mounts[0].Content), &got); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(got.Tools.Exclude, "custom_tool") || !slices.Contains(got.Tools.Exclude, "web_fetch") ||
				slices.Contains(got.Tools.Exclude, "google_web_search") != enabled || got.Tools.Sandbox || got.Model.Name != "gemini-3.5-flash" {
				t.Fatalf("policy %t lost settings: %+v", enabled, got)
			}
			if effort != "" && !slices.ContainsFunc(wiring.Env, func(env string) bool {
				return strings.HasPrefix(env, "GEMINI_CLI_SYSTEM_SETTINGS_PATH=") && strings.HasSuffix(env, "/thinking/"+effort+".json")
			}) {
				t.Fatalf("effort %s lost: %v", effort, wiring.Env)
			}
		}
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil || string(data) != original {
		t.Fatalf("host settings changed: %v", err)
	}
	for _, malformed := range []string{`{"tools":true}`, `{"tools":{"exclude":"web_fetch"}}`} {
		if _, err := ensureGeminiBoxDefaults(malformed, true); err == nil {
			t.Fatalf("malformed tools silently accepted: %s", malformed)
		}
	}
}
