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
