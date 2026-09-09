package networkgateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

const pinnedEnvoyImage = "envoyproxy/envoy:v1.39.1@sha256:57e14a549d7bd43c8d3f6d03e8cfa653e037d4b38e133acd9b54f38c524401b4"

func TestEnvoyLogFormatProducesRealNewlineFraming(t *testing.T) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(EnvoyBootstrap), &root); err != nil {
		t.Fatal(err)
	}
	found := 0
	var visit func(*yaml.Node)
	visit = func(node *yaml.Node) {
		if node.Kind == yaml.MappingNode {
			for i := 0; i < len(node.Content); i += 2 {
				if node.Content[i].Value == "inline_string" {
					found++
					value := node.Content[i+1].Value
					if !strings.HasSuffix(value, "\n") || strings.Count(value, "\n") != 1 || strings.Contains(value, `\n`) {
						t.Error("Envoy events lack actual newline framing after YAML decoding")
					}
				}
			}
		}
		for _, child := range node.Content {
			visit(child)
		}
	}
	visit(&root)
	if found != 1 {
		t.Fatalf("wanted exactly one bounded log format, got %d", found)
	}
}

func TestPinnedEnvoyValidatesBootstrap(t *testing.T) {
	if os.Getenv("COOP_EGRESS_LIVE_TESTS") != "1" {
		t.Skip("opt-in pinned Docker gateway qualification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
	defer cancel()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(random[:])
	name := "coop-network-check-" + id
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
		defer cancel()
		label, err := exec.CommandContext(ctx, "docker", "container", "inspect", "--format", `{{index .Config.Labels "coop.network.fixture"}}`, name).CombinedOutput()
		if err != nil {
			if !strings.Contains(string(label), "No such container") && !strings.Contains(string(label), "No such object") {
				t.Errorf("fixture cleanup could not verify ownership: %v %s", err, label)
			}
			return
		}
		if strings.TrimSpace(string(label)) != id {
			t.Error("fixture cleanup refused mismatched ownership")
			return
		}
		if output, err := exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput(); err != nil {
			t.Errorf("exact-owned fixture cleanup: %v %s", err, output)
		}
	})
	// No credentials, repository, networking, privileges or host filesystem are
	// exposed. This validates fields in the actual pinned binary, not a mock.
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--name", name, "--label", "coop.network.fixture="+id, "--network", "none", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--read-only", "--user", "65532:65532", "--memory", "256m", "--pids-limit", "32",
		"--tmpfs", "/private:rw,nosuid,nodev,noexec,uid=65532,gid=65532,mode=0700,size=1m",
		"--entrypoint", "envoy", pinnedEnvoyImage, "--mode", "validate", "--disable-hot-restart", "--concurrency", "1",
		"--file-flush-interval-msec", "100", "--file-flush-min-size-kb", "1", "--config-yaml", EnvoyBootstrap)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pinned Envoy bootstrap validation: %v\n%s", err, output)
	}
	t.Logf("pinned Envoy bootstrap validated: %s", pinnedEnvoyImage)
}
