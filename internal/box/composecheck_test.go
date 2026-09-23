package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/project"
)

// writeCompose writes body to a compose file in a fresh temp repo and returns the repo + path.
// (The path is passed to ValidateComposeFile explicitly, so its name doesn't matter here.)
func writeCompose(t *testing.T, body string) (repo, path string) {
	t.Helper()
	repo = t.TempDir()
	path = filepath.Join(repo, filepath.FromSlash(project.DefaultCompose))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo, path
}

func TestValidateComposeAccepts(t *testing.T) {
	cases := map[string]string{
		"named volume + inline env": `services:
  db:
    image: postgres:18
    environment:
      POSTGRES_PASSWORD: pw
    volumes: ["pgdata:/var/lib/postgresql"]
volumes:
  pgdata:
`,
		"loopback host port": `services:
  db:
    image: postgres:18
    ports: ["127.0.0.1:5432:5432"]
`,
		"container-only expose": `services:
  db:
    image: postgres:18
    expose: ["5432"]
`,
		"repo-relative bind": `services:
  db:
    image: postgres:18
    volumes: ["./initdb:/docker-entrypoint-initdb.d:ro"]
`,
		"long-form loopback port": `services:
  db:
    image: postgres:18
    ports:
      - target: 5432
        published: 5432
        host_ip: 127.0.0.1
`,
		"healthcheck + depends_on + restart": `services:
  db:
    image: postgres:18
    restart: unless-stopped
    healthcheck:
      test: ["CMD-SHELL", "pg_isready"]
  app:
    image: myapp:latest
    depends_on: [db]
`,
		"empty external named volume": `services:
  db:
    image: postgres:18
    volumes: ["shared:/data"]
volumes:
  shared:
    external: true
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			repo, path := writeCompose(t, body)
			if err := ValidateComposeFile(path, repo, false); err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

func TestValidateComposeRejects(t *testing.T) {
	// Each body carries exactly one disallowed directive; validation must refuse it.
	cases := map[string]string{
		"privileged":                 "services:\n  x:\n    image: a\n    privileged: true\n",
		"cap_add":                    "services:\n  x:\n    image: a\n    cap_add: [SYS_ADMIN]\n",
		"devices":                    "services:\n  x:\n    image: a\n    devices: [\"/dev/sda:/dev/sda\"]\n",
		"security_opt":               "services:\n  x:\n    image: a\n    security_opt: [\"seccomp:unconfined\"]\n",
		"network_mode host":          "services:\n  x:\n    image: a\n    network_mode: host\n",
		"pid host":                   "services:\n  x:\n    image: a\n    pid: host\n",
		"ipc host":                   "services:\n  x:\n    image: a\n    ipc: host\n",
		"userns_mode":                "services:\n  x:\n    image: a\n    userns_mode: host\n",
		"env_file":                   "services:\n  x:\n    image: a\n    env_file: [../.env]\n",
		"secrets":                    "services:\n  x:\n    image: a\n    secrets: [s]\n",
		"configs":                    "services:\n  x:\n    image: a\n    configs: [c]\n",
		"build":                      "services:\n  x:\n    build: .\n",
		"extends":                    "services:\n  x:\n    image: a\n    extends:\n      file: other.yml\n      service: y\n",
		"include":                    "include:\n  - other.yml\nservices:\n  x:\n    image: a\n",
		"volume driver_opts":         "services:\n  x:\n    image: a\n    volumes: [\"d:/data\"]\nvolumes:\n  d:\n    driver_opts:\n      type: none\n      o: bind\n      device: /etc\n",
		"network host driver":        "services:\n  x:\n    image: a\nnetworks:\n  n:\n    driver: host\n",
		"host bind root":             "services:\n  x:\n    image: a\n    volumes: [\"/:/host\"]\n",
		"host bind ssh":              "services:\n  x:\n    image: a\n    volumes: [\"~/.ssh:/x\"]\n",
		"parent escape bind":         "services:\n  x:\n    image: a\n    volumes: [\"../../etc:/x\"]\n",
		"docker socket":              "services:\n  x:\n    image: a\n    volumes: [\"/var/run/docker.sock:/var/run/docker.sock\"]\n",
		"interp bind":                "services:\n  x:\n    image: a\n    volumes: [\"${HOME}/.ssh:/x\"]\n",
		"bare host port":             "services:\n  x:\n    image: a\n    ports: [\"5432:5432\"]\n",
		"0.0.0.0 port":               "services:\n  x:\n    image: a\n    ports: [\"0.0.0.0:5432:5432\"]\n",
		"lan ip port":                "services:\n  x:\n    image: a\n    ports: [\"192.168.1.5:5432:5432\"]\n",
		"interp port":                "services:\n  x:\n    image: a\n    ports: [\"${IP}:5432:5432\"]\n",
		"long-form 0.0.0.0 port":     "services:\n  x:\n    image: a\n    ports:\n      - target: 5432\n        published: 5432\n",
		"missing image (build-only)": "services:\n  x:\n    command: sleep 1\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			repo, path := writeCompose(t, body)
			if err := ValidateComposeFile(path, repo, false); err == nil {
				t.Errorf("expected rejection, got nil for:\n%s", body)
			}
		})
	}
}

// A symlink inside the repo pointing OUTSIDE it must not smuggle a host path past the bind check.
func TestValidateComposeSymlinkEscape(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir() // a sibling temp dir, not under repo
	path := filepath.Join(repo, filepath.FromSlash(project.DefaultCompose))
	os.MkdirAll(filepath.Dir(path), 0o755)
	// The symlink sits beside the compose file (relative binds resolve against the compose dir).
	link := filepath.Join(filepath.Dir(path), "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	os.WriteFile(path, []byte("services:\n  x:\n    image: a\n    volumes: [\"./escape/secrets:/x\"]\n"), 0o644)
	if err := ValidateComposeFile(path, repo, false); err == nil {
		t.Fatal("a bind through a symlink that escapes the repo must be rejected")
	}
}

func TestValidateComposeMalformed(t *testing.T) {
	repo, path := writeCompose(t, "services: [this is not a map]\n")
	if err := ValidateComposeFile(path, repo, false); err == nil {
		t.Error("malformed compose (services not a mapping) must be rejected")
	}
}

func TestValidateComposeRejectsNestedSourceUnderWritableBind(t *testing.T) {
	body := "services:\n  writer:\n    image: alpine\n    volumes: [\"../data:/data\"]\n  reader:\n    image: alpine\n    volumes: [\"../data/.coopignore:/policy:ro\"]\n"
	repo, path := writeCompose(t, body)
	if err := ValidateComposeFile(path, repo, false); err == nil || !strings.Contains(err.Error(), "nested under writable") {
		t.Fatalf("writable ancestor with read-only nested source = %v, want refusal", err)
	}
	body = strings.Replace(body, "../data:/data", "../data:/data:ro", 1)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateComposeFile(path, repo, false); err != nil {
		t.Fatalf("read-only ancestor should be allowed: %v", err)
	}
}

func TestValidateComposeRejectsCaseAliasedNestedWritableBind(t *testing.T) {
	repo := t.TempDir()
	data := filepath.Join(repo, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "child"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias, err := os.Stat(filepath.Join(repo, "DATA"))
	if os.IsNotExist(err) {
		t.Skip("host filesystem is case-sensitive")
	}
	if err != nil {
		t.Fatal(err)
	}
	parent, err := os.Stat(data)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(parent, alias) {
		t.Skip("host filesystem does not alias case")
	}
	compose := filepath.Join(repo, "compose.yml")
	body := "services:\n  writer:\n    image: alpine\n    volumes: [\"./data:/data\"]\n  reader:\n    image: alpine\n    volumes: [\"./DATA/child:/child:ro\"]\n"
	if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateComposeFile(compose, repo, false); err == nil || !strings.Contains(err.Error(), "nested under writable") {
		t.Fatalf("case-aliased nested bind was accepted: %v", err)
	}
}

// A read-only session keeps its sidecars, but a bind of the repository into one must be
// read-only too; otherwise the sidecar is a write path into a checkout the agent cannot write.
func TestValidateComposeReadOnlyRepo(t *testing.T) {
	accepted := map[string]string{
		"short-form :ro":           "services:\n  x:\n    image: a\n    volumes: [\"./initdb:/docker-entrypoint-initdb.d:ro\"]\n",
		"short-form :ro cached":    "services:\n  x:\n    image: a\n    volumes: [\"./initdb:/docker-entrypoint-initdb.d:ro,cached\"]\n",
		"long-form read_only":      "services:\n  x:\n    image: a\n    volumes:\n      - type: bind\n        source: ./initdb\n        target: /initdb\n        read_only: true\n",
		"long-form no host create": "services:\n  x:\n    image: a\n    volumes:\n      - type: bind\n        source: ./initdb\n        target: /initdb\n        read_only: true\n        bind: {create_host_path: false}\n",
		"named volume stays free":  "services:\n  x:\n    image: a\n    volumes: [\"pgdata:/var/lib/postgresql\"]\nvolumes:\n  pgdata:\n",
	}
	for name, body := range accepted {
		t.Run(name, func(t *testing.T) {
			repo, path := writeCompose(t, body)
			if err := os.Mkdir(filepath.Join(repo, ".agent", "initdb"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := ValidateComposeFile(path, repo, true); err != nil {
				t.Errorf("read-only repo rejected a read-only bind: %v\n%s", err, body)
			}
		})
	}
	rejected := map[string]string{
		"short-form writable": "services:\n  x:\n    image: a\n    volumes: [\"./initdb:/docker-entrypoint-initdb.d\"]\n",
		"short-form rw mode":  "services:\n  x:\n    image: a\n    volumes: [\"./initdb:/docker-entrypoint-initdb.d:rw\"]\n",
		"long-form writable":  "services:\n  x:\n    image: a\n    volumes:\n      - type: bind\n        source: ./initdb\n        target: /initdb\n",
		"repo root writable":  "services:\n  x:\n    image: a\n    volumes: [\".:/work\"]\n",
	}
	for name, body := range rejected {
		t.Run(name, func(t *testing.T) {
			repo, path := writeCompose(t, body)
			err := ValidateComposeFile(path, repo, true)
			if err == nil || !strings.Contains(err.Error(), "repository is read-only") {
				t.Errorf("read-only repo accepted a writable bind (err=%v):\n%s", err, body)
			}
			// The same file is fine for a writable session: the rule is about the session's mode.
			if err := ValidateComposeFile(path, repo, false); err != nil {
				t.Errorf("writable session rejected a repo bind: %v", err)
			}
		})
	}
}

func TestValidateComposeRejectsHostMutatingBindOptions(t *testing.T) {
	cases := map[string]string{
		"short shared relabel":  "volumes: [\"./initdb:/initdb:ro,z\"]",
		"short private relabel": "volumes: [\"./initdb:/initdb:ro,Z\"]",
		"short propagation":     "volumes: [\"./initdb:/initdb:ro,rshared\"]",
		"long relabel":          "volumes: [{type: bind, source: ./initdb, target: /initdb, read_only: true, bind: {selinux: Z}}]",
		"long propagation":      "volumes: [{type: bind, source: ./initdb, target: /initdb, read_only: true, bind: {propagation: rshared}}]",
		"long host create":      "volumes: [{type: bind, source: ./initdb, target: /initdb, read_only: true, bind: {create_host_path: true}}]",
		"ambiguous access":      "volumes: [\"./initdb:/initdb:ro,rw\"]",
	}
	for name, volume := range cases {
		t.Run(name, func(t *testing.T) {
			repo, path := writeCompose(t, "services:\n  x:\n    image: alpine\n    "+volume+"\n")
			if err := os.Mkdir(filepath.Join(repo, ".agent", "initdb"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := ValidateComposeFile(path, repo, true); err == nil {
				t.Fatal("host-mutating bind option was accepted in a read-only session")
			}
			if err := ValidateComposeFile(path, repo, false); err == nil {
				t.Fatal("host-mutating bind option was accepted in a writable session")
			}
		})
	}
}

func TestValidateComposeReadOnlyBindNeedsExistingSource(t *testing.T) {
	for _, volume := range []string{
		"\"./missing:/data:ro\"",
		"{type: bind, source: ./missing, target: /data, read_only: true}",
	} {
		repo, path := writeCompose(t, "services:\n  x:\n    image: alpine\n    volumes: ["+volume+"]\n")
		if err := ValidateComposeFile(path, repo, true); err == nil || !strings.Contains(err.Error(), "must already exist") {
			t.Fatalf("read-only missing bind %s = %v, want pre-launch refusal", volume, err)
		}
		if err := ValidateComposeFile(path, repo, false); err != nil {
			t.Fatalf("writable session should preserve Compose's source creation: %v", err)
		}
	}
}

func TestValidateComposeExplicitBareBindIsHostPath(t *testing.T) {
	write := func(t *testing.T, volume string) (string, string) {
		t.Helper()
		return writeCompose(t, "services:\n  x:\n    image: alpine\n    volumes:\n      - type: bind\n        source: bare\n        target: /data\n"+volume)
	}
	t.Run("writable read-only denial", func(t *testing.T) {
		repo, path := write(t, "")
		if err := os.Mkdir(filepath.Join(repo, ".agent", "bare"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := ValidateComposeFile(path, repo, true); err == nil || !strings.Contains(err.Error(), "repository is read-only") {
			t.Fatalf("bare explicit bind bypassed read-only policy: %v", err)
		}
		if err := ValidateComposeFile(path, repo, false); err != nil {
			t.Fatalf("ordinary writable bind was refused: %v", err)
		}
	})
	t.Run("missing read-only source", func(t *testing.T) {
		repo, path := write(t, "        read_only: true\n")
		if err := ValidateComposeFile(path, repo, true); err == nil || !strings.Contains(err.Error(), "must already exist") {
			t.Fatalf("missing bare bind would let Compose create host path: %v", err)
		}
	})
	t.Run("host-mutating option", func(t *testing.T) {
		repo, path := write(t, "        read_only: true\n        bind: {selinux: Z}\n")
		if err := ValidateComposeFile(path, repo, false); err == nil || !strings.Contains(err.Error(), "host-mutating") {
			t.Fatalf("bare bind escaped option review: %v", err)
		}
	})
	t.Run("nested writable ancestor", func(t *testing.T) {
		repo, path := writeCompose(t, "services:\n  writer:\n    image: alpine\n    volumes: [{type: bind, source: data, target: /data}]\n  reader:\n    image: alpine\n    volumes: [{type: bind, source: data/child, target: /child, read_only: true}]\n")
		if err := os.MkdirAll(filepath.Join(repo, ".agent", "data", "child"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := ValidateComposeFile(path, repo, false); err == nil || !strings.Contains(err.Error(), "nested under writable") {
			t.Fatalf("bare ancestor escaped restart protection: %v", err)
		}
	})
}
