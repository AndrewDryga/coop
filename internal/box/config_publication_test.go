package box

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestConfigPublicationScopeAndHostValidation(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node"}
	ag, _ := agents.Get("claude")
	if err := ag.EnsureDefaults(cfg, "/workspace"); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []RunSpec{{Agent: "claude", Homes: true}, {Agent: "codex", Homes: true, Peers: []agents.Target{{Provider: "claude"}}}} {
		encoded, err := prepareConfigPublication(cfg, spec)
		var files []agents.ConfigPublication
		if err != nil || json.Unmarshal([]byte(encoded), &files) != nil || len(files) != 1 {
			t.Fatalf("scoped publication = %q, %v", encoded, err)
		}
	}
	if encoded, err := prepareConfigPublication(cfg, RunSpec{Agent: "codex", Homes: true}); err != nil || encoded != "" {
		t.Fatalf("out-of-scope publication = %q, %v", encoded, err)
	}
	if err := os.WriteFile(filepath.Join(cfg.AgentDir("claude"), ".claude.json"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareConfigPublication(cfg, RunSpec{Agent: "claude", Homes: true}); err == nil {
		t.Fatal("malformed host config accepted")
	}
}

func TestConfigPublicationOverridesExtraEnv(t *testing.T) {
	cfg := &config.Config{HomeInBox: "/home/node", ConfigDir: t.TempDir()}
	spec := RunSpec{configPublication: "host-witness", ExtraArgs: []string{"-e", "COOP_CONFIG_PUBLICATION=extra"}}
	args := assembleOptions(cfg, false, spec, nil, "", "", "/workspace", ttyNone, false, nil, nil, nil, nil, nil, "", "")
	if slices.Index(args, "COOP_CONFIG_PUBLICATION=host-witness") <= slices.Index(args, "COOP_CONFIG_PUBLICATION=extra") {
		t.Fatal("operator extras override the host publication witness")
	}
}

func TestConfigPublicationEntrypoint(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	for _, scenario := range []string{"matching", "stale-first", "mismatch", "same-length", "symlink", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "config.json")
			data := []byte("{\"text\":\"" + strings.Repeat("x", 887) + "\"}\n")
			if err := os.WriteFile(file, data, 0o600); err != nil {
				t.Fatal(err)
			}
			witness, _ := json.Marshal([]agents.ConfigPublication{{Path: file, Digest: publicationDigest(data)}})
			if scenario == "mismatch" {
				if err := os.WriteFile(file, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "same-length" {
				if err := os.WriteFile(file, []byte(strings.ReplaceAll(string(data), "x", "y")), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "symlink" {
				link := filepath.Join(dir, "link.json")
				if err := os.Symlink(file, link); err != nil {
					t.Fatal(err)
				}
				witness, _ = json.Marshal([]agents.ConfigPublication{{Path: link, Digest: publicationDigest(data)}})
			}
			if scenario == "oversized" {
				f, err := os.OpenFile(file, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				err = f.Truncate(agents.MaxNativeConfigBytes + 1)
				closeErr := f.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("truncate: %v/%v", err, closeErr)
				}
			}
			entry := filepath.Join(dir, "entry")
			script := strings.ReplaceAll(entrypointScript(t), "/usr/local/bin/node", node)
			if err := os.WriteFile(entry, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", entry, "/bin/sh", "-c", `printf launched; test -z "$COOP_CONFIG_PUBLICATION"`)
			cmd.Env = append(os.Environ(), "NODE_OPTIONS=", "COOP_NO_ASDF=1", "COOP_CONFIG_PUBLICATION="+string(witness), "COOP_SUPERVISE_DESCENDANTS=", "COOP_FORWARD=")
			if scenario == "stale-first" {
				preload := filepath.Join(dir, "stale.cjs")
				if err := os.WriteFile(preload, []byte(stalePublicationPreload), 0o600); err != nil {
					t.Fatal(err)
				}
				cmd.Env = append(cmd.Env, "NODE_OPTIONS=--require="+preload)
			}
			out, err := cmd.CombinedOutput()
			if scenario == "matching" || scenario == "stale-first" {
				if err != nil || string(out) != "launched" {
					t.Fatalf("startup = %q, %v", out, err)
				}
			} else if err == nil || strings.Contains(string(out), "launched") || !strings.Contains(string(out), "publication did not converge") {
				t.Fatalf("inconsistent config launched: %q, %v", out, err)
			}
		})
	}
}

func publicationDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// Model the captured857-byte first read followed by changed descriptor metadata.
const stalePublicationPreload = `const fs = require("node:fs");
const open = fs.openSync, stat = fs.fstatSync, read = fs.readSync;
let first, opens = 0, bytes = 0, stats = 0;
fs.openSync = (...args) => { const fd = open(...args); if (++opens === 1) first = fd; return fd; };
fs.fstatSync = fd => {
  const info = stat(fd);
  if (fd === first && opens === 1 && stats++ === 0) info.size =857;
  return info;
};
fs.readSync = (fd, buffer, offset, length, position) => {
  if (fd === first && opens === 1) {
    if (bytes ===857) return 0;
    const n = read(fd, buffer, offset, Math.min(length,857 - bytes), position);
    bytes += n; return n;
  }
  return read(fd, buffer, offset, length, position);
};`
