package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/acpctl"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestACPChildrenKeepAdmittedNetworkMode(t *testing.T) {
	for _, tc := range []struct {
		name, mode, ambient, expected string
		inheritProject, probeOnly     bool
	}{
		{"offline-with-no-ambient-setting", "none", "", "none", false, false},
		{"offline-overrides-ambient-open", "none", "open", "none", false, false},
		{"explicit-open-overrides-ambient-offline", "open", "none", "open", false, false},
		{"standalone-probe-inherits-project-policy", "open", "", "UNSET", true, true},
		{"standalone-probe-keeps-explicit-offline", "none", "none", "none", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COOP_EGRESS", tc.ambient)
			if tc.ambient == "" {
				if err := os.Unsetenv("COOP_EGRESS"); err != nil {
					t.Fatal(err)
				}
			}
			for _, shape := range []struct {
				name             string
				role             forkspace.ExecutionRole
				hasTarget, probe bool
			}{
				{"initial", forkspace.ExecutionRoleActive, false, false},
				{"replacement", forkspace.ExecutionRoleActive, true, false},
				{"warm", forkspace.ExecutionRoleWarm, true, false},
				{"model-probe", forkspace.ExecutionRoleProbe, true, true},
			} {
				if tc.probeOnly != shape.probe {
					continue // catalog probes are standalone; editor children have supervisor admission
				}
				t.Run(shape.name, func(t *testing.T) {
					root := t.TempDir()
					recorded, shim := filepath.Join(root, "recorded"), filepath.Join(root, "inner")
					body := "#!/bin/sh\nprintf 'mode:%s\\n' \"${COOP_EGRESS-UNSET}\" > " + strconv.Quote(recorded) + "\n"
					if err := os.WriteFile(shim, []byte(body), 0o700); err != nil {
						t.Fatal(err)
					}
					cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: root, Egress: tc.mode}
					if !tc.inheritProject {
						cfg.SetEgress(tc.mode)
					}
					ctrl := acpctl.New(cfg, "claude", "", "", root, acpctl.Selection{}, nil, nil, acpHost())
					if shape.probe {
						ctrl = nil
					}
					a := &app{cfg: cfg}
					child, err := a.spawnBox(context.Background(), shim, nil, "network-test", ctrl,
						agents.Target{Provider: "claude"}, "", shape.hasTarget, io.Discard, shape.role)
					if err != nil {
						t.Fatal(err)
					}
					defer child.Stop()
					var got string
					wait.For(t, "the actual child network mode", func() bool {
						data, err := os.ReadFile(recorded)
						got = string(data)
						return err == nil && strings.HasSuffix(got, "\n")
					})
					if want := "mode:" + tc.expected + "\n"; got != want {
						t.Fatalf("child received %q, want %q", got, want)
					}
				})
			}
		})
	}
}

func TestACPChildrenRequestTheirOwnMCPHandoff(t *testing.T) {
	for _, shape := range []struct {
		name string
		role forkspace.ExecutionRole
		fork bool
	}{
		{"initial", forkspace.ExecutionRoleActive, false},
		{"replacement", forkspace.ExecutionRoleActive, false},
		{"warm", forkspace.ExecutionRoleWarm, false},
		{"fork", forkspace.ExecutionRoleActive, true},
		{"catalog", forkspace.ExecutionRoleProbe, false},
	} {
		t.Run(shape.name, func(t *testing.T) {
			t.Setenv("COOP_ACP_MCP_ID", "foreign-run")
			t.Setenv("COOP_SESSION_MCP_HANDOFF", "/foreign-handoff")
			root := t.TempDir()
			t.Setenv("TMPDIR", root) // an earlier box may retain a broad bind of this directory
			recorded, shim := filepath.Join(root, "recorded"), filepath.Join(root, "inner")
			body := "#!/bin/sh\numask 077\n" +
				"if [ -n \"${COOP_SESSION_MCP_HANDOFF-}\" ]; then\n" +
				"  printf '{\"run_id\":\"%s\",\"mcpServers\":[{\"name\":\"shared\",\"command\":\"node\"}]}\\n' \"${COOP_ACP_MCP_ID-}\" > \"$COOP_SESSION_MCP_HANDOFF\"\nfi\n" +
				"printf '%s\\n%s\\n' \"${COOP_ACP_MCP_ID-}\" \"${COOP_SESSION_MCP_HANDOFF-}\" > " + strconv.Quote(recorded) + "\n"
			if err := os.WriteFile(shim, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: root}
			ctrl := acpctl.New(cfg, "claude", "", "", root, acpctl.Selection{}, nil, nil, acpHost())
			if shape.fork || shape.role == forkspace.ExecutionRoleProbe {
				ctrl = nil
			}
			a := &app{cfg: cfg}
			child, err := a.spawnBox(t.Context(), shim, nil, "mcp-test", ctrl,
				agents.Target{Provider: "claude"}, "", shape.name == "replacement", io.Discard, shape.role)
			if err != nil {
				t.Fatal(err)
			}
			defer child.Stop()
			var values []string
			wait.For(t, "the actual child MCP handoff", func() bool {
				data, err := os.ReadFile(recorded)
				values = strings.Split(string(data), "\n")
				return err == nil && len(values) == 3
			})
			if shape.role == forkspace.ExecutionRoleProbe {
				if values[0] != "" || values[1] != "" || child.MCPServers != nil {
					t.Fatalf("catalog inherited editor MCP authority: %q", values)
				}
				return
			}
			if !strings.HasPrefix(values[0], "acp-") || len(values[0]) != len("acp-")+16 {
				t.Fatalf("child run identity = %q, want its own generation", values[0])
			}
			if !filepath.IsAbs(values[1]) || values[1] == "/foreign-handoff" {
				t.Fatalf("child handoff = %q, want private absolute path", values[1])
			}
			protected, err := filepath.EvalSymlinks(filepath.Join(cfg.ConfigDir, "runfiles"))
			if err != nil {
				t.Fatal(err)
			}
			if relative, err := filepath.Rel(protected, values[1]); err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				t.Fatalf("handoff %q is outside protected runfiles %q", values[1], protected)
			}
			if info, err := os.Stat(filepath.Dir(values[1])); err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("handoff parent is not private: %v, %v", info, err)
			}
			if child.MCPServers == nil {
				t.Fatal("editor child has no shared tool projection")
			}
			servers, err := child.MCPServers()
			if err != nil || len(servers) != 1 || servers[0]["name"] != "shared" {
				t.Fatalf("read child-produced tools = %v, %v", servers, err)
			}
			child.Stop()
			if _, err := os.Stat(filepath.Dir(values[1])); !os.IsNotExist(err) {
				t.Fatalf("stopped child retained its handoff: %v", err)
			}
		})
	}
}

func TestACPMCPIdentityDoesNotSelectRemoteSessionCustody(t *testing.T) {
	t.Setenv("COOP_SESSION_RUN_ID", "session-0123456789abcdef01234567")
	for _, id := range []string{"", "foreign", "acp-not-hex-abcdef", "acp-0123456789abcdef"} {
		t.Setenv("COOP_ACP_MCP_ID", id)
		want := ""
		if id == "acp-0123456789abcdef" {
			want = id
		}
		if got := acpMCPIDFromEnv(); got != want {
			t.Fatalf("MCP identity(%q) = %q, want %q", id, got, want)
		}
	}
}
