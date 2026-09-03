package acpctl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
	agents "github.com/AndrewDryga/coop/internal/agent"
)

// TestACPResumeStateRoundTrip: the SIGHUP handoff JSON-round-trips through a 0600 temp file that's
// removed after one read (the handoff data is sensitive).
func TestACPResumeStateRoundTrip(t *testing.T) {
	limitedUntil := time.Now().Add(time.Hour).Round(time.Second)
	st := ResumeState{
		Proxy: acpproxy.Snapshot{
			Initialize: []byte(`{"method":"initialize"}`),
			Authentication: []acpproxy.AuthenticationSnap{{
				Provider: "codex", Account: "work", MethodID: "oauth",
				Request: []byte(`{"method":"authenticate","params":{"methodId":"oauth"}}`),
			}},
			Sessions: []acpproxy.SessionSnap{{EditorID: "S1", AdapterID: "N1", Provider: "codex", Turned: true}},
		},
		Ctrl: Snapshot{
			Selection:        Selection{Provider: "codex"},
			AutoAccount:      "work",
			AuthFailed:       map[string]bool{"codex@default": true},
			Limited:          map[string]time.Time{"codex@default": limitedUntil},
			RotationLimited:  map[string]time.Time{"claude:claude-fable-5/xhigh@default": limitedUntil},
			PlainTargets:     map[string]targetPreference{"claude": {Model: "claude-opus-4-8", Effort: "high"}, "codex": {Model: "m", Effort: "xhigh"}},
			Cached:           map[string]json.RawMessage{"S1": json.RawMessage(`[{"id":"model","currentValue":"m"}]`)},
			NativeCache:      map[string]nativeOptionCache{"S1": {Provider: "codex", Options: json.RawMessage(`[{"id":"model","currentValue":"m"}]`)}},
			Recreate:         map[string]bool{"S1": true},
			Lead:             "codex",
			Model:            "m",
			Target:           agents.Target{Provider: "codex", Model: "m", Effort: "xhigh"},
			LeadUsesSetModel: true,
		},
	}
	path, err := WriteResumeState(st)
	if err != nil {
		t.Fatal(err)
	}
	if fi, serr := os.Stat(path); serr != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("resume file perms = %v (err %v), want 0600", fi.Mode().Perm(), serr)
	}
	got, err := ReadResumeState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ctrl.Lead != "codex" || got.Ctrl.Target.Effort != "xhigh" || !got.Ctrl.LeadUsesSetModel || got.Ctrl.Selection.Provider != "codex" ||
		got.Ctrl.AutoAccount != "work" || !got.Ctrl.AuthFailed["codex@default"] || !got.Ctrl.Limited["codex@default"].Equal(limitedUntil) ||
		!got.Ctrl.RotationLimited["claude:claude-fable-5/xhigh@default"].Equal(limitedUntil) ||
		got.Ctrl.PlainTargets["claude"] != (targetPreference{Model: "claude-opus-4-8", Effort: "high"}) ||
		!strings.Contains(string(got.Ctrl.Cached["S1"]), `"currentValue":"m"`) || got.Ctrl.NativeCache["S1"].Provider != "codex" ||
		!strings.Contains(string(got.Ctrl.NativeCache["S1"].Options), `"currentValue":"m"`) || !got.Ctrl.Recreate["S1"] {
		t.Errorf("ctrl round-trip mismatch: %+v", got.Ctrl)
	}
	if len(got.Proxy.Authentication) != 1 || got.Proxy.Authentication[0].Provider != "codex" || got.Proxy.Authentication[0].Account != "work" ||
		got.Proxy.Authentication[0].MethodID != "oauth" {
		t.Errorf("authentication round-trip mismatch: %+v", got.Proxy.Authentication)
	}
	if string(got.Proxy.Initialize) != `{"method":"initialize"}` {
		t.Errorf("initialize round-trip mismatch: %s", got.Proxy.Initialize)
	}
	if len(got.Proxy.Sessions) != 1 || got.Proxy.Sessions[0].EditorID != "S1" || got.Proxy.Sessions[0].AdapterID != "N1" ||
		got.Proxy.Sessions[0].Provider != "codex" || !got.Proxy.Sessions[0].Turned {
		t.Errorf("proxy round-trip mismatch: %+v", got.Proxy.Sessions)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Error("resume file must be removed after one read")
	}
}

func TestACPResumeStateRejectsInvalidOrLegacyProxySnapshots(t *testing.T) {
	initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	legacySetup, err := json.Marshal(map[string]any{
		"proxy": map[string]any{"setup": [][]byte{initialize}},
		"ctrl":  map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{name: "corrupt JSON", raw: []byte(`{"proxy":`), want: "unexpected end"},
		{name: "legacy setup tape", raw: legacySetup, want: "initialize request"},
		{name: "missing adapter identity", raw: mustMarshalResumeState(t, ResumeState{Proxy: acpproxy.Snapshot{
			Initialize: initialize, Sessions: []acpproxy.SessionSnap{{EditorID: "S1", Provider: "codex"}},
		}}), want: "adapter identity"},
		{name: "missing provider identity", raw: mustMarshalResumeState(t, ResumeState{Proxy: acpproxy.Snapshot{
			Initialize: initialize, Sessions: []acpproxy.SessionSnap{{EditorID: "S1", AdapterID: "N1"}},
		}}), want: "provider identity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "resume.json")
			if err := os.WriteFile(path, tc.raw, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("COOP_ACP_RESUME_STATE", path)
			if _, err := ReadResumeState(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReadResumeState legacy snapshot error = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("rejected resume file was not consumed: %v", err)
			}
			if got := os.Getenv("COOP_ACP_RESUME_STATE"); got != "" {
				t.Fatalf("resume env survived rejected state: %q", got)
			}
		})
	}
}

func mustMarshalResumeState(t *testing.T, state ResumeState) []byte {
	t.Helper()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
