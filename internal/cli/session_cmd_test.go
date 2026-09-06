package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
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

func TestSessionPoliciesFlagsAreNarrow(t *testing.T) {
	_, policy, _, jsonOutput, err := parseSessionsFlags(
		[]string{"--policies", "/etc/coop/session-policies.yaml", "--json"},
		"policies",
	)
	if err != nil || policy != "/etc/coop/session-policies.yaml" || !jsonOutput {
		t.Fatalf("sessions policies flags = policy %q json %v err %v", policy, jsonOutput, err)
	}
	for _, args := range [][]string{
		{"--state", "/tmp/state"},
		{"--socket", "/tmp/control.sock"},
	} {
		if _, _, _, _, err := parseSessionsFlags(args, "policies"); err == nil {
			t.Fatalf("sessions policies unexpectedly accepted %v", args)
		}
	}
	if _, _, _, _, err := parseSessionsFlags([]string{"--json", "--json"}, "policies"); err == nil ||
		!strings.Contains(err.Error(), "sessions policies") {
		t.Fatalf("duplicate policies --json error = %v", err)
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
	for _, want := range []string{
		"no legacy turn retry receipts needed compaction",
		"database:",
		"backup: " + backup,
		"contains private session data",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("compact output missing %q: %q", want, output)
		}
	}
	if info, err := os.Stat(backup); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup info = %+v err=%v", info, err)
	}
}

func TestSessionPoliciesPrintsDigestsFromTheTrustedPolicyFile(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	configRoot := t.TempDir()
	profile := filepath.Join(configRoot, "codex", "profiles", "work")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_CONFIG_DIR", configRoot)
	conf := filepath.Join(t.TempDir(), "coop.conf")
	if err := os.WriteFile(conf, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_CONF", conf)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	policyRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(policyRoot, "session-policies.yaml")
	body := "version: 1\npolicies:\n" +
		"  write:\n    repository: " + repo + "\n    target: codex@work\n" +
		"    max_turns: 20\n    max_queued_turns: 10\n    max_queued_bytes: 4096\n" +
		"    max_patch_bytes: 8192\n    turn_timeout: 1h\n" +
		"  read:\n    repository: " + repo + "\n    repository_read_only: true\n" +
		"    target: codex@work\n    max_turns: 5\n    max_queued_turns: 2\n" +
		"    max_queued_bytes: 2048\n    max_patch_bytes: 4096\n    turn_timeout: 30m\n"
	if err := os.WriteFile(policyPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var code int
	var runErr error
	output := captureStdout(t, func() {
		code, runErr = (&app{cfg: cfg}).cmdSessions([]string{"policies", "--policies", policyPath, "--json"})
	})
	if runErr != nil || code != 0 {
		t.Fatalf("sessions policies = code %d err %v output %q", code, runErr, output)
	}
	var result sessionPoliciesResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode sessions policies output %q: %v", output, err)
	}
	loaded, err := sessionsvc.LoadPolicies(policyPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.PolicyFile != policyPath || len(result.PolicyDigests) != len(loaded) ||
		len(result.PolicyAuthorityDigests) != len(loaded) {
		t.Fatalf("sessions policies result = %+v", result)
	}
	for name, policy := range loaded {
		if got, want := result.PolicyDigests[name], sessionsvc.ResolvedPolicyDigest(policy); got != want {
			t.Errorf("policy %q digest = %q, want %q", name, got, want)
		}
		if got, want := result.PolicyAuthorityDigests[name], sessionsvc.ResolvedPolicyAuthorityDigest(policy); got != want {
			t.Errorf("policy %q authority digest = %q, want %q", name, got, want)
		}
	}
}

// `coop sessions policies` lists two facts per policy; each policy is a labeled block, never an
// unlabeled tab row (entity-blocks-with-labeled-fields).
func TestRenderSessionPoliciesUsesLabeledBlocks(t *testing.T) {
	var out bytes.Buffer
	renderSessionPolicies(&out, ui.Palette{}, sessionPoliciesResult{
		PolicyFile:             "/etc/coop/session-policies.yaml",
		PolicyDigests:          map[string]string{"observe": "aaaa", "engineer": "bbbb"},
		PolicyAuthorityDigests: map[string]string{"observe": "cccc", "engineer": "dddd"},
	}, []string{"engineer", "observe"})
	got := out.String()
	for _, want := range []string{
		"Policy file: /etc/coop/session-policies.yaml",
		"\nengineer\n  Policy digest:     bbbb\n  Authority digest:  dddd\n",
		"\nobserve\n  Policy digest:     aaaa\n  Authority digest:  cccc\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("policies output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\t") {
		t.Fatalf("policies output must not fall back to tab rows:\n%s", got)
	}
}
