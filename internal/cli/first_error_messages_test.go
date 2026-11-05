package cli

import (
	"github.com/AndrewDryga/coop/internal/box"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// A person who typed the wrong thing hears about what they typed, not about the next check along.
func TestEarliestMistakeIsTheOneNamed(t *testing.T) {
	if _, _, _, err := parseEvalRunArgs(nil); err == nil || !strings.Contains(err.Error(), "Missing suite") {
		t.Errorf("eval run with nothing = %v, want the missing suite named before the timeout", err)
	}
	a := &app{cfg: &config.Config{}}
	if code, err := a.cmdACP([]string{"--nosuch"}); code != 2 || err == nil || !strings.Contains(err.Error(), `Unknown option "--nosuch"`) {
		t.Errorf("acp --nosuch = (%d, %v), want an unknown option, not an unknown preset", code, err)
	}
}

// coop login refuses a name like ../x, so the refusal must not offer that sign-in.
func TestNoAccountOffersOnlyASignInLoginAccepts(t *testing.T) {
	if got := noAccountErr("claude", "work").Error(); !strings.Contains(got, "coop login claude@work") {
		t.Errorf("valid name lost its sign-in: %s", got)
	}
	if got := noAccountErr("claude", "../x").Error(); strings.Contains(got, "coop login") {
		t.Errorf("offered a sign-in login refuses: %s", got)
	}
}

// An offline box stops a one-prompt agent run before it starts; an interactive one is allowed.
func TestRefuseOfflineOnePrompt(t *testing.T) {
	offline := &config.Config{Egress: "none"}
	if err := refuseOfflineOnePrompt(offline, box.RunSpec{Agent: "codex", AgentCommand: true, Cmd: []string{"codex", "exec", "hi"}}); err == nil || !strings.Contains(err.Error(), "--egress filtered") {
		t.Errorf("offline exec = %v", err)
	}
	if err := refuseOfflineOnePrompt(offline, box.RunSpec{Agent: "codex", AgentCommand: true, Cmd: []string{"codex"}}); err != nil {
		t.Errorf("offline interactive = %v", err)
	}
	if err := refuseOfflineOnePrompt(&config.Config{Egress: "filtered"}, box.RunSpec{Agent: "claude", AgentCommand: true, Cmd: []string{"claude", "-p", "x"}}); err != nil {
		t.Errorf("filtered one prompt = %v", err)
	}
}
