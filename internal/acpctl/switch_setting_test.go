package acpctl

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

const switchSessionNew = `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1","configOptions":[` +
	`{"id":"model","type":"select","currentValue":"opus[1m]","options":[{"value":"opus[1m]","name":"Opus"},{"value":"sonnet","name":"Sonnet"}]},` +
	`{"id":"effort","type":"select","currentValue":"high","options":[{"value":"high","name":"High"},{"value":"max","name":"Max"}]}]}}` + "\n"

func switchSet(configID, value string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":7,"method":"session/set_config_option","params":{"sessionId":"s1","configId":"` +
		configID + `","value":"` + value + `"}}` + "\n")
}

// A model or effort picked while a switch has no box is kept, acknowledged at once with the pick
// shown, and applied by the replacement's sessionReady.
func TestACPControlSwitchSettingKeepsTheOfferedPick(t *testing.T) {
	c := newTestControl(t)
	toEd(c, []byte(switchSessionNew))

	for _, set := range [][]byte{switchSet("model", "sonnet"), switchSet("effort", "max")} {
		answer := c.switchSetting(set)
		if !strings.Contains(string(answer), `"id":7`) || !strings.Contains(string(answer), `"result"`) {
			t.Fatalf("the editor must get its answer now, got %s", answer)
		}
	}
	var answer struct {
		Result struct {
			ConfigOptions []struct {
				ID           string `json:"id"`
				CurrentValue string `json:"currentValue"`
			} `json:"configOptions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(c.switchSetting(switchSet("model", "sonnet")), &answer); err != nil {
		t.Fatal(err)
	}
	shown := map[string]string{}
	for _, option := range answer.Result.ConfigOptions {
		shown[option.ID] = option.CurrentValue
	}
	if shown["model"] != "sonnet" || shown["effort"] != "max" {
		t.Errorf("the answer must show the picks, got %v", shown)
	}
	joined := string(bytes.Join(c.sessionReady("s1"), nil))
	if !strings.Contains(joined, `"configId":"model","sessionId":"s1","value":"sonnet"`) ||
		!strings.Contains(joined, `"configId":"effort","sessionId":"s1","value":"max"`) {
		t.Fatalf("the replacement box must get the kept pick:\n%s", joined)
	}
}

// Anything coop cannot apply safely keeps the normal "wait for the switch" answer: a value the
// toolbar never offered, an option that is not model or effort, a preset, or a toolbar that belongs
// to another provider than the one coming up.
func TestACPControlSwitchSettingRefusesWhatItCannotApply(t *testing.T) {
	cases := map[string]struct {
		set    []byte
		before func(*Control)
	}{
		"unoffered value":  {set: switchSet("model", "not-a-model")},
		"other option":     {set: switchSet("mode", "plan")},
		"coop option":      {set: switchSet(CoopPresetID, "frontier")},
		"preset":           {set: switchSet("model", "sonnet"), before: func(c *Control) { c.sel = Selection{Preset: "frontier"} }},
		"another provider": {set: switchSet("model", "sonnet"), before: func(c *Control) { c.retargetLocked("codex") }},
		"unknown session": {set: []byte(`{"jsonrpc":"2.0","id":7,"method":"session/set_config_option",` +
			`"params":{"sessionId":"s9","configId":"model","value":"sonnet"}}`)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestControl(t)
			toEd(c, []byte(switchSessionNew))
			if tc.before != nil {
				tc.before(c)
			}
			model := c.target.Model
			if answer := c.switchSetting(tc.set); answer != nil {
				t.Fatalf("must leave the request to the normal path, got %s", answer)
			}
			if c.target.Model != model {
				t.Fatalf("a refused pick changed the target model to %q", c.target.Model)
			}
		})
	}
}

// The other adapter shapes: a models-only lead takes the pick through session/set_model, Codex keeps
// its reasoning_effort, and a lead whose model only rides the launch (Grok) or a pair the next
// launch refuses keeps the retry answer instead of a dropdown that lies.
func TestACPControlSwitchSettingFollowsEachAdapter(t *testing.T) {
	t.Run("gemini models field", func(t *testing.T) {
		c := newGeminiControl(t, "")
		toEd(c, []byte(geminiSessionNew))
		set := []byte(`{"jsonrpc":"2.0","id":7,"method":"session/set_config_option","params":{"sessionId":"g1","configId":"model","value":"gemini-2.5-flash"}}`)
		if c.switchSetting(set) == nil {
			t.Fatal("an offered Gemini model must be kept")
		}
		if joined := string(bytes.Join(c.sessionReady("g1"), nil)); !strings.Contains(joined, `"session/set_model"`) ||
			!strings.Contains(joined, `"modelId":"gemini-2.5-flash"`) {
			t.Fatalf("the replacement must set the kept Gemini model:\n%s", joined)
		}
	})
	t.Run("codex reasoning effort", func(t *testing.T) {
		c := New(&config.Config{ConfigDir: t.TempDir()}, "codex", "gpt-5", "", t.TempDir(), Selection{}, nil, nil, testHost())
		toEd(c, []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1","configOptions":[`+
			`{"id":"model","type":"select","currentValue":"gpt-5","options":[{"value":"gpt-5","name":"GPT-5"}]},`+
			`{"id":"reasoning_effort","type":"select","currentValue":"low","options":[{"value":"low","name":"Low"},{"value":"high","name":"High"}]}]}}`+"\n"))
		if c.switchSetting(switchSet("reasoning_effort", "high")) == nil {
			t.Fatal("an offered Codex effort must be kept")
		}
		if joined := string(bytes.Join(c.sessionReady("s1"), nil)); !strings.Contains(joined, `"configId":"reasoning_effort","sessionId":"s1","value":"high"`) {
			t.Fatalf("the replacement must set the kept effort:\n%s", joined)
		}
	})
	t.Run("grok launch-only model", func(t *testing.T) {
		c := New(&config.Config{ConfigDir: t.TempDir()}, "grok", "grok-4.5", "", t.TempDir(), Selection{}, nil, nil, testHost())
		toEd(c, []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1","models":{"currentModelId":"grok-4.5","availableModels":[{"modelId":"grok-4.5"},{"modelId":"grok-4.6"}]}}}`+"\n"))
		if answer := c.switchSetting(switchSet("model", "grok-4.6")); answer != nil || c.target.Model != "grok-4.5" {
			t.Fatalf("a model only the launch sets must not be kept: answer=%s target=%q", answer, c.target.Model)
		}
	})
	t.Run("pair the launch refuses", func(t *testing.T) {
		c := newTestControl(t)
		c.target.Effort = "high"
		toEd(c, []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1","configOptions":[`+
			`{"id":"model","type":"select","currentValue":"opus[1m]","options":[{"value":"opus[1m]","name":"Opus"},{"value":"claude-haiku-4-5","name":"Haiku 4.5"}]}]}}`+"\n"))
		if answer := c.switchSetting(switchSet("model", "claude-haiku-4-5")); answer != nil || c.target.Model != "opus[1m]" {
			t.Fatalf("a model that cannot carry the effort must not be kept: answer=%s target=%q", answer, c.target.Model)
		}
	})
}
