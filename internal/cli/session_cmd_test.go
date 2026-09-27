package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/ui"
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

func TestSessionPaths(t *testing.T) {
	state, socket, err := sessionSocketPath("", "")
	home, _ := os.UserHomeDir()
	if err != nil || !strings.HasPrefix(state, filepath.Join(home, ".local", "state", "coop", "sessions")) || socket != filepath.Join(state, "control.sock") {
		t.Fatalf("defaults = %q %q %v", state, socket, err)
	}
}

func TestSessionDoctorFlags(t *testing.T) {
	socket, output, err := parseSessionDoctorFlags([]string{"--socket", "/tmp/control.sock", "--json"})
	if err != nil || socket != "/tmp/control.sock" || !output {
		t.Fatalf("doctor flags = %q %v %v", socket, output, err)
	}
	for _, args := range [][]string{{"--policies", "/tmp/policies"}, {"--json", "--json"}, {"--state", "/tmp/state"}} {
		if _, _, err := parseSessionDoctorFlags(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestSessionCompactFlagsRequireOneBackup(t *testing.T) {
	state, backup, err := parseSessionCompactFlags([]string{"--state", "/tmp/state", "--backup", "/tmp/backup.sqlite"})
	if err != nil || state != "/tmp/state" || backup != "/tmp/backup.sqlite" {
		t.Fatalf("compact flags = state %q backup %q err %v", state, backup, err)
	}
	for _, args := range [][]string{
		nil,
		{"--state", "/tmp/state"},
		{"--backup"},
		{"--backup", "/tmp/one", "--backup", "/tmp/two"},
		{"--policies", "/tmp/policies"},
		{"--json"},
	} {
		if _, _, err := parseSessionCompactFlags(args); err == nil || !strings.Contains(err.Error(), "sessions compact") {
			t.Fatalf("compact flags unexpectedly accepted %v: %v", args, err)
		}
	}
}

func TestRunSessionCompactCreatesANewBackup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := session.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.sqlite")
	var output []string
	ui.SetLiveSink(func(line string) { output = append(output, line) })
	defer ui.SetLiveSink(nil)
	code, runErr := runSessionCompact(root, backup)
	if runErr != nil || code != 0 {
		t.Fatalf("sessions compact = code %d err %v output %q", code, runErr, output)
	}
	joined := strings.Join(output, "\n")
	// Zero legacy receipts is not "nothing happened": the verified backup was still written, and
	// both byte facts are labeled rather than printed as an unexplained arrow.
	for _, want := range []string{
		"No older retry records needed compaction.",
		"  Database  ",
		"  Backup    " + backup + " · ",
		"⚠ The backup contains private session data",
		"  Keep it protected until you no longer need it for recovery.",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("compact output missing %q: %q", want, output)
		}
	}
	if info, err := os.Stat(backup); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup info = %+v err=%v", info, err)
	}
}
