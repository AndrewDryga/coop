package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A secret-looking bind stays a decoy until a human approves the compose file's exact content;
// the approval is content-keyed, so editing the file hides the path again.
func TestServiceSecretApprovalIsBoundToTheComposeContent(t *testing.T) {
	t.Setenv(ServiceApprovalRootEnv, t.TempDir())
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

	// A file that binds nothing secret-looking has nothing to review and records nothing.
	write(".agent/compose.yml", "services:\n  db:\n    image: postgres:18\n    volumes: [\"../realm.json:/r:ro\"]\n")
	if review, err := ReviewServiceSecrets(repo, compose); err != nil || review != nil {
		t.Fatalf("plain compose review = %+v, err=%v; want nil", review, err)
	}
	if entries, _ := os.ReadDir(os.Getenv(ServiceApprovalRootEnv)); len(entries) != 1 {
		t.Fatalf("approval store has %d entries, want only the one approval", len(entries))
	}
}
