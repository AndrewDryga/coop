package box

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/runtime"
)

const networkRulesFixture = "egress_rules:\n  - to: {domain: EXAMPLE.com}\n    protocol: tls\n    ports: [443]\n"

func TestNetworkRulesFileCannotLaunderRulesThroughOrdinaryMode(t *testing.T) {
	for _, mode := range []egress.Mode{egress.Open, egress.None} {
		t.Run(string(mode), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			path := filepath.Join(t.TempDir(), "rules.yaml")
			if err := os.WriteFile(path, []byte(networkRulesFixture), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{ConfigDir: t.TempDir()}
			capture, err := AdmitNetwork(cfg, runtime.Runtime{Name: "must-not-execute"}, RunSpec{Repo: t.TempDir()},
				NetworkAdmission{InvocationMode: &mode, RulesFile: path})
			if capture != nil || err == nil || !strings.Contains(err.Error(), "network_policy_conflict") {
				t.Fatal("ordinary mode accepted rules or reached the runtime", err)
			}
			state, err := NetworkStatePath()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("conflicting rules published state", err)
			}
		})
	}
}

func TestCaptureNetworkRulesFileClassifiesTheCapturedSource(t *testing.T) {
	for _, kind := range []string{"private", "repository", "traversed-alias", "hard-link", "shared-writable"} {
		t.Run(kind, func(t *testing.T) {
			repo, private := t.TempDir(), t.TempDir()
			path := filepath.Join(private, "rules.yaml")
			if kind == "repository" {
				path = filepath.Join(repo, "rules.yaml")
			}
			if err := os.WriteFile(path, []byte(networkRulesFixture), 0o600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "traversed-alias":
				bridge := filepath.Join(repo, "bridge")
				if err := os.Symlink(private, bridge); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(bridge, "rules.yaml")
			case "hard-link":
				if err := os.Link(path, filepath.Join(repo, "linked.yaml")); err != nil {
					t.Fatal(err)
				}
			case "shared-writable":
				if err := os.Chmod(path, 0o666); err != nil {
					t.Fatal(err)
				}
			}
			rules, operator, err := CaptureNetworkRulesFile(path, []string{repo})
			if err != nil || operator != (kind == "private") || len(rules) != 1 || rules[0].To.Domain != "example.com" {
				t.Fatalf("capture = %#v operator %v error %v", rules, operator, err)
			}
			// A later review or launch consumes the captured data, not the path.
			if err := os.WriteFile(path, []byte("egress_rules: []\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if len(rules) != 1 || rules[0].To.Domain != "example.com" {
				t.Fatal("capture changed with its source")
			}
		})
	}
}

func TestCaptureNetworkRulesFileRefusesInvalidSources(t *testing.T) {
	for _, kind := range []string{"absent", "directory", "fifo", "symlink", "oversize", "malformed", "parent-traversal"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "rules.yaml")
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "fifo":
				err = syscall.Mkfifo(path, 0o600)
			case "symlink":
				err = os.WriteFile(filepath.Join(root, "target"), []byte(networkRulesFixture), 0o600)
				if err == nil {
					err = os.Symlink("target", path)
				}
			case "oversize":
				err = os.WriteFile(path, []byte(strings.Repeat(" ", egress.MaxDocumentBytes+1)), 0o600)
			case "malformed":
				err = os.WriteFile(path, []byte("egress_rules: [{to: {domain: example.com}, trusted: true}]"), 0o600)
			case "parent-traversal":
				path = root + "/child/../rules.yaml"
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, operator, err := CaptureNetworkRulesFile(path, nil); err == nil || operator {
				t.Fatalf("invalid source granted authority: %v %v", operator, err)
			}
		})
	}
}

func TestNetworkRulesFileRepositoryRequestRefusesBeforeRuntime(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	path := filepath.Join(repo, "network.yaml")
	if err := os.WriteFile(path, []byte(networkRulesFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir()}
	capture, err := AdmitNetwork(cfg, runtime.Runtime{Name: "must-not-execute"}, RunSpec{Repo: repo}, NetworkAdmission{RulesFile: path})
	if err == nil {
		_ = capture.Close()
		t.Fatal("repository rules bypassed approval")
	}
	if !strings.Contains(err.Error(), "network_approval_required") {
		t.Fatal("wrong refusal", err)
	}
	root, err := NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unapproved rules created authority state", err)
	}
}

// The authority root itself must be outside every agent mount, and the check
// happens before any state, key or runtime inspection exists.
func TestNetworkAuthorityRootInsideAnAgentMountRefusesBeforeState(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("XDG_STATE_HOME", repo)
	mode := egress.Filtered
	cfg := &config.Config{ConfigDir: t.TempDir()}
	capture, err := AdmitNetwork(cfg, runtime.Runtime{Name: "must-not-execute"}, RunSpec{Repo: repo}, NetworkAdmission{InvocationMode: &mode})
	if err == nil {
		_ = capture.Close()
		t.Fatal("authority inside an agent mount was accepted")
	}
	if !strings.Contains(err.Error(), "outside every agent mount") {
		t.Fatal("wrong refusal", err)
	}
	root, err := NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("exposed authority root was created", err)
	}
}
