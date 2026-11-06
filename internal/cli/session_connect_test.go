package cli

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/workerconnector"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestParseSessionConnectFlagsReportsTheActualBadArgument(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // a connect without --state resolves the default session state
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--bogus", "x"}, `Unknown option "--bogus"`},
		{[]string{"config.json"}, `Unexpected argument "config.json"`},
		{[]string{"--controller"}, `Missing value for "--controller"`},
		{[]string{"--controller", "https://controller.example", "extra"}, `Unexpected argument "extra"`},
		{[]string{"--config", "config.json"}, `Unknown option "--config"`},
		// Leaving the option out is not a missing value.
		{[]string{"--token-file", "token"}, `Missing --controller for "coop sessions connect"`},
		{[]string{"--controller", "https://a.example", "--controller", "https://b.example"}, `Option "--controller" can only be used once`},
		// An address the worker could never use is rejected input, not a failed connection, and the
		// refusal does not repeat it: a user name in it can carry a password.
		{[]string{"--controller", "http://controller.example"}, `Invalid value for "--controller" in "coop sessions connect"`},
		{[]string{"--controller", "https://user:secret@controller.example/path"}, `Invalid value for "--controller" in "coop sessions connect"`},
	} {
		_, err := parseSessionConnectFlags(tc.args)
		var usage *ui.UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") {
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

// A token file the worker cannot use is refused before anything starts, with the check it failed
// and its fix: the chmod for a file other users can open. The usage line shows --token-file as
// optional, as coop help does: only the first connection needs it.
func TestSessionConnectExplainsAnUnusableTokenFile(t *testing.T) {
	home := shortHome(t)
	token := filepath.Join(home, "tok")
	if err := os.WriteFile(token, []byte("qa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(token, 0o644); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "w1")
	code, err := runSessionConnect(&config.Config{}, sessionConnectOptions{Controller: "https://127.0.0.1:9", TokenFile: token, State: state})
	want := &ui.UsageError{
		Headline: "Could not use the enrollment token file",
		Cause:    token + " is open to other users (mode 0644).",
		Rows:     [][2]string{{"Make it private:", "chmod 600 " + token}, {"Help:", "coop help sessions connect"}},
		ExitCode: 1,
	}
	if usage := asUsage(err); code != 1 || usage == nil || !reflect.DeepEqual(usage, want) {
		t.Errorf("connect with a shared token file = (%d, %#v), want %#v", code, err, want)
	}
	if _, err := os.Lstat(state); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused connect made its state folder: %v", err)
	}

	if err := os.Chmod(token, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = runSessionConnect(&config.Config{}, sessionConnectOptions{Controller: "https://127.0.0.1:9", TokenFile: token, State: state})
	if usage := asUsage(err); usage == nil || usage.Cause != sentence(token+" does not hold an enrollment token")+"\nSave the token the controller gave you in it, on one line." {
		t.Errorf("connect with a private file that holds no token = %#v", err)
	}

	_, err = parseSessionConnectFlags([]string{"config.json"})
	if usage := asUsage(err); usage == nil || usage.Rows[0] != [2]string{"Usage:", "coop sessions connect --controller <https-url> [--token-file <path>]"} {
		t.Errorf("the usage line = %#v, want --token-file shown as optional", err)
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

// A first connection waits for a controller it cannot reach, and stops on anything waiting cannot fix.
func TestWaitForIdentityRetriesOnlyAnUnreachableController(t *testing.T) {
	unreachable := &url.Error{Op: "Post", URL: "https://controller.example", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}
	calls, warned := 0, 0
	warn := func(string, ...any) { warned++ }
	now := func(time.Duration) <-chan time.Time {
		ready := make(chan time.Time, 1)
		ready <- time.Now()
		return ready
	}
	id, err := waitForIdentity(context.Background(), func(context.Context) (workerconnector.WorkerIdentity, error) {
		calls++
		if calls < 3 {
			return workerconnector.WorkerIdentity{}, unreachable
		}
		return workerconnector.WorkerIdentity{ID: "worker"}, nil
	}, warn, now)
	if err != nil || id.ID != "worker" || calls != 3 || warned != 2 {
		t.Errorf("retry = (%+v, %v), %d calls, %d warnings", id, err, calls, warned)
	}

	refused := errors.New("the controller refused this token")
	calls = 0
	if _, err := waitForIdentity(context.Background(), func(context.Context) (workerconnector.WorkerIdentity, error) {
		calls++
		return workerconnector.WorkerIdentity{}, refused
	}, warn, now); !errors.Is(err, refused) || calls != 1 {
		t.Errorf("refused token = %v after %d calls, want one try", err, calls)
	}

	untrusted := &url.Error{Op: "Post", URL: "https://controller.example", Err: x509.UnknownAuthorityError{}}
	calls = 0
	if _, err := waitForIdentity(context.Background(), func(context.Context) (workerconnector.WorkerIdentity, error) {
		calls++
		return workerconnector.WorkerIdentity{}, untrusted
	}, warn, now); err == nil || calls != 1 {
		t.Errorf("untrusted certificate = %v after %d calls, want one try", err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	never := func(time.Duration) <-chan time.Time { return nil }
	if _, err := waitForIdentity(ctx, func(context.Context) (workerconnector.WorkerIdentity, error) {
		return workerconnector.WorkerIdentity{}, unreachable
	}, warn, never); err == nil {
		t.Error("a stopped command kept waiting")
	}
}

// Each compact failure says what to change, in place of "could not be verified".
func TestSessionCompactFailureNamesTheProblem(t *testing.T) {
	for _, tc := range []struct{ err, want string }{
		{"create new session backup: open /b: file exists", "already exists"},
		{"inspect backup directory: lstat /nope: no such file or directory", "folder does not exist"},
		{"inspect existing session database: lstat /s/session.sqlite: no such file or directory", "no session data"},
	} {
		var err error = errors.New(tc.err)
		if strings.Contains(tc.err, "file exists") {
			err = fmt.Errorf("create new session backup: %w", fs.ErrExist)
		}
		if got := sessionCompactFailure(sessionsvc.CompactionResult{}, err).Error(); !strings.Contains(got, tc.want) {
			t.Errorf("%q said %q, want %q", tc.err, got, tc.want)
		}
	}
}
