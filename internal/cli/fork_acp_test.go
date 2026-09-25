package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	if hooks.ToEditor != nil || hooks.SelectProvider != nil {
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
