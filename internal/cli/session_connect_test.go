package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
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
		{[]string{"--config"}, `Missing value for "--config"`},
		{[]string{"--config", "config.json", "extra"}, `Unexpected argument "extra"`},
	} {
		_, err := parseSessionConnectFlags(tc.args)
		var usage *ui.UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseSessionConnectFlags(%v) = %v, want %q usage error", tc.args, err, tc.want)
		}
	}
	if got, err := parseSessionConnectFlags([]string{"--config", "config.json"}); err != nil || !filepath.IsAbs(got) {
		t.Fatalf("valid config = %q, %v; want absolute path", got, err)
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

// A READY local service is reused: nothing is started, and the invocation owns nothing to stop.
func TestSessionConnectReusesAReadyService(t *testing.T) {
	home := shortHome(t)
	state := filepath.Join(home, "sessions")
	socket := filepath.Join(state, "control.sock")
	fakeService(t, socket, true)

	out := captureStderr(t, func() {
		owned, err := ensureLocalSessionService(context.Background(), freshConfig(t), state, filepath.Join(home, "policies.yaml"), socket)
		if err != nil {
			t.Fatalf("reuse failed: %v", err)
		}
		if owned != nil {
			owned.stop()
			t.Error("a service this invocation did not create was reported as owned")
		}
	})
	if !strings.Contains(out, "Using the running local session service") {
		t.Errorf("reuse was not narrated:\n%s", out)
	}
	if strings.Contains(out, "Starting the local session service") {
		t.Errorf("a ready service must not be started again:\n%s", out)
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

	owned, err := ensureLocalSessionService(context.Background(), freshConfig(t), state, filepath.Join(home, "policies.yaml"), socket)
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
	policy := filepath.Join(home, "policies.yaml")
	writeConnectPolicy(t, policy)

	cfg := signedInConfig(t)
	var owned *ownedSessionService
	out := captureStderr(t, func() {
		var err error
		owned, err = ensureLocalSessionService(context.Background(), cfg, state, policy, socket)
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

// A service that STOPS before it is ready leaves nothing behind: its own error is the whole story,
// the invocation owns nothing to stop, and no half-started service is left listening for a
// controller. Autostart must never generate the policy file it could not find.
func TestSessionConnectReportsAServiceThatNeverBecameReady(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", home)
	state := filepath.Join(home, "sessions")
	socket := filepath.Join(state, "control.sock")
	policy := filepath.Join(home, "policies.yaml") // deliberately absent

	owned, err := ensureLocalSessionService(context.Background(), signedInConfig(t), state, policy, socket)
	if err == nil {
		if owned != nil {
			owned.stop()
		}
		t.Fatal("a service with no policy file was reported as ready")
	}
	if owned != nil {
		owned.stop()
		t.Error("a service that never became ready was reported as owned")
	}
	if _, listening := sessionServiceReady(socket); listening {
		t.Error("a half-started service was left listening")
	}
	if _, statErr := os.Stat(policy); statErr == nil {
		t.Error("autostart generated a session policy file")
	}
}

// Two invocations racing the same state root resolve through the storage lock: the loser does not
// start a second service, it re-checks readiness and uses the one that won.
func TestSessionConnectResolvesARaceThroughTheStorageLock(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", home)
	state := filepath.Join(home, "sessions")
	socket := filepath.Join(state, "control.sock")
	policy := filepath.Join(home, "policies.yaml")
	writeConnectPolicy(t, policy)

	cfg := signedInConfig(t)
	winner, err := ensureLocalSessionService(context.Background(), cfg, state, policy, socket)
	if err != nil || winner == nil {
		t.Fatalf("first connect = (%v, %v)", winner, err)
	}
	defer winner.stop()

	out := captureStderr(t, func() {
		loser, err := ensureLocalSessionService(context.Background(), cfg, state, policy, socket)
		if err != nil {
			t.Fatalf("the racing connect failed instead of reusing: %v", err)
		}
		if loser != nil {
			loser.stop()
			t.Error("the racing connect claimed ownership of a service it did not start")
		}
	})
	if !strings.Contains(out, "Using the running local session service") {
		t.Errorf("the racing connect did not reuse the running service:\n%s", out)
	}
	// The winner is untouched.
	if ready, _ := sessionServiceReady(socket); !ready {
		t.Error("the running service stopped during the race")
	}
}

// writeConnectPolicy writes the smallest valid session policy file: autostart must not invent one,
// so every test that starts a real service supplies it.
func writeConnectPolicy(t *testing.T, path string) {
	t.Helper()
	// The loader requires a real Git worktree at a real (non-symlinked) path.
	repo := shortHome(t)
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "T"},
		{"commit", "-q", "--allow-empty", "-m", "base"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "noglobal"),
			"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "nosystem"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	body := "version: 1\npolicies:\n  local:\n    repository: " + repo + "\n" +
		"    target: claude\n    max_turns: 5\n    max_queued_turns: 2\n" +
		"    max_queued_bytes: 2048\n    max_patch_bytes: 4096\n    turn_timeout: 30m\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
