package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/sessionsvc"
)

// sessionHealthDTO decodes /healthz, like sessionReadyDTO decodes /readyz — the doctor is a client
// of the endpoint, not a user of the server's struct.
type sessionHealthDTO struct {
	Healthy bool `json:"healthy"`
}

func shortSessionSocketRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "coop-session-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func TestSessionHTTPDoctorDialerUsesUnixSocket(t *testing.T) {
	root := shortSessionSocketRoot(t)
	socket := filepath.Join(root, "control.sock")
	listener, cleanup, err := sessionsvc.ListenSocket(root, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"healthy":true,"ready":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	client := sessionUnixHTTPClient(socket)
	var health sessionHealthDTO
	if err := sessionDoctorGet(client, "/healthz", &health); err != nil || !health.Healthy {
		t.Fatalf("Unix doctor health err=%v health=%+v", err, health)
	}
	var ready sessionReadyDTO
	if err := sessionDoctorGet(client, "/readyz", &ready); err != nil || !ready.Ready {
		t.Fatalf("Unix doctor ready err=%v ready=%+v", err, ready)
	}
	code, output := captureSessionDoctorJSON(t, socket)
	if code != 0 {
		t.Fatalf("doctor success code = %d output=%s", code, output)
	}
	var success sessionDoctorResult
	if err := json.Unmarshal([]byte(output), &success); err != nil || !success.Healthy || !success.Ready || success.Error != "" {
		t.Fatalf("doctor success = %+v err=%v output=%s", success, err, output)
	}
	_ = server.Close()
	cleanup()
	code, output = captureSessionDoctorJSON(t, socket)
	if code == 0 {
		t.Fatalf("doctor failure code = %d output=%s", code, output)
	}
	var failure sessionDoctorResult
	if err := json.Unmarshal([]byte(output), &failure); err != nil || failure.Healthy || failure.Ready || failure.Error == "" {
		t.Fatalf("doctor failure = %+v err=%v output=%s", failure, err, output)
	}
}

func captureSessionDoctorJSON(t *testing.T, socket string) (int, string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = writer
	code, runErr := runSessionDoctor(socket, true)
	_ = writer.Close()
	os.Stdout = previous
	data, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if runErr != nil {
		t.Fatal(runErr)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return code, string(data)
}

func TestSessionCLIPathsUseRealHomeDefaults(t *testing.T) {
	state, policy, socket, err := sessionCLIPaths("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(state, filepath.Join(home, ".local", "state", "coop", "sessions")) || policy != filepath.Join(home, ".config", "coop", "session-policies.yaml") || socket != filepath.Join(state, "control.sock") {
		t.Fatalf("defaults = state %q policy %q socket %q", state, policy, socket)
	}
}
