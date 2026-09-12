package acpctl

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
)

func TestACPOnlyMeaningfulSelects(t *testing.T) {
	const native = `[{"id":"model","type":"select","currentValue":"only","options":[{"value":"only","name":"Only"}]},{"id":"effort","type":"select","currentValue":"high","options":[{"value":"high"}]},{"id":"fast","type":"boolean","currentValue":false},{"id":"choice","type":"select","currentValue":"a","options":[{"value":"a"},{"value":"b"}]},{"id":"different","type":"select","currentValue":"a","options":[{"value":"b"}]},{"id":"empty","type":"select","currentValue":"a","options":[]}]`
	for _, path := range []string{"new", "native_update", "replay", "coop_update", "selector_ack"} {
		t.Run(path, func(t *testing.T) {
			c := newTestControl(t)
			c.presets = nil
			c.creds = nil
			c.model = "only"
			initial := []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"S","configOptions":` + native + `}}` + "\n")
			out := toEd(c, initial)
			switch path {
			case "native_update":
				out = toEd(c, []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S","update":{"sessionUpdate":"config_option_update","configOptions":`+native+`}}}`+"\n"))
			case "replay":
				out = toEd(c, []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S","update":{"sessionUpdate":"config_option_update","coopReplay":true,"configOptions":[]}}}`+"\n"))
			case "coop_update":
				out = c.configOptionUpdate("S")
			case "selector_ack":
				out = c.ackOptions(json.RawMessage("2"), "S")
			}
			var envelope struct {
				Result struct {
					Options []struct{ ID string } `json:"configOptions"`
				} `json:"result"`
				Params struct {
					Update struct {
						Options []struct{ ID string } `json:"configOptions"`
					} `json:"update"`
				} `json:"params"`
			}
			if err := json.Unmarshal(out, &envelope); err != nil {
				t.Fatal(err)
			}
			options := append(envelope.Result.Options, envelope.Params.Update.Options...)
			var ids []string
			for _, option := range options {
				ids = append(ids, option.ID)
			}
			if want := []string{"fast", "choice", "different", "empty"}; !slices.Equal(ids, want) {
				t.Fatalf("editor options = %v, want only actionable controls %v", ids, want)
			}
			if !bytes.Contains(c.nativeCache["S"].Options, []byte(`"id":"model"`)) || !bytes.Contains(c.cached["S"], []byte(`"id":"model"`)) {
				t.Fatal("hiding the sole model discarded native target/replay truth")
			}
			if !bytes.HasSuffix(out, []byte("\n")) {
				t.Fatal("toolbar rewrite lost ACP line framing")
			}
		})
	}
}

func TestACPGroupedAndUnknownSelects(t *testing.T) {
	for _, tc := range []struct {
		name, options string
		hidden        bool
	}{
		{"flat", `[{"value":"a","name":"A","_meta":{"future":true}}]`, true},
		{"grouped", `[{"group":"g","name":"Group","options":[{"value":"a","name":"A"}]}]`, true},
		{"grouped choices", `[{"group":"g","name":"Group","options":[{"value":"a"},{"value":"b"}]}]`, false},
		{"two groups", `[{"group":"g","options":[{"value":"a"}]},{"group":"h","options":[{"value":"b"}]}]`, false},
		{"one leaf across groups", `[{"group":"g","options":[]},{"group":"h","options":[{"value":"a"}]},{"group":"i","options":[]}]`, true},
		{"empty group", `[{"group":"g","options":[]}]`, false},
		{"unknown", `[{"future":"a"}]`, false},
		{"ambiguous", `[{"value":"a","options":[{"value":"a"}]}]`, false},
		{"missing value", `[{"name":"A"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(`[{"id":"custom","type":"select","currentValue":"a","options":` + tc.options + `,"_meta":{"keep":true}}]`)
			got := visibleConfigOptions(raw)
			if tc.hidden {
				if string(got) != "[]" {
					t.Fatalf("inert select retained: %s", got)
				}
				return
			}
			if !bytes.Equal(got, raw) {
				t.Fatalf("actionable or unknown option changed: %s", got)
			}
		})
	}
}

func TestACPSingleSynthesizedModelSurvivesHiddenAcknowledgement(t *testing.T) {
	c := newGeminiControl(t, "")
	initial := toEd(c, []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"S","models":{"currentModelId":"only","availableModels":[{"modelId":"only","name":"Only"}]}}}`))
	ids, _ := configOptionIDs(t, initial)
	if slices.Contains(ids, "model") || !c.leadUsesSetModel {
		t.Fatalf("sole synthesized model must be hidden without losing its adapter contract: %s", initial)
	}
	handled, _, wire, restart := c.fromEditor([]byte(`{"jsonrpc":"2.0","id":2,"method":"session/set_config_option","params":{"sessionId":"S","configId":"model","value":"only"}}`))
	if handled || restart || !bytes.Contains(wire, []byte(`"method":"session/set_model"`)) {
		t.Fatalf("persisted hidden model failed translation: %s", wire)
	}
	ack := toEd(c, []byte(`{"jsonrpc":"2.0","id":2,"result":{}}`))
	ids, _ = configOptionIDs(t, ack)
	if slices.Contains(ids, "model") || !bytes.Contains(c.cached["S"], []byte(`"id":"model"`)) {
		t.Fatalf("translated acknowledgement lost singleton projection/cache separation: %s", ack)
	}
	settings := bytes.Join(c.sessionReady("S"), nil)
	if !bytes.Contains(settings, []byte(`"modelId":"only"`)) {
		t.Fatalf("hidden singleton no longer restores the model: %s", settings)
	}
}
