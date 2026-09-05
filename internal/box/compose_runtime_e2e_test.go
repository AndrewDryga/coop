//go:build boxruntimee2e

package box

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/runtime"
)

// Config resolution exercises real Compose semantics without launching services or publishing ports.
func TestRuntimeComposeSnapshot(t *testing.T) {
	t.Setenv("literal", "synthetic-host-value-must-not-be-imported")
	t.Setenv("COOP_AUDIT_UNSET", "")
	rt, err := runtime.Detect(os.Getenv("COOP_RUNTIME"))
	if err != nil {
		t.Fatal(err)
	}
	repo, source := writeCompose(t, `services:
  db:
    image: postgres:18
    environment: ["PASSWORD=$$literal", "EMPTY="]
    volumes: ["./data:/data:ro"]
    ports: [{target: 5432, host_ip: 127.0.0.1}]
`)
	args, cleanup, err := snapshotComposeArgs(repo, source)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.WriteFile(filepath.Join(filepath.Dir(source), ".env"),
		[]byte("BAD=${COOP_AUDIT_UNSET:?implicit env file must not be read}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := runCompose(rt, &out, &stderr, "config", append(args, "config", "--format", "json")); err != nil {
		t.Fatalf("resolve snapshot: %v: %s", err, stderr.String())
	}
	var doc struct {
		Services map[string]struct {
			Environment map[string]string
			Volumes     []struct{ Source string }
			Ports       []struct {
				HostIP string `json:"host_ip"`
			}
		}
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	db := doc.Services["db"]
	// Compose's rendered model retains the escape for reuse as a Compose input.
	if db.Environment["PASSWORD"] != "$$literal" || db.Environment["EMPTY"] != "" {
		t.Fatalf("literal environment changed: %+v", db.Environment)
	}
	if len(db.Volumes) != 1 || db.Volumes[0].Source != filepath.Join(filepath.Dir(source), "data") {
		t.Fatalf("snapshot changed relative bind anchor: %+v", db.Volumes)
	}
	if len(db.Ports) != 1 || db.Ports[0].HostIP != "127.0.0.1" {
		t.Fatalf("automatic publication lost loopback: %+v", db.Ports)
	}
}
