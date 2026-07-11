package box

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/scaffold"
)

// writeCompose writes body to compose.agent.yml in a fresh temp repo and returns the repo + path.
func writeCompose(t *testing.T, body string) (repo, path string) {
	t.Helper()
	repo = t.TempDir()
	path = filepath.Join(repo, "compose.agent.yml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo, path
}

// The real scaffolded postgres+redis file — coop's own output — must validate.
func TestValidateComposeScaffolded(t *testing.T) {
	repo := t.TempDir()
	if err := scaffold.WriteCompose(repo, []string{"postgres", "redis"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateComposeFile(filepath.Join(repo, "compose.agent.yml"), repo); err != nil {
		t.Fatalf("coop's own scaffolded compose file must pass validation: %v", err)
	}
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
		"container-only port + expose": `services:
  db:
    image: postgres:18
    ports: ["5432"]
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
			if err := ValidateComposeFile(path, repo); err != nil {
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
			if err := ValidateComposeFile(path, repo); err == nil {
				t.Errorf("expected rejection, got nil for:\n%s", body)
			}
		})
	}
}

// A symlink inside the repo pointing OUTSIDE it must not smuggle a host path past the bind check.
func TestValidateComposeSymlinkEscape(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir() // a sibling temp dir, not under repo
	link := filepath.Join(repo, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	path := filepath.Join(repo, "compose.agent.yml")
	os.WriteFile(path, []byte("services:\n  x:\n    image: a\n    volumes: [\"./escape/secrets:/x\"]\n"), 0o644)
	if err := ValidateComposeFile(path, repo); err == nil {
		t.Fatal("a bind through a symlink that escapes the repo must be rejected")
	}
}

func TestValidateComposeMalformed(t *testing.T) {
	repo, path := writeCompose(t, "services: [this is not a map]\n")
	if err := ValidateComposeFile(path, repo); err == nil {
		t.Error("malformed compose (services not a mapping) must be rejected")
	}
}
