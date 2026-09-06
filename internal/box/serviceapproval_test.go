package box

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/runtime"
)

// A secret-looking bind stays a decoy until a human approves the compose file's exact content;
// the approval is content-keyed, so editing the file hides the path again.
func TestServiceSecretApprovalIsBoundToTheComposeContent(t *testing.T) {
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	repo := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("certs/tls.key", "-----BEGIN PRIVATE KEY-----\n")
	write("certs/tls.crt", "-----BEGIN CERTIFICATE-----\n")
	write("realm.json", "{}\n")
	composeBody := `services:
  keycloak:
    image: quay.io/keycloak/keycloak:26
    volumes:
      - "../certs/tls.crt:/certs/tls.crt:ro"
      - "../certs/tls.key:/certs/tls.key:ro"
      - "../realm.json:/opt/realm.json:ro"
`
	write(".agent/compose.yml", composeBody)
	compose := filepath.Join(repo, ".agent", "compose.yml")

	hasShadow := func(args []string) bool {
		for _, a := range args {
			if strings.HasSuffix(a, "coop-compose-override-shadow.yml") {
				return true
			}
		}
		return false
	}
	args, cleanup, hidden, err := snapshotComposeArgs(repo, compose, false)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if strings.Join(hidden, ",") != "certs/tls.key" || !hasShadow(args) {
		t.Fatalf("before approval: hidden=%v shadow=%v; want the key hidden behind a decoy", hidden, hasShadow(args))
	}

	review, err := ReviewServiceSecrets(repo, compose)
	if err != nil || review == nil || review.File != ".agent/compose.yml" || strings.Join(review.Hidden, ",") != "certs/tls.key" {
		t.Fatalf("review = %+v, err=%v; want the key listed for the human", review, err)
	}
	if err := review.Approve(); err != nil {
		t.Fatal(err)
	}
	if again, err := ReviewServiceSecrets(repo, compose); err != nil || again != nil {
		t.Fatalf("after approval review = %+v, err=%v; want nothing left to ask", again, err)
	}
	args, cleanup, hidden, err = snapshotComposeArgs(repo, compose, false)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if len(hidden) != 0 || hasShadow(args) {
		t.Fatalf("after approval: hidden=%v shadow=%v; want the service to read the real key", hidden, hasShadow(args))
	}
	approval, ok := ApprovedServiceSecrets([]byte(composeBody))
	if !ok || approval.File != ".agent/compose.yml" || strings.Join(approval.Paths, ",") != "certs/tls.key" || approval.ApprovedAt.IsZero() {
		t.Fatalf("stored approval = %+v, ok=%v", approval, ok)
	}

	// The one way a box can reach these binds is editing the file — which voids the approval.
	write(".agent/compose.yml", composeBody+"  extra:\n    image: alpine\n    volumes: [\"../certs/tls.key:/k:ro\"]\n")
	args, cleanup, hidden, err = snapshotComposeArgs(repo, compose, false)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if strings.Join(hidden, ",") != "certs/tls.key" || !hasShadow(args) {
		t.Fatalf("after an edit: hidden=%v shadow=%v; want the decoy back", hidden, hasShadow(args))
	}
	if review, err := ReviewServiceSecrets(repo, compose); err != nil || review == nil {
		t.Fatalf("after an edit review = %+v, err=%v; want a fresh review", review, err)
	}

	// An approval names exact files. A secret that appears LATER under an approved DIRECTORY bind
	// keeps its decoy: the human never saw it.
	write(".agent/compose.yml", "services:\n  keycloak:\n    image: quay.io/keycloak/keycloak:26\n    volumes:\n      - \"../certs:/certs:ro\"\n")
	review, err = ReviewServiceSecrets(repo, compose)
	if err != nil || review == nil || strings.Join(review.Hidden, ",") != "certs/tls.key" {
		t.Fatalf("directory bind review = %+v, err=%v", review, err)
	}
	if err := review.Approve(); err != nil {
		t.Fatal(err)
	}
	write("certs/.env", "STOLEN=1\n")
	args, cleanup, hidden, err = snapshotComposeArgs(repo, compose, false)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if strings.Join(hidden, ",") != "certs/.env" || !hasShadow(args) {
		t.Fatalf("new secret under an approved bind: hidden=%v shadow=%v; want it still hidden", hidden, hasShadow(args))
	}
	later, err := ReviewServiceSecrets(repo, compose)
	if err != nil || later == nil || strings.Join(later.Hidden, ",") != "certs/.env" {
		t.Fatalf("follow-up review = %+v, err=%v; want only the new file to approve", later, err)
	}
	if err := later.Approve(); err != nil {
		t.Fatal(err)
	}
	if approval, ok := ApprovedServiceSecrets([]byte(readFileString(t, compose))); !ok || strings.Join(approval.Paths, ",") != "certs/.env,certs/tls.key" {
		t.Fatalf("re-approval = %+v, ok=%v; want both files kept", approval, ok)
	}
	_, cleanup, hidden, err = snapshotComposeArgs(repo, compose, false)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if len(hidden) != 0 {
		t.Fatalf("after approving both: hidden=%v", hidden)
	}

	// A file that binds nothing secret-looking has nothing to review and records nothing.
	write(".agent/compose.yml", "services:\n  db:\n    image: postgres:18\n    volumes: [\"../realm.json:/r:ro\"]\n")
	if review, err := ReviewServiceSecrets(repo, compose); err != nil || review != nil {
		t.Fatalf("plain compose review = %+v, err=%v; want nil", review, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(os.Getenv(ServiceStateRootEnv), "service-approvals")); len(entries) != 2 {
		t.Fatalf("approval store has %d entries, want one per approved compose content", len(entries))
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// `compose up -d` leaves the sidecars running long after coop exits, so the decoy a sidecar mounts
// has to survive the cleanup of the private per-start directory. It did not: the sidecar was left
// bound to a deleted path, and what it saw after a restart was whatever the runtime invented.
func TestServiceDecoySourcesOutliveTheComposeCommand(t *testing.T) {
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tls.key"), []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(repo, ".agent", "compose.yml")
	if err := os.WriteFile(compose, []byte("services:\n  kc:\n    image: example/kc\n    volumes:\n      - \"../tls.key:/certs/tls.key:ro\"\n      - \"../.ssh:/ssh:ro\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args, cleanup, hidden, err := snapshotComposeArgs(repo, compose, false)
	if err != nil {
		t.Fatal(err)
	}
	override := ""
	for _, a := range args {
		if strings.HasSuffix(a, "coop-compose-override-shadow.yml") {
			override = a
		}
	}
	if override == "" || len(hidden) != 2 {
		t.Fatalf("args = %v, hidden = %v; want an override hiding both paths", args, hidden)
	}
	body, err := os.ReadFile(override)
	if err != nil {
		t.Fatal(err)
	}
	var sources []string
	for _, line := range strings.Split(string(body), "\n") {
		if _, rest, ok := strings.Cut(strings.TrimSpace(line), "source: "); ok {
			sources = append(sources, strings.Trim(rest, `"`))
		}
	}
	if len(sources) != 2 {
		t.Fatalf("override sources = %v, want one per hidden path:\n%s", sources, body)
	}
	cleanup() // coop is done with the command; the sidecars it started are not
	for _, source := range sources {
		info, err := os.Stat(source)
		if err != nil {
			t.Fatalf("decoy source %s is gone after the command: %v", source, err)
		}
		if info.IsDir() {
			continue
		}
		if info.Size() != 0 {
			t.Fatalf("decoy source %s is not empty", source)
		}
	}
}

// The notice naming a hidden file must reach the user's terminal on EVERY start path. A box start
// hands compose a buffer it reads only when the start fails, so a notice written there is thrown
// away — which is exactly how a shadowed key reached a session as a service that just crashed.
func TestHiddenServiceFileNoticeGoesToTheUserNotTheComposeWriter(t *testing.T) {
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tls.key"), []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(repo, ".agent", "compose.yml")
	if err := os.WriteFile(compose, []byte("services:\n  kc:\n    image: example/kc\n    volumes:\n      - \"../tls.key:/certs/tls.key:ro\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\ncase \"$*\" in *\"config --services\"*) echo kc ;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	var composeWriter bytes.Buffer
	_, runErr := startServicesFile(runtime.Runtime{Name: shim}, repo, compose, io.Discard, &composeWriter, false)
	os.Stderr = old
	w.Close()
	var seen bytes.Buffer
	if _, err := io.Copy(&seen, r); err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("start = %v", runErr)
	}
	if !strings.Contains(seen.String(), "empty file in place of tls.key") {
		t.Fatalf("the notice never reached the user:\nstderr: %q\ncompose writer: %q", seen.String(), composeWriter.String())
	}
	if strings.Contains(composeWriter.String(), "empty file in place of") {
		t.Fatalf("the notice went to the compose writer, which a box start discards: %q", composeWriter.String())
	}
}
