package box

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestRemoteNativeRunKeepsProviderCredentialsAtController(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "coop-model-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	socket := filepath.Join(root, "worker.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("proxy-capability-", 4)
	digest := strings.Repeat("a", 64)
	var forwards atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/grant"):
			var req workerproto.ModelGrantRequest
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.JobDigest != digest || req.Provider != "claude" || req.Account != "default" {
				w.WriteHeader(403)
				return
			}
			_ = json.NewEncoder(w).Encode(workerproto.ModelGrant{ModelGrantRequest: req, Token: token, Selection: "apikey", Epoch: 1, ExpiresAt: time.Now().Add(time.Minute)})
		case strings.Contains(r.URL.Path, "/forward/claude/api.anthropic.com/v1/messages"):
			if r.Header.Get("X-Coop-Model-Grant") != token {
				t.Error("temporary grant absent")
				w.WriteHeader(403)
				return
			}
			if r.Header.Get("X-Api-Key") != "" || r.Header.Get("Authorization") != "" {
				t.Error("workload credential reached worker relay")
			}
			forwards.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: scripted controller reply\n\n")
		default:
			w.WriteHeader(403)
		}
	})}
	go func() { _ = server.Serve(l) }()
	defer server.Close()
	encoded, err := ControllerModelEnvironment(socket, "session", digest)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(ControllerModelsEnv, encoded)
	t.Setenv(ControllerJobEnv, digest)
	cfg := &config.Config{ConfigDir: t.TempDir(), Egress: "open", HomeInBox: "/home/node"}
	run, err := planNativeAccounts(t.Context(), cfg, runtime.Runtime{}, RunSpec{Agent: "claude", Homes: true, Repo: t.TempDir()}, false)
	if err != nil || run == nil {
		t.Fatal(err)
	}
	if run.accounts[0].record != nil {
		t.Fatal("created a local canonical credential")
	}
	if err = run.prepare(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	defer run.close()
	public, _ := json.Marshal(run.accounts[0].seed)
	if strings.Contains(string(public), token) {
		t.Fatal("capability entered native public seed")
	}
	snapshot, err := os.ReadFile(filepath.Join(run.dir, "claude.json"))
	if err != nil || !strings.Contains(string(snapshot), token) {
		t.Fatal("guard capability missing", err)
	}
	client := (&ControllerModels{Socket: filepath.Join(run.dir, "relay.sock")}).client()
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), "POST", "http://localhost/claude/api.anthropic.com/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !strings.Contains(string(body), "scripted controller reply") || forwards.Load() != 1 {
		t.Fatalf("relay result %d %q %v", resp.StatusCode, body, err)
	}
	if err = run.publishRemote(run.accounts[0], true); err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(req.Clone(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 || forwards.Load() != 1 {
		t.Fatal("revoked proxy capability was used")
	}
}
