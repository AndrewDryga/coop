package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/loopcfg"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
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
	if len(c.Work.Agent) == 0 || len(c.Signoff.Agent) == 0 {
		t.Error("work and signoff should carry model ladders")
	}
	// Every preset rung in every ladder must name a preset that actually exists in-repo.
	for name, ladder := range map[string][]string{
		"work.agent": c.Work.Agent, "between.agent": c.Between.Agent, "signoff.agent": c.Signoff.Agent,
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

// This repository's gate runs in its own box, and Coop's box images carry no Go analyzers, so
// .agent/Dockerfile installs them at the Makefile's pins: Staticcheck from its module file, which
// pins the importer too, and govulncheck at its version. A bump on one side alone would otherwise
// surface only when the gate next ran in a box.
func TestOwnDockerfileInstallsTheGatePins(t *testing.T) {
	data, err := os.ReadFile(".agent/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	modfile := makeVariable(t, "STATICCHECK_MODFILE")
	for _, want := range []string{
		"COPY --chown=node:node " + modfile + " /tmp/staticcheck/go.mod\n",
		"COPY --chown=node:node " + strings.TrimSuffix(modfile, ".mod") + ".sum /tmp/staticcheck/go.sum\n",
		"go -C /tmp/staticcheck install -mod=readonly honnef.co/go/tools/cmd/staticcheck",
		"go install golang.org/x/vuln/cmd/govulncheck@" + makeVariable(t, "GOVULNCHECK_VERSION") + "\n",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf(".agent/Dockerfile must install the gate's pinned analyzers: missing %q", want)
		}
	}
}

// The toolchain image's build context is its ignore file's allowlist, so a COPY input the list
// leaves out fails only when someone builds the image.
func TestOwnDockerfileContextKeepsEveryCopyInput(t *testing.T) {
	dockerfile, err := os.ReadFile(".agent/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	ignore, err := os.ReadFile(".agent/Dockerfile.dockerignore")
	if err != nil {
		t.Fatal(err)
	}
	patterns, err := ignorefile.ReadAll(bytes.NewReader(ignore))
	if err != nil {
		t.Fatal(err)
	}
	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		t.Fatal(err)
	}
	var sources []string
	for line := range strings.Lines(string(dockerfile)) {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "COPY" && fields[0] != "ADD" {
			continue
		}
		var paths []string
		for _, field := range fields[1:] {
			if strings.HasPrefix(field, "--from=") {
				paths = nil
				break
			}
			if !strings.HasPrefix(field, "--") {
				paths = append(paths, field)
			}
		}
		if len(paths) > 1 {
			sources = append(sources, paths[:len(paths)-1]...)
		}
	}
	if len(sources) == 0 {
		t.Fatal(".agent/Dockerfile copies nothing from its context; this test reads COPY lines")
	}
	for _, source := range sources {
		if excluded, err := matcher.MatchesOrParentMatches(source); err != nil || excluded {
			t.Errorf(".agent/Dockerfile.dockerignore must keep the COPY input %s (excluded: %t, %v)", source, excluded, err)
		}
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
		if r.Name == "thinker" && r.Primary().Effort != "xhigh" {
			t.Errorf("thinker effort = %q, want xhigh (a consult is one shot — spend the effort)", r.Primary().Effort)
		}
	}
}
