package box

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func serviceReviewRuntime(t *testing.T, recorder string) runtime.Runtime {
	t.Helper()
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "unix:///fixture.sock")
	path := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n"
	if recorder != "" {
		script += "echo \"$@\" >> " + strconv.Quote(recorder) + "\n"
	}
	script += `for last; do :; done
case "$*" in
  *"info --format"*) printf '{"ID":"%s","OSType":"linux","Architecture":"amd64","ServerVersion":"29.1","KernelVersion":"fixture","SecurityOptions":[]}\n' "${COOP_TEST_DAEMON:-fixture-daemon}" ;;
  *"volume inspect"*)
	if [ -n "$COOP_TEST_VOLUME_STATE" ] && [ ! -f "$COOP_TEST_VOLUME_STATE" ]; then exit 1; fi
    options='{}'
    if [ "$COOP_TEST_VOLUME_OPTS" = bind ]; then options='{"type":"none","o":"bind","device":"/"}'; fi
    printf '{"Name":"%s","Driver":"local","Scope":"local","Mountpoint":"/var/lib/docker/volumes/%s/_data","CreatedAt":"%s","Options":%s}\n' "$last" "$last" "${COOP_TEST_VOLUME_CREATED:-2026-01-01T00:00:00Z}" "$options" ;;
	*"volume ls"*) if [ -z "$COOP_TEST_VOLUME_STATE" ] || [ -f "$COOP_TEST_VOLUME_STATE" ]; then printf '"%s"\n' "$last"; fi ;;
	*"volume create"*) if [ -n "$COOP_TEST_VOLUME_STATE" ]; then printf '%s\n' "$last" > "$COOP_TEST_VOLUME_STATE"; fi; echo "$last" ;;
  *"config --services"*) echo db ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: path}
}

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
	if err != nil || review == nil || review.File != ".agent/compose.yml" || strings.Join(review.Paths(), ",") != "certs/tls.key" {
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
	approval, ok := ApprovedServiceSecrets(repo, compose, []byte(composeBody))
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
	if err != nil || review == nil || strings.Join(review.Paths(), ",") != "certs/tls.key" {
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
	if err != nil || later == nil || strings.Join(later.Paths(), ",") != "certs/.env" {
		t.Fatalf("follow-up review = %+v, err=%v; want only the new file to approve", later, err)
	}
	if err := later.Approve(); err != nil {
		t.Fatal(err)
	}
	if approval, ok := ApprovedServiceSecrets(repo, compose, []byte(readFileString(t, compose))); !ok || strings.Join(approval.Paths, ",") != "certs/.env,certs/tls.key" {
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
	entries, _ := os.ReadDir(filepath.Join(os.Getenv(ServiceStateRootEnv), "service-approvals"))
	count := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("approval store has %d records, want one per approved compose content", count)
	}
}

func TestServiceApprovalCannotCrossRepositoriesOrCopiedMarkers(t *testing.T) {
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	body := []byte("services:\n  web:\n    image: nginx:1\n    volumes: [\"./tls.key:/tls.key:ro\"]\n")
	makeRepo := func() (string, string) {
		t.Helper()
		repo := t.TempDir()
		compose := filepath.Join(repo, "compose.yml")
		if err := os.WriteFile(compose, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "tls.key"), []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return repo, compose
	}
	a, fileA := makeRepo()
	b, fileB := makeRepo()
	review, err := ReviewServiceSecrets(a, fileA)
	if err != nil || review == nil {
		t.Fatalf("repository A review = %v, %v", review, err)
	}
	if err := review.Approve(); err != nil {
		t.Fatal(err)
	}
	if _, ok := ApprovedServiceSecrets(a, fileA, body); !ok {
		t.Fatal("repository A lost its approval")
	}
	if _, ok := ApprovedServiceSecrets(b, fileB, body); ok {
		t.Fatal("identical Compose content inherited another repository's approval")
	}
	marker, err := os.ReadFile(filepath.Join(a, serviceApprovalMarker))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, serviceApprovalMarker), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ApprovedServiceSecrets(b, fileB, body); ok {
		t.Fatal("copied service marker inherited another repository's approval")
	}
	if review, err := ReviewServiceSecrets(b, fileB); err != nil || review == nil {
		t.Fatalf("copied marker should still need review: %v, %v", review, err)
	} else if err := review.Approve(); err == nil {
		t.Fatal("approval published against an unmatched copied marker")
	}
	// Replacing the public name cannot restore authority with its bytes.
	if err := os.Remove(filepath.Join(a, serviceApprovalMarker)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, serviceApprovalMarker), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ApprovedServiceSecrets(a, fileA, body); ok {
		t.Fatal("replaced marker inherited the old approval")
	}
}

func TestWritableSecretBindsAndSecretDirectoriesCannotBeApproved(t *testing.T) {
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	for _, tc := range []struct{ name, bind, blocked string }{
		{"direct writable file", `./tls.key:/key`, "tls.key"},
		{"writable parent", `./certs:/certs`, "certs/tls.key"},
		{"secret directory", `./.ssh:/ssh:ro`, ".ssh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.Mkdir(filepath.Join(repo, "certs"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(repo, ".ssh"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, rel := range []string{"tls.key", "certs/tls.key", ".ssh/id_key"} {
				if err := os.WriteFile(filepath.Join(repo, rel), []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			compose := filepath.Join(repo, "compose.yml")
			body := "services:\n  app:\n    image: app:1\n    volumes: [\"" + tc.bind + "\"]\n"
			if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			review, err := ReviewServiceSecrets(repo, compose)
			if err != nil || review == nil || len(review.Files) != 0 || len(review.Blocked) != 1 || review.Blocked[0].Path != tc.blocked {
				t.Fatalf("unsafe secret review = %+v, %v", review, err)
			}
			if err := review.Approve(); err == nil {
				t.Fatal("unsafe secret source was approved")
			}
			_, cleanup, hidden, err := snapshotComposeArgs(repo, compose, false)
			if err != nil || !slices.Contains(hidden, tc.blocked) {
				t.Fatalf("unsafe source lost its decoy: %v, %v", hidden, err)
			}
			cleanup()
			if tc.name == "secret directory" {
				if err := os.WriteFile(filepath.Join(repo, ".ssh", "later.key"), []byte("new key"), 0o600); err != nil {
					t.Fatal(err)
				}
				_, cleanup, hidden, err = snapshotComposeArgs(repo, compose, false)
				if err != nil || !slices.Contains(hidden, ".ssh") {
					t.Fatalf("new child escaped hidden directory: %v, %v", hidden, err)
				}
				cleanup()
			}
		})
	}
}

func TestExternalVolumeApprovalNamesActualAccessAndExpiresOnEdit(t *testing.T) {
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	rt := serviceReviewRuntime(t, "")
	repo := t.TempDir()
	compose := filepath.Join(repo, "compose.yml")
	body := "services:\n  db:\n    image: postgres:18\n    volumes: [\"customer:/data:ro\"]\n  writer:\n    image: alpine\n    volumes:\n      - type: volume\n        source: customer\n        target: /backup\nvolumes:\n  customer:\n    name: actual-customer-data\n"
	if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := ReviewServiceStart(repo, compose, rt)
	if err != nil || len(review.Volumes) != 1 || review.Volumes[0].Name != "actual-customer-data" ||
		!review.Volumes[0].Writable || strings.Join(review.Volumes[0].Consumers, ",") != "db → /data,writer → /backup" {
		t.Fatalf("volume review = %+v, %v", review, err)
	}
	if err := review.ApproveVolumes(); err != nil {
		t.Fatal(err)
	}
	if again, err := ReviewServiceStart(repo, compose, rt); err != nil || again.VolumeApprovalNeeded {
		t.Fatalf("unchanged review = %+v, %v", again, err)
	}
	for _, changed := range []string{
		strings.Replace(body, "actual-customer-data", "different-data", 1),
		strings.Replace(body, "customer:/data:ro", "customer:/other:ro", 1),
		strings.Replace(body, "customer:/data:ro", "customer:/data", 1),
	} {
		if err := os.WriteFile(compose, []byte(changed), 0o644); err != nil {
			t.Fatal(err)
		}
		if again, err := ReviewServiceStart(repo, compose, rt); err != nil || !again.VolumeApprovalNeeded {
			t.Fatalf("edited volume inherited approval: %+v, %v", again, err)
		}
	}
}

func TestVolumeApprovalRefusesChangedDaemonOrVolumeObject(t *testing.T) {
	for _, change := range []string{"endpoint", "daemon", "created-at", "bind-backed"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv(ServiceStateRootEnv, t.TempDir())
			recorder := filepath.Join(t.TempDir(), "runtime.log")
			rt := serviceReviewRuntime(t, recorder)
			repo := t.TempDir()
			compose := filepath.Join(repo, "compose.yml")
			body := "services:\n  db:\n    image: postgres:18\n    volumes: [customer:/data:ro]\nvolumes:\n  customer:\n    external: true\n    name: customer-data\n"
			if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			review, err := ReviewServiceStart(repo, compose, rt)
			if err != nil {
				t.Fatal(err)
			}
			if err := review.ApproveVolumes(); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "endpoint":
				t.Setenv("DOCKER_HOST", "unix:///different.sock")
			case "daemon":
				t.Setenv("COOP_TEST_DAEMON", "different-daemon")
			case "created-at":
				t.Setenv("COOP_TEST_VOLUME_CREATED", "2026-02-02T00:00:00Z")
			case "bind-backed":
				t.Setenv("COOP_TEST_VOLUME_OPTS", "bind")
			}
			if _, err := UpServicesReviewed(rt, repo, compose, review, io.Discard, io.Discard); err == nil {
				t.Fatal("changed Docker volume authority reached Compose")
			}
			if calls, err := os.ReadFile(recorder); err == nil && strings.Contains(string(calls), " up ") {
				t.Fatalf("Compose ran after authority changed: %s", calls)
			}
			if next, err := ReviewServiceStart(repo, compose, rt); change == "bind-backed" {
				if err == nil || next != nil {
					t.Fatalf("bind-backed local volume became approvable: %+v, %v", next, err)
				}
			} else if err != nil || !next.VolumeApprovalNeeded {
				t.Fatalf("changed Docker object reused saved grant: %+v, %v", next, err)
			}
		})
	}
}

func TestApprovedCustomVolumeIsCreatedPlainAndBoundToItsObject(t *testing.T) {
	t.Setenv(ServiceStateRootEnv, t.TempDir())
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	state := filepath.Join(t.TempDir(), "created")
	t.Setenv("COOP_TEST_VOLUME_STATE", state)
	rt := serviceReviewRuntime(t, recorder)
	repo := t.TempDir()
	compose := filepath.Join(repo, "compose.yml")
	body := "services:\n  db:\n    image: postgres:18\n    volumes: [customer:/data]\nvolumes:\n  customer:\n    name: customer-data\n"
	if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	review, err := ReviewServiceStart(repo, compose, rt)
	if err != nil || !review.VolumeApprovalNeeded || !slices.Equal(review.NewVolumes, []string{"customer-data"}) {
		t.Fatalf("absent custom volume review = %+v, %v", review, err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("review created the volume before approval: %v", err)
	}
	if err := review.ApproveVolumes(); err != nil {
		t.Fatal(err)
	}
	if identity, ok := review.VolumeIdentity("customer-data"); !ok || identity.CreatedAt == "" {
		t.Fatalf("created volume identity = %+v, %t", identity, ok)
	}
	if next, err := ReviewServiceStart(repo, compose, rt); err != nil || next.VolumeApprovalNeeded {
		t.Fatalf("created plain volume required repeat review: %+v, %v", next, err)
	}
	if calls, err := os.ReadFile(recorder); err != nil || !strings.Contains(string(calls), "volume create --driver local customer-data") {
		t.Fatalf("approved volume was not created plain: %v\n%s", err, calls)
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
	sections := newLaunchSections(RunSpec{Agent: "gemini"})
	sections.internet(&config.Config{Egress: "open"}, RunSpec{Agent: "gemini"}, nil)
	_, runErr := startServicesFile(runtime.Runtime{Name: shim}, repo, compose, io.Discard, &composeWriter, false, true)
	sections.servicesFailed("Container project-db Running\nContainer project-keycloak Waiting")
	os.Stderr = old
	w.Close()
	var seen bytes.Buffer
	if _, err := io.Copy(&seen, r); err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("start = %v", runErr)
	}
	if !strings.Contains(seen.String(), "empty source in place of tls.key") {
		t.Fatalf("the notice never reached the user:\nstderr: %q\ncompose writer: %q", seen.String(), composeWriter.String())
	}
	if strings.Contains(composeWriter.String(), "empty source in place of") {
		t.Fatalf("the notice went to the compose writer, which a box start discards: %q", composeWriter.String())
	}
	want := "Configuring network access\n  ⚠ Unrestricted — nothing is blocked\n\n" +
		"⚠ services get an empty source in place of tls.key (looks like a secret) — run `coop up` in a terminal to review eligible read-only files; secret directories and writable binds remain hidden\n\n" +
		"⚠ Project services could not start\n\n" +
		"      Container project-db Running\n      Container project-keycloak Waiting\n\n  Run coop up to retry.\n"
	if seen.String() != want {
		t.Fatalf("service warnings lost their separating paragraphs:\ngot %q\nwant %q", seen.String(), want)
	}
}

// The repo can say which files its services genuinely need. That request grants nothing: it labels
// the prompt, so a file nobody asked for stands out, and a file that appeared since the last yes is
// called out even when a compose edit voided the approval that covered it.
func TestReviewLabelsWhatTheRepoAsksForAndWhatIsNew(t *testing.T) {
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
	write("certs/.env", "SMUGGLED=1\n")
	write(".agent/project.yaml", "services:\n  require_real_files:\n    - certs/tls.key\n")
	write(".agent/compose.yml", "services:\n  kc:\n    image: example/kc\n    volumes:\n      - \"../certs:/certs:ro\"\n")
	compose := filepath.Join(repo, ".agent", "compose.yml")

	review, err := ReviewServiceSecrets(repo, compose)
	if err != nil || review == nil || len(review.Files) != 2 {
		t.Fatalf("review = %+v, err = %v; want both hidden files", review, err)
	}
	byPath := map[string]ReviewFile{}
	for _, f := range review.Files {
		byPath[f.Path] = f
	}
	if key := byPath["certs/tls.key"]; !key.Requested || key.New || key.Reason() != ".agent/project.yaml asks for it" {
		t.Fatalf("requested file = %+v, reason %q", key, key.Reason())
	}
	if env := byPath["certs/.env"]; env.Requested || env.New || env.Reason() != "nothing in the repo asks for it" {
		t.Fatalf("unrequested file = %+v, reason %q", env, env.Reason())
	}
	// The list keeps the files' own order, so a reader can follow it against their Compose file;
	// what deserves a second look is carried by each path's LABEL, not by its position (the
	// approved prompt in the CLI design shows exactly that shape).
	if got, want := review.Paths(), []string{"certs/.env", "certs/tls.key"}; !slices.Equal(got, want) {
		t.Fatalf("review order = %v, want the files' own order %v", got, want)
	}
	if err := review.Approve(); err != nil {
		t.Fatal(err)
	}

	// An agent edits the compose file, which voids the approval, and a third secret appears. The
	// human is asked again: the two they already vouched for read as known, the third as new.
	write("certs/extra.pem", "-----BEGIN PRIVATE KEY-----\n")
	write(".agent/compose.yml", "services:\n  kc:\n    image: example/kc\n    volumes:\n      - \"../certs:/certs:ro\"\n    environment:\n      X: \"1\"\n")
	again, err := ReviewServiceSecrets(repo, compose)
	if err != nil || again == nil || len(again.Files) != 3 {
		t.Fatalf("review after the edit = %+v, err = %v; want every file asked again", again, err)
	}
	byPath = map[string]ReviewFile{}
	for _, f := range again.Files {
		byPath[f.Path] = f
	}
	if byPath["certs/tls.key"].New || byPath["certs/.env"].New {
		t.Fatalf("already-approved paths read as new: %+v", again.Files)
	}
	// Still the files' own order; "new since your last approval" is a label on the path, not a
	// promotion to the top of the list.
	if got, want := again.Paths(), []string{"certs/.env", "certs/extra.pem", "certs/tls.key"}; !slices.Equal(got, want) {
		t.Fatalf("review order after the edit = %v, want the files' own order %v", got, want)
	}
	extra := byPath["certs/extra.pem"]
	if !extra.New || extra.Requested || extra.Reason() != "nothing in the repo asks for it; new since you last approved this file" {
		t.Fatalf("the new file = %+v, reason %q", extra, extra.Reason())
	}
}
