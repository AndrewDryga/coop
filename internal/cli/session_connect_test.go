package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/ui"
)

func TestParseSessionConnectFlagsReportsTheActualBadArgument(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--bogus", "x"}, `Unknown option "--bogus"`},
		{[]string{"config.json"}, `Unexpected argument "config.json"`},
		{[]string{"--controller"}, `Missing value for "--controller"`},
		{[]string{"--controller", "https://controller.example", "extra"}, `Unexpected argument "extra"`},
		{[]string{"--config", "config.json"}, `Unknown option "--config"`},
	} {
		_, err := parseSessionConnectFlags(tc.args)
		var usage *ui.UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseSessionConnectFlags(%v) = %v, want %q usage error", tc.args, err, tc.want)
		}
	}
	if got, err := parseSessionConnectFlags([]string{"--controller", "https://controller.example", "--token-file", "token"}); err != nil || !filepath.IsAbs(got.TokenFile) || !filepath.IsAbs(got.State) || got.Controller != "https://controller.example" {
		t.Fatalf("valid connect = %+v, %v", got, err)
	}
	if _, err := parseSessionConnectFlags([]string{"--controller", "https://controller.example"}); err != nil {
		t.Fatalf("reconnect without enrollment token: %v", err)
	}

}

// shortHome is a REAL, short directory for a session state root. t.TempDir() will not do: on macOS
// it is a symlinked /var/folders path (the service refuses a state parent that is not a real
// directory) and it is long enough to exceed the 104-byte sun_path limit for a Unix socket.
func shortHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "coop-sc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func fakeSessionRuntime(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeService answers /healthz and /readyz on a real Unix socket, the way the local session
// service does — so the autostart decision is exercised against an actual listening socket
// instead of a stub of the probe. ready=false is a service that is up but cannot take sessions.
func fakeService(t *testing.T, socket string, ready bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": ready})
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	// Wait for the socket to answer, so a test never races its own fixture.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if r, _ := sessionServiceReady(socket); r == ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("fake session service never answered")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Readiness is not authority to reuse a service owned by another connection.
func TestSessionConnectRefusesToReuseAReadyService(t *testing.T) {
	home := shortHome(t)
	state := filepath.Join(home, "sessions")
	socket := filepath.Join(state, "control.sock")
	fakeService(t, socket, true)
	owned, err := startLocalSessionService(context.Background(), freshConfig(t), state, socket, nil, nil)
	if owned != nil || err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("foreign ready service = %v, %v", owned, err)
	}
	if ready, _ := sessionServiceReady(socket); !ready {
		t.Fatal("refusal disturbed the foreign service")
	}
}

// A service that is LISTENING but not ready is not absent. Its socket is left alone, no second
// service is started, and the failure says what to check — unlinking it would cut off whoever is
// already talking to it.
func TestSessionConnectRefusesToDisplaceAnUnhealthyService(t *testing.T) {
	home := shortHome(t)
	state := filepath.Join(home, "sessions")
	socket := filepath.Join(state, "control.sock")
	fakeService(t, socket, false)

	owned, err := startLocalSessionService(context.Background(), freshConfig(t), state, socket, nil, nil)
	if err == nil {
		if owned != nil {
			owned.stop()
		}
		t.Fatal("a live but unready service was displaced")
	}
	if !strings.Contains(err.Error(), "already listening") {
		t.Errorf("the refusal should name the live service, got: %v", err)
	}
	if info, statErr := os.Lstat(socket); statErr != nil || info.Mode()&os.ModeSocket == 0 {
		t.Errorf("the live socket was removed: %v", statErr)
	}
	// The socket still answers: nothing tore it down.
	if _, listening := sessionServiceReady(socket); !listening {
		t.Error("the live service stopped answering after the refusal")
	}
}

// With no service at all, connect starts one, proves it ready, and OWNS it — so Ctrl-C stops it.
func TestSessionConnectStartsAndOwnsAService(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", home)
	state := filepath.Join(home, "sessions")
	socket := filepath.Join(state, "control.sock")

	cfg := signedInConfig(t)
	cfg.RuntimeName = fakeSessionRuntime(t)
	var owned *ownedSessionService
	out := captureStderr(t, func() {
		var err error
		owned, err = startLocalSessionService(context.Background(), cfg, state, socket, nil, nil)
		if err != nil {
			t.Fatalf("autostart failed: %v", err)
		}
	})
	if owned == nil {
		t.Fatal("a service this invocation started was not reported as owned")
	}
	for _, want := range []string{"Starting the local session service…", "✓ Local session service ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("autostart narration missing %q:\n%s", want, out)
		}
	}
	if ready, _ := sessionServiceReady(socket); !ready {
		t.Fatal("the started service never became ready")
	}
	// Stopping the owned service really stops it — and only it.
	owned.stop()
	if _, listening := sessionServiceReady(socket); listening {
		t.Error("the owned service kept listening after stop")
	}
}

// A failed startup must not leave a socket or owned process behind.
func TestSessionConnectReportsAServiceThatNeverBecameReady(t *testing.T) {
	home := shortHome(t)
	state := filepath.Join(home, "not-a-directory")
	if err := os.WriteFile(state, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(state, "control.sock")
	owned, err := startLocalSessionService(context.Background(), signedInConfig(t), state, socket, nil, nil)
	if owned != nil {
		owned.stop()
		t.Fatal("failed service claimed ownership")
	}
	if err == nil {
		t.Fatal("invalid state directory was accepted")
	}
}

// The storage lock chooses exactly one connection, even when both initial probes see no service.
func TestSessionConnectResolvesARaceThroughTheStorageLock(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", home)
	state := filepath.Join(home, "sessions")
	socket := filepath.Join(state, "control.sock")
	cfg := signedInConfig(t)
	cfg.RuntimeName = fakeSessionRuntime(t)
	type result struct {
		owned *ownedSessionService
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			owned, err := startLocalSessionService(context.Background(), cfg, state, socket, nil, nil)
			results <- result{owned, err}
		}()
	}
	close(start)
	var winner *ownedSessionService
	for range 2 {
		r := <-results
		if r.owned != nil {
			if winner != nil {
				r.owned.stop()
				t.Fatal("both connections claimed the same service")
			}
			winner = r.owned
			t.Cleanup(winner.stop)
			if r.err != nil {
				t.Fatal(r.err)
			}
		} else if r.err == nil {
			t.Fatal("loser silently reused the winning service")
		}
	}
	if winner == nil {
		t.Fatal("neither connection acquired the service")
	}
	if ready, _ := sessionServiceReady(socket); !ready {
		t.Fatal("losing connection disturbed the winner")
	}
}
