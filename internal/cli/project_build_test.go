package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestProjectBuildPromptOnlyBelongsToHostInteractiveLaunches(t *testing.T) {
	t.Setenv("COOP_BOX", "")
	t.Setenv(box.ControllerJobEnv, "")
	base := box.RunSpec{Agent: "codex", AgentCommand: true, Cmd: []string{"codex"}}
	if !projectBuildPromptEligible(base, true, true) {
		t.Fatal("ordinary host interactive launch cannot confirm a build")
	}
	for name, change := range map[string]func(*box.RunSpec){
		"loop": func(s *box.RunSpec) { s.Batch = true }, "ACP": func(s *box.RunSpec) { s.ForceNoTTY = true },
		"probe": func(s *box.RunSpec) { s.Quiet = true }, "controller": func(s *box.RunSpec) { s.ControllerJob = true },
		"login": func(s *box.RunSpec) { s.Login = true }, "raw": func(s *box.RunSpec) { s.AgentCommand = false },
		"supervised": func(s *box.RunSpec) { s.SupervisorID = "owner" },
	} {
		t.Run(name, func(t *testing.T) {
			spec := base
			change(&spec)
			if projectBuildPromptEligible(spec, true, true) {
				t.Fatal("an unattended/ineligible caller acquired host build confirmation")
			}
		})
	}
	if projectBuildPromptEligible(base, false, true) || projectBuildPromptEligible(base, true, false) {
		t.Fatal("headless or piped launch acquired confirmation")
	}
	for _, key := range []string{"COOP_BOX", box.ControllerJobEnv} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "present")
			if projectBuildPromptEligible(base, true, true) {
				t.Fatal("box/controller subprocess acquired host build confirmation")
			}
		})
	}
}

func TestProjectBuildPromptRejectsNativeHeadlessCommands(t *testing.T) {
	t.Setenv("COOP_BOX", "")
	t.Setenv(box.ControllerJobEnv, "")
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		cfg := &config.Config{}
		spec := box.RunSpec{Agent: name, AgentCommand: true, Cmd: ag.Interactive(cfg)}
		if !projectBuildPromptEligible(spec, true, true) {
			t.Errorf("%s native TUI cannot confirm", name)
		}
		spec.Cmd = ag.Headless(cfg, "fixture")
		// Session history production deliberately differs from actual interactivity.
		if projectBuildPromptEligible(spec, true, true) {
			t.Errorf("%s native print mode acquired terminal confirmation", name)
		}
		spec.Cmd = ag.ACP(cfg)
		spec.ForceNoTTY = true
		if projectBuildPromptEligible(spec, true, true) {
			t.Errorf("%s protocol invocation acquired confirmation", name)
		}
	}
}

func TestProjectBuildPromptRejectsNativeMaintenanceAndJoinedPrintFlags(t *testing.T) {
	t.Setenv("COOP_BOX", "")
	t.Setenv(box.ControllerJobEnv, "")
	for _, cmd := range [][]string{
		{"claude", "auth", "login"}, {"claude", "mcp", "list"}, {"claude", "-pfixture"},
		{"grok", "login", "--device-auth"}, {"grok", "inspect", "--json"}, {"grok", "-pfixture"},
		{"gemini", "skills", "list"}, {"gemini", "-pfixture"}, {"gemini", "-yp", "fixture"},
		{"codex", "app-server"}, {"codex", "login", "--device-auth"},
		{"claude", "--model", "--print", "fixture"}, {"gemini", "--model", "-p", "fixture"},
		{"grok", "--model", "--single", "fixture"}, {"codex", "--model", "--help"},
	} {
		spec := box.RunSpec{Agent: cmd[0], AgentCommand: true, Cmd: cmd}
		if projectBuildPromptEligible(spec, true, true) {
			t.Errorf("native non-TUI acquired build decision: %q", cmd)
		}
	}
	claude, _ := agents.Get("claude")
	if !claude.InteractiveInvocation([]string{"claude", "--append-system-prompt", "doctor"}) {
		t.Fatal("a prompt value was mistaken for a maintenance command")
	}
}

func TestProjectBuildConfirmationExplainsNetworkingAndDefaultsToRefusal(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		called := false
		var err error
		out := captureTerminal(t, func() {
			err = confirmProjectBuild(t.Context(), ".agent/Dockerfile", "project inputs changed", func() bool { called = true; return accepted })
		})
		if !called || (err == nil) != accepted {
			t.Fatalf("confirmation = %v / called=%v / accepted=%v", err, called, accepted)
		}
		for _, text := range []string{"Preparing the project box", "Project inputs changed", ".agent/Dockerfile", "ordinary networking", "agent will still use filtered access"} {
			if !strings.Contains(out, text) {
				t.Errorf("confirmation omitted %q:\n%s", text, out)
			}
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	out := captureTerminal(t, func() {
		err := confirmProjectBuild(ctx, ".agent/Dockerfile", "inputs changed", func() bool { cancel(); return true })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled confirmation approved build: %v", err)
		}
	})
	if strings.Contains(out, "Project box built") {
		t.Fatal("cancelled decision reported a completed build")
	}
}
