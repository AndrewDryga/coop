package loop

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/runtime"
)

// Every box a loop starts carries its preset's roles, under whichever provider leads that box. A
// work ladder that rotates onto a provider which cannot host one of the preset's native roles is
// refused before the first box — never run with the role quietly turned into a read-only consult.
func TestLoopRefusesALeadThatCannotHostANativeRole(t *testing.T) {
	repo := t.TempDir()
	writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "2026-01-01-x", "task.md"), "# x\n")
	c := New(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}, runtime.Runtime{Name: "true"}, "test", Host{})
	c.boxRun = func(box.RunSpec) (int, error) {
		t.Error("a box launched with a native role its lead cannot host")
		return 0, nil
	}
	p := &preset.Preset{Name: "t", Dir: t.TempDir(), Roles: []preset.Role{
		{Name: "thinker", Mode: preset.ModeNative, Targets: []agents.Target{{Provider: "claude"}}},
	}}
	code, err := c.Run(RunSpec{Repo: repo, Image: "img", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard, Preset: p,
		Rotation: ladder.NewRotation([]agents.Target{target("claude", "work"), target("codex", "work")})})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "work agent: preset t: thinker is a native claude subagent, but codex can lead this preset too") {
		t.Fatalf("a work ladder a native role cannot follow = (%d, %v)", code, err)
	}
}

// Review boxes carry the preset's roles too, under the review rung's provider: a signoff on another
// provider is refused before any box. A stage that never starts a box is not asked — a verify pass
// switched off may name any agent, and the run goes on to network admission (which this fixture's
// runtime then refuses, so the test needs no box).
func TestLoopChecksTheReviewStagesThatRun(t *testing.T) {
	p := &preset.Preset{Name: "t", Dir: t.TempDir(), Roles: []preset.Role{
		{Name: "thinker", Mode: preset.ModeNative, Targets: []agents.Target{{Provider: "claude"}}},
	}}
	for _, test := range []struct {
		name, loopYAML, want string
	}{
		{"signoff on another provider", "signoff:\n  agent: [codex:gpt-5.6-sol]\n", "signoff agent: preset t: thinker is a native claude subagent, but codex can lead"},
		{"a verify pass switched off", "verify:\n  enabled: false\n  agent: [codex:gpt-5.6-sol]\n", "restricted networking needs docker"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			repo := t.TempDir()
			writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "2026-01-01-x", "task.md"), "# x\n")
			writeTaskFile(t, filepath.Join(repo, ".agent", "loop.yaml"), test.loopYAML)
			writeTaskFile(t, filepath.Join(repo, ".agent", "project.yaml"), "box:\n  egress: filtered\n")
			cfg := &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}
			writeTaskFile(t, filepath.Join(cfg.AgentProfileDir("claude", "default"), ".credentials.json"),
				`{"claudeAiOauth":{"accessToken":"access","expiresAt":4102444800000,"scopes":["user:inference"]}}`)
			host := Host{BuildRotation: func(_ string, rungs []agents.Target) (*ladder.Rotation, error) { return ladder.NewRotation(rungs), nil }}
			c := New(cfg, runtime.Runtime{Name: "true"}, "test", host)
			c.boxRun = func(box.RunSpec) (int, error) {
				t.Error("a box launched")
				return 0, nil
			}
			_, err := c.Run(RunSpec{Repo: repo, Image: "img", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard, Preset: p,
				Rotation: ladder.NewRotation([]agents.Target{target("claude", "work")})})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
		})
	}
}
