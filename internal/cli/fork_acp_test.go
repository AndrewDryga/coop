package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestForkACPMapsOnlyItsProjectAndForkCwd(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	workspace := filepath.Join(root, "repo-forks", "review")
	other := filepath.Join(root, "other")
	for _, path := range []string{repo, workspace, other} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, cwd, wantCwd string
		wantError                  bool
	}{
		{"new from parent", "session/new", repo, workspace, false},
		{"new from parent alias", "session/new", alias, workspace, false},
		{"load from parent", "session/load", repo, workspace, false},
		{"resume from parent", "session/resume", repo, workspace, false},
		{"new from fork", "session/new", workspace, workspace, false},
		{"unrelated cwd", "session/new", other, "", true},
		{"missing cwd", "session/new", filepath.Join(root, "missing"), "", true},
		{"other method", "session/prompt", other, other, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": tc.method,
				"params": map[string]any{"cwd": tc.cwd, "sessionId": "s1", "mcpServers": []any{}}})
			line = append(line, '\n')
			handled, response, rewritten, restart := forkACPFromEditor(line, repo, workspace)
			if restart {
				t.Fatal("cwd rewrite requested a target switch")
			}
			if tc.wantError {
				if !handled || len(rewritten) != 0 || !strings.HasSuffix(string(response), "\n") {
					t.Fatalf("denial = handled %v, response %q, rewrite %q", handled, response, rewritten)
				}
				var denial struct {
					ID    int `json:"id"`
					Error struct {
						Code int `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(response, &denial); err != nil || denial.ID != 7 || denial.Error.Code != -32602 {
					t.Fatalf("bad cwd response = %s, %v", response, err)
				}
				return
			}
			if handled || len(response) != 0 {
				t.Fatalf("accepted cwd was handled locally: %v %q", handled, response)
			}
			if tc.method == "session/prompt" {
				if len(rewritten) != 0 {
					t.Fatalf("unrelated method was rewritten: %s", rewritten)
				}
				return
			}
			if !strings.HasSuffix(string(rewritten), "\n") {
				t.Fatalf("rewritten ACP request is not line framed: %q", rewritten)
			}
			var got struct {
				ID     int `json:"id"`
				Params struct {
					Cwd        string `json:"cwd"`
					SessionID  string `json:"sessionId"`
					MCPServers []any  `json:"mcpServers"`
				} `json:"params"`
			}
			if err := json.Unmarshal(rewritten, &got); err != nil || got.ID != 7 || got.Params.Cwd != tc.wantCwd ||
				got.Params.SessionID != "s1" || got.Params.MCPServers == nil {
				t.Fatalf("rewritten request = %s, %v", rewritten, err)
			}
		})
	}
}

func TestForkACPFixedHooksForceModeAndHandlePermissions(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	ctrl := newForkACPControl(cfg, agents.Target{Provider: "claude", Model: "opus", Effort: "high"}, t.TempDir())
	hooks := fixedForkACPHooks(ctrl, t.TempDir(), t.TempDir())
	if hooks.SelectProvider != nil {
		t.Fatal("pinned fork ACP exposed the plain editor's provider toolbar")
	}
	settings := hooks.SessionReady("s1")
	joined := string(settings[0])
	if len(settings) < 3 || !strings.Contains(joined, `"configId":"mode"`) ||
		!strings.Contains(joined, `"value":"bypassPermissions"`) {
		t.Fatalf("fork session did not force its permission mode: %q", settings)
	}
	if !strings.Contains(string(settings[1]), `"configId":"model"`) ||
		!strings.Contains(string(settings[1]), `"value":"opus"`) ||
		!strings.Contains(string(settings[2]), `"configId":"effort"`) ||
		!strings.Contains(string(settings[2]), `"value":"high"`) {
		t.Fatalf("fork session did not retain the requested model and effort: %q", settings)
	}
	var setting struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(settings[0], &setting); err != nil || setting.ID == "" {
		t.Fatalf("invalid forced setting: %s, %v", settings[0], err)
	}
	failed, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": setting.ID,
		"error": map[string]any{"code": -32602, "message": "mode unavailable"}})
	if notice := hooks.InjectedResponse(settings[0], failed); !strings.Contains(string(notice), "mode unavailable") ||
		!strings.Contains(string(notice), `"sessionId":"s1"`) {
		t.Fatalf("rejected fork setting was invisible: %s", notice)
	}
	reply, forward := hooks.AutoReply([]byte(`{"jsonrpc":"2.0","id":9,"method":"session/request_permission","params":{"options":[{"optionId":"allow_once","kind":"allow_once"}]}}`))
	if forward || !strings.Contains(string(reply), `"optionId":"allow_once"`) {
		t.Fatalf("permission request was not handled by Coop: forward=%v reply=%s", forward, reply)
	}
}

func TestForkACPFixedHooksRetainAcceptedNativeChoices(t *testing.T) {
	root := t.TempDir()
	repo, workspace := filepath.Join(root, "repo"), filepath.Join(root, "fork")
	for _, path := range []string{repo, workspace} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{ConfigDir: t.TempDir()}
	hooks := fixedForkACPHooks(newForkACPControl(cfg,
		agents.Target{Provider: "claude", Model: "opus", Effort: "high"}, workspace), repo, workspace)
	if hooks.SelectProvider != nil {
		t.Fatal("fixed fork gained provider selection")
	}
	if hooks.ToEditor == nil {
		t.Fatal("fixed fork does not observe native choice responses")
	}
	newSession := []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"s1","configOptions":[{"id":"model"}]}}` + "\n")
	if forwarded, restart := hooks.ToEditor(newSession); restart || !bytes.Equal(forwarded, newSession) ||
		bytes.Contains(forwarded, []byte("coop_provider")) || bytes.Contains(forwarded, []byte("coop_account")) ||
		bytes.Contains(forwarded, []byte("coop_preset")) {
		t.Fatalf("fixed fork changed the native toolbar: %s, restart=%t", forwarded, restart)
	}
	newRequest := []byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":` + strconv.Quote(repo) + `}}` + "\n")
	if handled, response, rewritten, restart := hooks.FromEditor(newRequest); handled || len(response) != 0 || restart ||
		!bytes.Contains(rewritten, []byte(strconv.Quote(workspace))) {
		t.Fatalf("fixed fork lost cwd rewriting: handled=%t response=%s rewritten=%s restart=%t", handled, response, rewritten, restart)
	}
	for _, choice := range []struct {
		id     int
		field  string
		value  string
		result string
	}{
		{3, "model", "sonnet", `{"result":{"configOptions":[]}}`},
		{4, "effort", "max", `{"result":{"configOptions":[]}}`},
		{5, "model", "not-a-model", `{"error":{"code":-32602,"message":"unsupported"}}`},
	} {
		request := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/set_config_option","params":{"sessionId":"s1","configId":%q,"value":%q}}`+"\n", choice.id, choice.field, choice.value))
		if handled, response, rewritten, restart := hooks.FromEditor(request); handled || len(response) != 0 || len(rewritten) != 0 || restart {
			t.Fatalf("native choice was intercepted: handled=%t response=%s rewritten=%s restart=%t", handled, response, rewritten, restart)
		}
		result := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,`, choice.id) + choice.result[1:] + "\n")
		if forwarded, restart := hooks.ToEditor(result); restart || !bytes.Equal(forwarded, result) {
			t.Fatalf("native choice response changed: %s, restart=%t", forwarded, restart)
		}
	}
	if hooks.ChildReset != nil {
		hooks.ChildReset()
	}
	settings := string(bytes.Join(hooks.SessionReady("s1"), nil))
	if !strings.Contains(settings, `"configId":"model","sessionId":"s1","value":"sonnet"`) ||
		!strings.Contains(settings, `"configId":"effort","sessionId":"s1","value":"max"`) ||
		strings.Contains(settings, "not-a-model") {
		t.Fatalf("restarted fork lost or poisoned native target: %s", settings)
	}
}

func TestForkACPFixedHooksRetainGeminiNativeModel(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	hooks := fixedForkACPHooks(newForkACPControl(cfg,
		agents.Target{Provider: "gemini", Model: "initial-model"}, t.TempDir()), t.TempDir(), t.TempDir())
	for _, choice := range []struct {
		id    int
		model string
		ok    bool
	}{
		{1, "chosen-model", true},
		{2, "rejected-model", false},
	} {
		request := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/set_model","params":{"sessionId":"s1","modelId":%q}}`+"\n", choice.id, choice.model))
		if handled, _, _, restart := hooks.FromEditor(request); handled || restart {
			t.Fatalf("native Gemini model set was intercepted: %s", request)
		}
		response := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{}}`+"\n", choice.id))
		if !choice.ok {
			response = []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32602}}`+"\n", choice.id))
		}
		if forwarded, restart := hooks.ToEditor(response); restart || !bytes.Equal(forwarded, response) {
			t.Fatalf("native Gemini response changed: %s, restart=%t", forwarded, restart)
		}
	}
	settings := string(bytes.Join(hooks.SessionReady("s1"), nil))
	if !strings.Contains(settings, `"method":"session/set_model"`) ||
		!strings.Contains(settings, `"modelId":"chosen-model"`) || strings.Contains(settings, "rejected-model") {
		t.Fatalf("restarted Gemini fork lost or poisoned native model: %s", settings)
	}
}
