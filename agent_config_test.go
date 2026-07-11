package main

import (
	"testing"

	"github.com/AndrewDryga/coop/internal/loopcfg"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/project"
)

// Coop's own committed .agent/ configs (loop.yaml, project.yaml, the frontier preset) are
// dogfood: coop runs its own loop against them. Pin that they always LOAD — a typo'd key, a
// malformed target, or a preset rung naming a preset that doesn't exist would otherwise only
// surface when the next unattended loop starts. Tests run from the repo root, so "." is the repo.

func TestOwnLoopYAMLLoads(t *testing.T) {
	c, err := loopcfg.Load(".")
	if err != nil {
		t.Fatalf(".agent/loop.yaml must load: %v", err)
	}
	// The steps this repo turned on stay on — losing one in an edit should be a conscious act.
	if !c.Preflight.Enabled {
		t.Error("preflight.enabled should be true (the queue tidy is part of this repo's loop)")
	}
	if !c.Between.Enabled || c.Between.Prompt == "" {
		t.Error("between should be enabled with its audit prompt set")
	}
	if len(c.Work.Agent) == 0 || len(c.Review.Agent) == 0 {
		t.Error("work and review should carry model ladders")
	}
	// Every preset rung in every ladder must name a preset that actually exists in-repo.
	for name, ladder := range map[string][]string{
		"work.agent": c.Work.Agent, "between.agent": c.Between.Agent, "review.agent": c.Review.Agent,
	} {
		rungs, err := loopcfg.Rungs(ladder)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, r := range rungs {
			if r.Preset == "" {
				continue
			}
			if _, err := preset.Load(".", "", r.Preset); err != nil {
				t.Errorf("%s names preset %q which does not load: %v", name, r.Preset, err)
			}
		}
	}
}

func TestOwnProjectYAMLLoads(t *testing.T) {
	p, err := project.Load(".")
	if err != nil {
		t.Fatalf(".agent/project.yaml must load: %v", err)
	}
	if p.Gate == "" {
		t.Error("gate: should be set — fork merges in this repo revalidate with make check")
	}
}

func TestOwnFrontierPresetLoads(t *testing.T) {
	p, err := preset.Load(".", "", "frontier")
	if err != nil {
		t.Fatalf("the frontier preset must load: %v", err)
	}
	// The three documented roles stay wired (AGENTS.md's orchestration section names them).
	have := make(map[string]bool, len(p.Roles))
	for _, r := range p.Roles {
		have[r.Name] = true
	}
	for _, role := range []string{"thinker", "critic", "fast"} {
		if !have[role] {
			t.Errorf("frontier preset lost its %q role — AGENTS.md documents it", role)
		}
	}
	for _, r := range p.Roles {
		if r.Name == "critic" && r.Effort != "xhigh" {
			t.Errorf("critic effort = %q, want xhigh (a consult is one shot — spend the effort)", r.Effort)
		}
	}
}
