package box

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestComposeRejectsAmbientHostAuthority(t *testing.T) {
	for name, body := range map[string]string{
		"variable bind":            "volumes: [\"${OUTSIDE}:/data\"]",
		"long variable bind":       "volumes: [{type: bind, source: '${OUTSIDE}', target: /data}]",
		"bare environment":         "environment: [HOST_TOKEN]",
		"null environment":         "environment: {HOST_TOKEN: null}",
		"environment substitution": "environment: {TOKEN: '${HOST_TOKEN}'}",
		"command substitution":     "command: ['echo', '$HOST_TOKEN']",
		"label substitution":       "labels: {token: '${HOST_TOKEN:-fallback}'}",
		"automatic host port":      "ports: ['5432']",
		"integer host port":        "ports: [5432]",
		"long automatic port":      "ports: [{target: 5432}]",
		"malformed port":           "ports: [true]",
		"escaped then variable":    "command: ['echo', '$$$HOST_TOKEN']",
		"YAML escaped variable":    "command: [\"\\u0024HOST_TOKEN\"]",
		"anchored variable":        "environment: {TOKEN: &token '$HOST_TOKEN', OTHER: *token}",
		"binary environment":       "environment: {TOKEN: !!binary JENPT1BfQVVESVRfQ0FOQVJZ}",
		"binary command":           "command: [!!binary JENPT1BfQVVESVRfQ0FOQVJZ]",
		"key alias as value":       "labels: {&key '$HOST_TOKEN': literal, other: *key}",
	} {
		t.Run(name, func(t *testing.T) {
			repo, path := writeCompose(t, "services:\n  db:\n    image: postgres:18\n    "+body+"\n")
			if err := ValidateComposeFile(path, repo, false); err == nil {
				t.Fatal("accepted ambient host authority")
			}
		})
	}
}

func TestComposePreservesExplicitContainerValues(t *testing.T) {
	repo, path := writeCompose(t, `services:
  db:
    image: postgres:18
    environment: ["PASSWORD=$$literal", "EMPTY=", "PRICE=5$", "BRACED=$${HOST_TOKEN}"]
    command: ["sh", "-c", "echo $$PASSWORD"]
    labels: {"$LITERAL_KEY": !!binary bGl0ZXJhbA==}
    ports: [{target: 5432, host_ip: 127.0.0.1}]
    expose: [5432]
`)
	if err := ValidateComposeFile(path, repo, false); err != nil {
		t.Fatal(err)
	}
}

func TestComposeArtifactsStayOutsideWorkspace(t *testing.T) {
	repo, source := writeCompose(t, "services:\n  db:\n    image: postgres:18\n")
	t.Setenv("TMPDIR", repo)
	if _, cleanup, _, err := snapshotComposeArgs(repo, source, false); err == nil {
		cleanup()
		t.Fatal("approved snapshot was published in writable workspace")
	}
	if _, cleanup, err := writeServiceOverride([]ServicePort{{Service: "db", ContainerPort: 5432, HostPort: 25432}}, repo); err == nil {
		cleanup()
		t.Fatal("generated override was published in writable workspace")
	}
}

func TestComposeArtifactsDoNotRetainMutableTempAlias(t *testing.T) {
	repo, source := writeCompose(t, "services:\n  db:\n    image: postgres:18\n")
	external, replacement := t.TempDir(), t.TempDir()
	alias := filepath.Join(repo, "temporary")
	if err := os.Symlink(external, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", alias)
	args, cleanup, _, err := snapshotComposeArgs(repo, source, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	path := args[len(args)-1]
	override, cleanupOverride, err := writeServiceOverride([]ServicePort{{Service: "db", ContainerPort: 5432, HostPort: 25432}}, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupOverride()
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, alias); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []string{path, override} {
		if strings.HasPrefix(artifact, alias+string(filepath.Separator)) {
			t.Fatalf("private artifact retains mutable alias: %s", artifact)
		}
		if _, err := os.ReadFile(artifact); err != nil {
			t.Fatalf("alias replacement redirected approved artifact: %v", err)
		}
	}
	cleanup()
	cleanupOverride()
	for _, artifact := range []string{path, override} {
		if _, err := os.Stat(artifact); !os.IsNotExist(err) {
			t.Fatalf("cleanup did not remove original private artifact: %v", err)
		}
	}
	if _, err := os.Stat(replacement); err != nil {
		t.Fatalf("cleanup touched the replacement target: %v", err)
	}
}

func TestComposeRejectsUnreadSourceAuthority(t *testing.T) {
	t.Run("second document", func(t *testing.T) {
		repo, path := writeCompose(t, "services:\n  db:\n    image: postgres:18\n---\nservices:\n  x:\n    image: alpine\n    privileged: true\n")
		if err := ValidateComposeFile(path, repo, false); err == nil {
			t.Fatal("accepted an unvalidated second document")
		}
	})
	t.Run("outside source symlink", func(t *testing.T) {
		_, outside := writeCompose(t, "services:\n  db:\n    image: postgres:18\n")
		repo := t.TempDir()
		path := filepath.Join(repo, "compose.yml")
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
		if err := ValidateComposeFile(path, repo, false); err == nil {
			t.Fatal("accepted a Compose source outside repository authority")
		}
	})
	t.Run("bounded regular file", func(t *testing.T) {
		repo, path := writeCompose(t, strings.Repeat("# comment\n", maxComposeFileBytes/8))
		if err := ValidateComposeFile(path, repo, false); err == nil {
			t.Fatal("accepted oversized Compose input")
		}
		fifo := filepath.Join(repo, "fifo.yml")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ValidateComposeFile(fifo, repo, false); err == nil {
			t.Fatal("accepted non-regular Compose input")
		}
	})
}

func TestComposeLaunchKeepsValidatedBytes(t *testing.T) {
	approved := "services:\n  db:\n    image: postgres:18\n"
	repo, source := writeCompose(t, approved)
	captured := filepath.Join(t.TempDir(), "launched.yml")
	shim := filepath.Join(t.TempDir(), "runtime")
	// Mutate the repository source during port discovery, before service discovery and up.
	script := "#!/bin/sh\nset -eu\nargs=$*\nfile=\n" +
		"while [ $# -gt 0 ]; do\n  if [ \"$1\" = -f ]; then shift; file=$1; break; fi\n  shift\ndone\n" +
		"case \"$args\" in\n" +
		"  *'config --format json'*) printf '%s\\n' 'services: {db: {image: alpine, privileged: true}}' > " + strconv.Quote(source) + "; printf '%s\\n' '{\"services\":{\"db\":{}}}' ;;\n" +
		"  *'config --services'*) printf '%s\\n' db ;;\n" +
		"  *'up -d --wait --remove-orphans'*) cp \"$file\" " + strconv.Quote(captured) + " ;;\n" +
		"esac\n"
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureServicesFile(runtime.Runtime{Name: shim}, repo, source, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(captured)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != approved {
		t.Fatalf("launch reread mutable repository input: %s", data)
	}
	changed, err := os.ReadFile(source)
	if err != nil || !strings.Contains(string(changed), "privileged") {
		t.Fatalf("fixture did not replace the source: %q, %v", changed, err)
	}
}

func TestRunReusesStartedServicePorts(t *testing.T) {
	repo, _ := writeCompose(t, "services:\n  db:\n    image: postgres:18\n    expose: [5432]\n")
	recorder := filepath.Join(t.TempDir(), "runtime.log")
	started := filepath.Join(t.TempDir(), "started")
	shim := filepath.Join(t.TempDir(), "runtime")
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> " + strconv.Quote(recorder) + "\n" +
		"case \"$*\" in\n" +
		"  *'config --format json'*)\n" +
		"    if [ -e " + strconv.Quote(started) + " ]; then\n" +
		"      printf '%s\\n' '{\"services\":{\"other\":{\"expose\":[\"80\"]}}}'\n" +
		"    else\n      printf '%s\\n' '{\"services\":{\"db\":{\"expose\":[\"5432\"]}}}'\n    fi ;;\n" +
		"  *'config --services'*) printf '%s\\n' db ;;\n" +
		"  *'up -d --wait --remove-orphans'*) touch " + strconv.Quote(started) + " ;;\n" +
		"esac\n"
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "open", AutoUp: true}
	code, err := Run(cfg, runtime.Runtime{Name: shim}, RunSpec{
		Image: "i", Repo: repo, Cmd: []string{"true"}, Network: true, Batch: true, Quiet: true,
	})
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v", code, err)
	}
	data, err := os.ReadFile(recorder)
	if err != nil {
		t.Fatal(err)
	}
	calls := string(data)
	if strings.Count(calls, "config --format json") != 1 ||
		!strings.Contains(calls, "COOP_SERVICE_DB_URL=") || strings.Contains(calls, "COOP_SERVICE_OTHER_URL=") {
		t.Fatalf("box ports diverged from started services:\n%s", calls)
	}
}
