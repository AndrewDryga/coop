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
	"github.com/AndrewDryga/coop/internal/egress"
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

func TestSessionCLIPathsUseConfiguredDefaults(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for name, xdg := range map[string]string{"home fallback": "", "XDG config home": t.TempDir()} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", xdg)
			state, policy, socket, err := sessionCLIPaths("", "", "")
			if err != nil {
				t.Fatal(err)
			}
			configHome := filepath.Join(home, ".config")
			if xdg != "" {
				configHome = xdg
			}
			if !strings.HasPrefix(state, filepath.Join(home, ".local", "state", "coop", "sessions")) || policy != filepath.Join(configHome, "coop", "session-policies.yaml") || socket != filepath.Join(state, "control.sock") {
				t.Fatalf("defaults = state %q policy %q socket %q", state, policy, socket)
			}
		})
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
		// The reach is RESOLVED against this host, exactly as the daemon publishes it. An open
		// policy reports its mode and no fingerprint: there is nothing captured for one to pin.
		network := result.PolicyNetworks[name]
		if network.Mode != string(egress.Open) || network.Fingerprint != "" || network.Unresolved != "" {
			t.Errorf("policy %q network = %+v; want the resolved open mode and no fingerprint", name, network)
		}
	}
}

// A policy whose network this host cannot resolve still lists — with the reason in place of the
// fingerprint. The daemon refuses to SERVE it; this read is where the operator sees why.
func TestSessionPolicyNetworkReportsWhatItCannotResolve(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	real, err := filepath.EvalSymlinks(repo)
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
	body := "version: 1\npolicies:\n  filtered:\n    repository: " + real + "\n" +
		"    target: codex@work\n    max_turns: 5\n    max_queued_turns: 2\n" +
		"    max_queued_bytes: 2048\n    max_patch_bytes: 4096\n    turn_timeout: 30m\n" +
		"    egress:\n      mode: filtered\n      rules:\n        - to: {domain: example.com}\n" +
		"          protocol: tls\n          ports: [443]\n"
	policyRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(policyRoot, "session-policies.yaml")
	if err := os.WriteFile(policyPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	policies, err := sessionsvc.LoadPolicies(policyPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	network, snapshot := sessionPolicyNetworkOf(cfg, policies["filtered"])
	if network.Mode != string(egress.Filtered) || network.Fingerprint != "" || !strings.Contains(network.Unresolved, "coop net setup") {
		t.Fatalf("unresolvable policy network = %+v; want no fingerprint and the reason", network)
	}
	// The human view raises the unresolved reach as an ISSUE instead of listing rules it could not
	// compile — a partial list would read as this configuration's complete access.
	var out bytes.Buffer
	view := sessionConfigurationViewOf("filtered", policies["filtered"], network, snapshot)
	renderSessionConfigurations(&out, ui.Palette{}, policyPath, []sessionConfigurationView{view})
	got := out.String()
	for _, want := range []string{"⚠ Network access is not ready", "coop net setup"} {
		if !strings.Contains(got, want) {
			t.Fatalf("unresolved configuration lacks %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"Network access needs approval", "coop approve", "Network  ", "example.com"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("setup failure contains %q:\n%s", forbidden, got)
		}
	}

	if err := os.MkdirAll(filepath.Join(real, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	silent := policies["filtered"]
	silent.Egress = sessionsvc.EgressPolicy{}
	projectFile := filepath.Join(real, ".agent", "project.yaml")
	if err := os.WriteFile(projectFile, []byte("box:\n  egress: filtered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inherited, _ := sessionPolicyNetworkOf(cfg, silent)
	if inherited.Mode != string(egress.Filtered) || inherited.Unresolved == "" {
		t.Fatalf("inherited filtered failure = %+v", inherited)
	}

	if err := os.WriteFile(projectFile, []byte("box:\n  egress: open\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pending, pendingSnapshot := sessionPolicyNetworkOf(cfg, silent)
	out.Reset()
	renderSessionConfigurations(&out, ui.Palette{}, policyPath,
		[]sessionConfigurationView{sessionConfigurationViewOf("filtered", silent, pending, pendingSnapshot)})
	got = out.String()
	for _, want := range []string{"⚠ Network access needs approval", "Run coop approve in " + real + ".\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("pending approval lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Network  ") || strings.Contains(got, "example.com") {
		t.Fatalf("an unresolved configuration must not print a rule summary:\n%s", got)
	}
}
