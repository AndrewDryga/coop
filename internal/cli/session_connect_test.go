package cli

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/testutil/workertls"
	"github.com/AndrewDryga/coop/internal/workerconnector"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
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

// A first connection with no token is refused before anything starts or is contacted. The block says
// no identity is saved and shows this command with a token file added, keeping the --state and
// --ca-file it was given: without them the command would enroll in another folder, or fail the
// certificate check.
func TestSessionConnectExplainsAFirstConnectionWithoutAToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // the default state folder follows it
	var contacts atomic.Int32
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			contacts.Add(1)
			conn.Close()
		}
	}()
	controller := "https://" + listener.Addr().String()
	ca := filepath.Join(home, "ca.pem")
	if err := os.WriteFile(ca, workertls.NewCA(t).PEM, 0o600); err != nil {
		t.Fatal(err)
	}

	refuse := func(options sessionConnectOptions, state, example string) {
		t.Helper()
		code, err := runSessionConnect(&config.Config{}, options)
		want := &ui.UsageError{
			Headline: "A first connection needs an enrollment token",
			Cause: "No worker identity is saved in " + state + ".\n" +
				"Save the token the controller gave you in a file and pass it with --token-file.",
			Rows:     [][2]string{{"Example:", example}, {"Help:", "coop help sessions connect"}},
			ExitCode: 1,
		}
		if usage := asUsage(err); code != 1 || !reflect.DeepEqual(usage, want) {
			t.Errorf("connect with no token = (%d, %#v), want %#v", code, err, want)
		}
		if _, err := os.Lstat(state); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a refused connect made its state folder: %v", err)
		}
	}
	state := filepath.Join(home, "w1")
	refuse(sessionConnectOptions{Controller: controller, CAFile: ca, State: state}, state,
		"coop sessions connect --controller "+shellWord(controller)+" --ca-file "+shellWord(ca)+" --state "+shellWord(state)+
			" --token-file /path/to/token")
	// The default state folder is left out, as it is when it is not typed.
	refuse(sessionConnectOptions{Controller: controller}, filepath.Join(home, ".local", "state", "coop", "sessions"),
		"coop sessions connect --controller "+shellWord(controller)+" --token-file /path/to/token")
	if contacts.Load() != 0 {
		t.Errorf("a connection with no token contacted the controller %d times", contacts.Load())
	}
}

// Only a connection that has to enroll needs a token. A saved identity is reused with the flag or
// without it, and a token file is checked only when nothing is saved.
func TestFirstConnectionRefusalNeedsATokenOnlyWithoutASavedIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	token := filepath.Join(home, "token")
	if err := os.WriteFile(token, []byte(strings.Repeat("t", 43)), 0o600); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(home, "gone")
	saved := filepath.Join(home, "saved")
	if err := os.Mkdir(saved, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saved, "identity.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(home, "empty")
	for _, tc := range []struct {
		name, state, token string
		refusal            string // the headline of the block, or "" for no refusal
	}{
		{"nothing saved, no token", empty, "", "A first connection needs an enrollment token"},
		{"nothing saved, a token file that is gone", empty, gone, "Could not use the enrollment token file"},
		{"nothing saved, a usable token", empty, token, ""},
		{"identity saved, no token", saved, "", ""},
		{"identity saved, the flag it enrolled with", saved, gone, ""},
	} {
		got := ""
		if err := firstConnectionRefusal(tc.state, sessionConnectOptions{Controller: "https://controller.example", TokenFile: tc.token}); err != nil {
			got = err.Error()
			if usage := asUsage(err); usage != nil {
				got = usage.Headline
			}
		}
		if got != tc.refusal {
			t.Errorf("%s refused with %q, want %q", tc.name, got, tc.refusal)
		}
	}
}

// connectTo runs a first `coop sessions connect` to controller with a valid token file and a fresh
// state folder, and returns the token file's path, the exit code and error, and what it printed.
func connectTo(t *testing.T, controller, caFile string) (token string, code int, out string, err error) {
	t.Helper()
	home := shortHome(t)
	t.Setenv("HOME", home)
	token = filepath.Join(home, "token")
	if err := os.WriteFile(token, []byte(strings.Repeat("t", 43)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := signedInConfig(t)
	cfg.RuntimeName = fakeSessionRuntime(t)
	options := sessionConnectOptions{Controller: controller, TokenFile: token, CAFile: caFile, State: filepath.Join(home, "w1")}
	out = captureStderr(t, func() { code, err = runSessionConnect(cfg, options) })
	return token, code, out, err
}

// writeCertificateFile saves a PEM file for --ca-file in a folder of its own.
func writeCertificateFile(t *testing.T, certificatePEM []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, certificatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// enrollmentController is a TLS controller that answers every enrollment with status. It returns
// the controller, a --ca-file that trusts it, and the count of enrollments it was asked for.
func enrollmentController(t *testing.T, status int) (*httptest.Server, string, *atomic.Int32) {
	t.Helper()
	var enrollments atomic.Int32
	controller := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/coop-workers/enroll" {
			t.Errorf("the worker asked for %s, want only the enrollment", request.URL.Path)
		}
		enrollments.Add(1)
		response.WriteHeader(status)
	}))
	t.Cleanup(controller.Close)
	trusted := writeCertificateFile(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: controller.Certificate().Raw}))
	return controller, trusted, &enrollments
}

// A controller that refuses the enrollment token ends a first connection at once, naming what it
// answered and what to do, and leaves the token file for the new token.
func TestSessionConnectExplainsARefusedEnrollmentToken(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		controller, trusted, enrollments := enrollmentController(t, status)
		token, code, out, err := connectTo(t, controller.URL, trusted)
		want := &ui.UsageError{
			Headline: "The controller refused the enrollment token",
			Cause: fmt.Sprintf("It answered HTTP %d to the enrollment request.\n", status) +
				"Get a new token from the controller and save it in the file you gave --token-file.",
			Rows:     [][2]string{{"Help:", "coop help sessions connect"}},
			ExitCode: 1,
		}
		if usage := asUsage(err); code != 1 || !reflect.DeepEqual(usage, want) {
			t.Errorf("connect with a %d from the controller = (%d, %#v), want %#v", status, code, err, want)
		}
		if enrollments.Load() != 1 || strings.Contains(out, "Trying again") {
			t.Errorf("a refused token was retried: %d enrollments\n%s", enrollments.Load(), out)
		}
		if _, err := os.Stat(token); err != nil {
			t.Errorf("the refused token file was removed: %v", err)
		}
	}
}

// Any other status is not something a person can fix, so it keeps its one status line.
func TestSessionConnectKeepsTheStatusLineForAnotherEnrollmentStatus(t *testing.T) {
	controller, trusted, _ := enrollmentController(t, http.StatusInternalServerError)
	_, code, _, err := connectTo(t, controller.URL, trusted)
	if code != 1 || err == nil || asUsage(err) != nil || err.Error() != "worker identity endpoint returned HTTP 500" {
		t.Errorf("connect with a controller that answers 500 = (%d, %#v), want its one status line", code, err)
	}
}

// A controller certificate from an authority this machine does not trust ends a first connection at
// once and names --ca-file. When --ca-file was given, the block says its file lacks the authority
// instead of blaming the system trust store, which --ca-file replaced.
func TestSessionConnectExplainsAnUntrustedControllerCertificate(t *testing.T) {
	var requests atomic.Int32
	controller := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	controller.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshakes are the point
	controller.StartTLS()
	defer controller.Close()
	check := func(caFile, cause string) {
		t.Helper()
		token, code, out, err := connectTo(t, controller.URL, caFile)
		want := &ui.UsageError{
			Headline: "This machine does not trust the controller's certificate",
			Cause:    cause,
			Rows:     [][2]string{{"Help:", "coop help sessions connect"}},
			ExitCode: 1,
		}
		if usage := asUsage(err); code != 1 || !reflect.DeepEqual(usage, want) {
			t.Errorf("connect with --ca-file %q = (%d, %#v), want %#v", caFile, code, err, want)
		}
		if requests.Load() != 0 || strings.Contains(out, "Trying again") {
			t.Errorf("a controller that could not be trusted was asked or retried: %d requests\n%s", requests.Load(), out)
		}
		if _, err := os.Stat(token); err != nil {
			t.Errorf("the token file was removed: %v", err)
		}
	}

	const advice = "\nIf you trust that authority, give --ca-file its certificate file."
	// The controller's certificate is in no system trust store.
	check("", "The authority that signed it is not in the system trust store."+advice)
	// A --ca-file for some other authority replaces the system store, so it is the one that lacks it.
	other := writeCertificateFile(t, workertls.NewCA(t).PEM)
	check(other, "The authority that signed it is not in "+other+"."+advice)
}

// A state folder holding an identity this worker cannot use is refused with the reason: one saved for
// another controller names it and the two ways forward, and a damaged file is called damaged and
// named. The file is never replaced, and the token passed beside it is not used.
func TestSessionConnectExplainsASavedIdentityItCannotUse(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", home)
	cfg := signedInConfig(t)
	cfg.RuntimeName = fakeSessionRuntime(t)
	token := filepath.Join(home, "token")
	if err := os.WriteFile(token, []byte(strings.Repeat("t", 43)), 0o600); err != nil {
		t.Fatal(err)
	}
	help := [2]string{"Help:", "coop help sessions connect"}
	for _, tc := range []struct {
		name, saved string
		block       func(file string) *ui.UsageError
	}{
		{"another-controller", `{"controller_url":"https://prod.example","worker_id":"worker-a","workspace_ref":"main"}`, func(file string) *ui.UsageError {
			return &ui.UsageError{
				Headline: "The saved worker identity is for another controller",
				Cause: "The identity in " + filepath.Dir(file) + " belongs to https://prod.example, not https://staging.example.\n" +
					"Use that address for --controller, or a new --state folder with a new --token-file.",
				Rows: [][2]string{help}, ExitCode: 1,
			}
		}},
		{"damaged", "{not json", func(file string) *ui.UsageError {
			return &ui.UsageError{
				Headline: "The saved worker identity is damaged",
				Cause: file + " cannot be read as a worker identity.\n" +
					"Coop will not replace it, even with a new token.\n" +
					"Restore it from a backup, or ask the controller's operator how to recover this worker.",
				Rows: [][2]string{help}, ExitCode: 1,
			}
		}},
	} {
		state := filepath.Join(home, tc.name)
		file := filepath.Join(state, "identity.json")
		if err := os.MkdirAll(state, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(tc.saved), 0o600); err != nil {
			t.Fatal(err)
		}
		var code int
		var err error
		out := captureStderr(t, func() {
			code, err = runSessionConnect(cfg, sessionConnectOptions{Controller: "https://staging.example", TokenFile: token, State: state})
		})
		if usage := asUsage(err); code != 1 || !reflect.DeepEqual(usage, tc.block(file)) {
			t.Errorf("connect with %s saved = (%d, %#v), want %#v", tc.name, code, err, tc.block(file))
		}
		if strings.Contains(out, "Trying again") {
			t.Errorf("%s: an identity that cannot be used was retried\n%s", tc.name, out)
		}
		if after, readErr := os.ReadFile(file); readErr != nil || string(after) != tc.saved {
			t.Errorf("%s: the saved identity was changed: %q, %v", tc.name, after, readErr)
		}
		if _, statErr := os.Stat(token); statErr != nil {
			t.Errorf("%s: the token was used over a saved identity: %v", tc.name, statErr)
		}
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
