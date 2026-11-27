package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestControllerAccountDoesNotNeedAWorkerLocalProfile(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "coop-cli-model-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	socket := filepath.Join(root, "relay.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var binding workerproto.ModelGrantRequest
		if json.NewDecoder(r.Body).Decode(&binding) != nil || binding.JobDigest != digest || binding.Provider != "claude" || binding.Account != "work" {
			http.Error(w, "account refused", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(workerproto.ModelGrant{ModelGrantRequest: binding, Token: strings.Repeat("proxy-", 8), Selection: "apikey", Epoch: 1, ExpiresAt: time.Now().Add(time.Minute)})
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	binding, err := box.ControllerModelEnvironment(socket, "session", digest)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(box.ControllerModelsEnv, binding)
	t.Setenv(box.ControllerJobEnv, digest)
	cfg := &config.Config{ConfigDir: t.TempDir()}
	app := &app{cfg: cfg}
	if err := app.requireProfile("claude", "work"); err != nil {
		t.Fatalf("controller account requires worker login: %v", err)
	}
	if err := app.requireProfile("claude", "other"); err == nil {
		t.Fatal("unselected account was accepted")
	}
	profiles, err := cfg.Profiles("claude")
	if err != nil || len(profiles) != 0 {
		t.Fatalf("created local credential state: %v %v", profiles, err)
	}
	t.Setenv(box.ControllerJobEnv, strings.Repeat("b", 64))
	if err := app.requireProfile("claude", "work"); err == nil {
		t.Fatal("mismatched controller binding was accepted")
	}
}
