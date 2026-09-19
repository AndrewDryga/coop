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

// withoutThinking drops gemini's per-effort thinking mounts, so a test about an adapter's native
// config reads just that config. TestGeminiThinkingWiring owns the thinking mounts.
func withoutThinking(mounts []MCPMount) []MCPMount {
	var out []MCPMount
	for _, m := range mounts {
		if !strings.Contains(m.BoxPath, "/.coop-gemini/thinking/") {
			out = append(out, m)
		}
	}
	return out
}

// The thinking mapping is a reading of one client's internals — its family base names, its model
// list, its LOW/HIGH enum. Moving the locked client must send someone back to re-read them, so the
// version the mapping was captured on is pinned here, apart from the locked declaration it guards.
func TestGeminiThinkingIsQualifiedOnTheLockedClient(t *testing.T) {
	const qualified = "0.59.0"
	gemini, _ := Get("gemini")
	clients := gemini.LockedClients(ClientPlatform{OS: "linux", Architecture: "arm64", Libc: "glibc"})
	if len(clients) == 0 {
		t.Fatal("no locked Gemini client for linux/arm64/glibc: the tripwire has nothing to guard")
	}
	for _, client := range clients {
		if client.Version != qualified {
			t.Fatalf("locked Gemini CLI is %s but its thinking mapping was captured on %s: re-verify chat-base-3/"+
				"chat-base-2.5, geminiThinkingModels and the ThinkingLevel values against the new bundle "+
				"(.agent/kb/gemini-effort-thinking-settings.md), then move this pin", client.Version, qualified)
		}
	}
}

// Gemini's CLI never sees an effort, so the adapter is the only thing that can refuse one: a level
// Gemini 3 cannot think at (any Gemini target can land on a Gemini 3 model), or a model no family
// base reaches. Everything it accepts must name a model the thinking settings actually cover.
func TestGeminiEffortValidation(t *testing.T) {
	gemini, _ := Get("gemini")
	for _, ok := range []struct{ model, effort string }{
		{"", "low"}, {"", "high"}, {"", ""},
		{"gemini-2.5-pro", "low"}, {"gemini-3-pro-preview", "high"}, {"auto", "low"},
		{"flash", "high"}, {"gemini-3-flash", "low"}, {"gemma-4-31b-it", "high"},
		{"some-future-model", ""}, // no effort, nothing to carry
	} {
		if err := ValidateEffort(gemini, ok.model, ok.effort); err != nil {
			t.Errorf("ValidateEffort(gemini, %q, %q) = %v, want accepted", ok.model, ok.effort, err)
		}
	}
	for _, bad := range []struct{ model, effort, want string }{
		{"gemini-2.5-pro", "medium", "low or high"}, // the 2.5 fallback chain can reach Gemini 3
		{"", "xhigh", "low or high"},
		{"gemini-3.5-flash", "max", "low or high"},
		{"gemini-9-ultra", "high", "without an effort"},
		{"custom-tuned", "low", "without an effort"},
	} {
		err := ValidateEffort(gemini, bad.model, bad.effort)
		if err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("ValidateEffort(gemini, %q, %q) = %v, want an error mentioning %q", bad.model, bad.effort, err, bad.want)
		}
	}
	// Every adapter that judges its own effort accepts any level here, as before.
	for _, name := range Names() {
		if a, _ := Get(name); a.Effort().Validate == nil && ValidateEffort(a, "anything", "whatever") != nil {
			t.Errorf("%s has no validator but refused an effort", name)
		}
	}
}

// One system-settings file per effort, both family bases in it, mounted outside the account's own
// profile; the box's Gemini points at the one for this run's effort and nothing else.
func TestGeminiThinkingWiring(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node"}
	gemini, _ := Get("gemini")

	wiring, err := gemini.MCP(cfg, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"COOP_GEMINI_THINKING=/home/node/.coop-gemini/thinking"}; !slices.Equal(wiring.Env, want) {
		t.Errorf("gemini env without an effort = %v, want only the directory the arms choose from", wiring.Env)
	}
	files := map[string]string{}
	for _, m := range wiring.Mounts {
		if strings.HasPrefix(m.BoxPath, "/home/node/.coop-gemini/thinking/") {
			files[filepath.Base(m.BoxPath)] = m.Content
		} else if m.BoxPath != "/home/node/.gemini/settings.json" {
			t.Errorf("unexpected gemini mount %s", m.BoxPath)
		}
	}
	for effort, want := range map[string]struct {
		level  string
		budget float64
	}{"low": {"LOW", 1024}, "high": {"HIGH", 24576}} {
		content, ok := files[effort+".json"]
		if !ok {
			t.Errorf("no thinking settings mounted for %s: %v", effort, files)
			continue
		}
		var settings struct {
			General      map[string]any `json:"general"`
			ModelConfigs struct {
				CustomAliases map[string]struct {
					Extends     string `json:"extends"`
					ModelConfig struct {
						Model                 string `json:"model"`
						GenerateContentConfig struct {
							ThinkingConfig map[string]any `json:"thinkingConfig"`
						} `json:"generateContentConfig"`
					} `json:"modelConfig"`
				} `json:"customAliases"`
			} `json:"modelConfigs"`
		}
		if err := json.Unmarshal([]byte(content), &settings); err != nil {
			t.Fatalf("%s thinking settings are not JSON: %v\n%s", effort, err, content)
		}
		// This file replaces the image's system layer, so it carries that layer's update switch.
		if settings.General["enableAutoUpdate"] != false || settings.General["enableAutoUpdateNotification"] != false || len(settings.General) != 2 {
			t.Errorf("%s thinking settings drop the update switch: general = %v", effort, settings.General)
		}
		aliases := settings.ModelConfigs.CustomAliases
		gemini3, gemini25, flash := aliases["chat-base-3"], aliases["chat-base-2.5"], aliases["gemini-3-flash"]
		// The pinned client's own bases extend chat-base; a replacement that did not would lose
		// includeThoughts and the chat sampling defaults.
		if gemini3.Extends != "chat-base" || gemini3.ModelConfig.GenerateContentConfig.ThinkingConfig["thinkingLevel"] != want.level ||
			len(gemini3.ModelConfig.GenerateContentConfig.ThinkingConfig) != 1 {
			t.Errorf("%s chat-base-3 = %+v, want only thinkingLevel %s over chat-base", effort, gemini3, want.level)
		}
		if gemini25.Extends != "chat-base" || gemini25.ModelConfig.GenerateContentConfig.ThinkingConfig["thinkingBudget"] != want.budget ||
			len(gemini25.ModelConfig.GenerateContentConfig.ThinkingConfig) != 1 {
			t.Errorf("%s chat-base-2.5 = %+v, want only thinkingBudget %v over chat-base", effort, gemini25, want.budget)
		}
		if flash.Extends != "chat-base-3" || flash.ModelConfig.Model != "gemini-3-flash" || len(aliases) != 3 {
			t.Errorf("%s aliases = %+v, want the two bases plus gemini-3-flash on chat-base-3", effort, aliases)
		}
	}
	if len(files) != 2 {
		t.Errorf("thinking settings = %v, want exactly low and high", files)
	}

	cfg.SetActiveEffort("gemini", "high")
	wiring, err = gemini.MCP(cfg, "/workspace")
	if err != nil || !slices.Contains(wiring.Env, "GEMINI_CLI_SYSTEM_SETTINGS_PATH=/home/node/.coop-gemini/thinking/high.json") {
		t.Errorf("gemini with effort high = (%v, %v), want the box pointed at high.json", wiring.Env, err)
	}
	cfg.SetActiveEffort("gemini", "medium")
	if _, err := gemini.MCP(cfg, "/workspace"); err == nil {
		t.Error("an effort with no thinking settings must fail assembly, not run at the client default")
	}
}

// The consult and delegate arms re-choose per call: their own $effort picks its file, and a call
// without one keeps the box's. Run for real, because the quoting inside ${effort:+…} is the part
// that can go wrong — a directory with a space must survive as one assignment.
func TestGeminiEffortArmsChooseSettingsPerCall(t *testing.T) {
	gemini, _ := Get("gemini")
	for name, arm := range map[string]string{
		"consult fresh": gemini.ConsultFresh(), "consult resume": gemini.ConsultResume(), "delegate": gemini.DelegateExec(),
	} {
		if !strings.Contains(arm, geminiEffortEnv+"gemini ") {
			t.Errorf("gemini %s arm does not choose its thinking settings: %s", name, arm)
		}
	}
	bin := t.TempDir()
	stub := "#!/bin/sh\nprintf '%s|%s\\n' \"${GEMINI_CLI_SYSTEM_SETTINGS_PATH-unset}\" \"$*\"\n"
	if err := os.WriteFile(filepath.Join(bin, "gemini"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(effort string, env ...string) string {
		t.Helper()
		// The arm reads the wrapper's shell variables; the environment seeds them in a -c shell.
		cmd := exec.Command("sh", "-c", gemini.DelegateExec())
		cmd.Env = append([]string{
			"PATH=" + bin + ":" + os.Getenv("PATH"), "COOP_GEMINI_THINKING=/home/a b/thinking",
			"effort=" + effort, "model=gemini-2.5-pro", "prompt=do it",
		}, env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("delegate arm: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got, want := run("low"), "/home/a b/thinking/low.json|--yolo --model gemini-2.5-pro -o stream-json -p do it"; got != want {
		t.Errorf("delegate with effort low ran %q, want %q", got, want)
	}
	if got, want := run("", "GEMINI_CLI_SYSTEM_SETTINGS_PATH=/box/high.json"), "/box/high.json|--yolo --model gemini-2.5-pro -o stream-json -p do it"; got != want {
		t.Errorf("delegate without an effort ran %q, want the box's settings kept: %q", got, want)
	}
	if got, want := run(""), "unset|--yolo --model gemini-2.5-pro -o stream-json -p do it"; got != want {
		t.Errorf("delegate with no effort anywhere ran %q, want no settings: %q", got, want)
	}
}
