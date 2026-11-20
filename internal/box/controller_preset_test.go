package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestControllerPresetBindsTheActualJobLeadAndMount(t *testing.T) {
	root := t.TempDir()
	identity := strings.Repeat("a", 64)
	job := workerproto.JobSpec{RepositoryReadOnly: true, Targets: []string{"codex@work"}, Preset: &workerproto.JobPreset{
		Version: 1, Name: "review", Roles: []workerproto.JobPresetRole{{Name: "reader", Mode: "consult", Targets: []string{"gemini@team"}, When: []string{}, Prompt: "Review the change."}},
	}}
	data, digest, err := EncodeControllerPreset(identity, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ControllerPresetFile), data, 0600); err != nil {
		t.Fatal(err)
	}
	lead, _ := agent.ParseTarget("codex@work")
	t.Setenv(ControllerJobEnv, identity)
	t.Setenv(ControllerPresetDigestEnv, digest)
	p, err := ControllerPresetForChild(root, lead, true)
	if err != nil || p == nil || len(p.Roles) != 1 || p.Roles[0].PromptText != "Review the change." {
		t.Fatalf("approved consult recipe: %+v %v", p, err)
	}
	if _, err := ControllerPresetForChild(root, lead, false); err == nil {
		t.Fatal("accepted a different mount")
	}
	other, _ := agent.ParseTarget("codex@personal")
	if _, err := ControllerPresetForChild(root, other, true); err == nil {
		t.Fatal("accepted an unapproved account")
	}
	t.Setenv(ControllerJobEnv, strings.Repeat("b", 64))
	if _, err := ControllerPresetForChild(root, lead, true); err == nil {
		t.Fatal("reused a different job's recipe")
	}
	t.Setenv(ControllerJobEnv, identity)
	if err := os.WriteFile(filepath.Join(root, ControllerPresetFile), append(data, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ControllerPresetForChild(root, lead, true); err == nil {
		t.Fatal("accepted a changed file")
	}
	t.Setenv(ControllerPresetDigestEnv, "")
	if p, err := ControllerPresetForChild(root, lead, true); err != nil || p != nil {
		t.Fatal("a job without a preset consulted the file")
	}
}
