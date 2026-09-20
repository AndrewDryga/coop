package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestParseEvalRunArgs(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantSuite   string
		wantConfigs []string
		wantJobs    int
		wantRepeat  int
		wantTimeout time.Duration
		wantLoop    string
		wantErr     bool
	}{
		{"suite and two configs", []string{"./s.yaml", "codex", "frontier", "--timeout", "60m"}, "./s.yaml", []string{"codex", "frontier"}, 1, 1, time.Hour, "", false},
		{"flags interleaved", []string{"./s.yaml", "--jobs", "4", "codex", "--repeat", "3", "--timeout", "30m"}, "./s.yaml", []string{"codex"}, 4, 3, 30 * time.Minute, "", false},
		{"loop config override", []string{"./s.yaml", "frontier", "--timeout", "60m", "--loop-config", ".agent/loop.yaml"}, "./s.yaml", []string{"frontier"}, 1, 1, time.Hour, ".agent/loop.yaml", false},
		// A run needs an explicit --timeout: it covers preparation, work, grading and cleanup.
		{"no timeout", []string{"./s.yaml", "codex"}, "", nil, 0, 0, 0, "", true},
		{"missing flag value", []string{"./s.yaml", "codex", "--jobs"}, "", nil, 0, 0, 0, "", true},
		{"repeated flag", []string{"./s.yaml", "codex", "--jobs", "2", "--jobs", "3", "--timeout", "10m"}, "", nil, 0, 0, 0, "", true},
		{"zero jobs", []string{"./s.yaml", "codex", "--jobs", "0", "--timeout", "10m"}, "", nil, 0, 0, 0, "", true},
		{"non-number repeat", []string{"./s.yaml", "codex", "--repeat", "lots", "--timeout", "10m"}, "", nil, 0, 0, 0, "", true},
		{"bad timeout", []string{"./s.yaml", "codex", "--timeout", "soon"}, "", nil, 0, 0, 0, "", true},
		{"unknown flag", []string{"./s.yaml", "codex", "--turbo", "--timeout", "10m"}, "", nil, 0, 0, 0, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			suite, configs, opts, err := parseEvalRunArgs(c.args)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if suite != c.wantSuite || strings.Join(configs, ",") != strings.Join(c.wantConfigs, ",") ||
				opts.Jobs != c.wantJobs || opts.Repeat != c.wantRepeat || opts.Timeout != c.wantTimeout ||
				opts.LoopConfigOverride != c.wantLoop {
				t.Errorf("parse(%v) = suite %q configs %v jobs %d repeat %d timeout %s loop %q",
					c.args, suite, configs, opts.Jobs, opts.Repeat, opts.Timeout, opts.LoopConfigOverride)
			}
		})
	}
}

// A bad target or a missing preset is refused during resolution, before any plan or provider work.
func TestResolveEvalConfigurationsRefusesBadPositionals(t *testing.T) {
	a := &app{cfg: &config.Config{RepoOverride: t.TempDir()}}
	if _, err := a.resolveEvalConfigurations([]string{"codex:bad:model:extra"}); err == nil {
		t.Error("a malformed target was accepted")
	}
	if _, err := a.resolveEvalConfigurations([]string{"no-such-preset-xyz"}); err == nil {
		t.Error("a missing preset was accepted")
	}
	// A well-formed bare target resolves to a target configuration.
	configs, err := a.resolveEvalConfigurations([]string{"codex"})
	if err != nil {
		t.Fatalf("codex should resolve: %v", err)
	}
	if len(configs) != 1 || configs[0].Kind != "target" || configs[0].Label != "codex" {
		t.Errorf("codex resolved to %+v", configs)
	}
}
