// Package loopcfg reads .agent/loop.yaml — coop's committed per-repo configuration for
// `coop loop`: the preflight / work / between / signoff / verify steps, each with its own model
// ladder and prompt, plus the signoff round cap. It supersedes the retired .agent/loop/*.md files
// and the COOP_LOOP_* / COOP_REVIEW_* env vars.
//
// A missing file — or any missing field — means "use coop's built-in default", so an absent
// loop.yaml is exactly today's behavior. Prompts never OVERRIDE a built-in: signoff.prompt
// APPENDS to its senior review; between.prompt and verify.prompt SET their pass (no built-ins);
// preflight's built-in tidy runs host-side in coop, so preflight.prompt SETS the optional agent
// cleanup on top.
package loopcfg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"gopkg.in/yaml.v3"
)

// File is the repo-relative path of the loop config.
const File = ".agent/loop.yaml"

// Config is a parsed .agent/loop.yaml. Every field is optional; a zero Config means "all
// built-in defaults" (an absent file decodes to this).
// Fields are in loop order: preflight (before) → work → between (after each task) → signoff (end).
type Config struct {
	Preflight Preflight `yaml:"preflight"`
	Work      Work      `yaml:"work"`
	Between   Between   `yaml:"between"`
	Signoff   Signoff   `yaml:"signoff"`
	Verify    Verify    `yaml:"verify"`
}

// Preflight is the one-shot pre-loop queue cleanup. The built-in tidy (unblock tasks whose
// decision.md gained a Resolution) runs host-side in coop; Prompt is the optional agent pass.
type Preflight struct {
	Enabled bool   `yaml:"enabled"` // run the cleanup; false = off
	Prompt  string `yaml:"prompt"`  // SETS an extra agent cleanup (a box runs only for this); "" = host-side tidy only
}

// Work is the task-working iterations.
type Work struct {
	Agent   []string `yaml:"agent"`   // model ladder (target|preset rungs); empty = the CLI agent / preset lead
	Command []string `yaml:"command"` // raw per-iteration command (argv); empty = coop's built-in agent form
}

// Between is the opt-in per-task audit after each completed task.
type Between struct {
	Enabled bool     `yaml:"enabled"` // run the audit; false = off
	Agent   []string `yaml:"agent"`   // audit model ladder; empty = the signoff model
	Prompt  string   `yaml:"prompt"`  // SETS the audit prompt (between has no built-in); required when enabled
}

// Signoff is the end-of-loop pass: the senior review that accepts the batch or reopens tasks.
type Signoff struct {
	Rounds int      `yaml:"rounds"` // work→signoff round cap; 0 = the built-in default (5)
	Agent  []string `yaml:"agent"`  // signoff model ladder; empty = the work model
	Prompt string   `yaml:"prompt"` // APPENDED to the built-in senior review; "" = nothing appended
}

// Verify is an optional FINAL pass, after the signoff accepts the batch: it receives the run's change
// context (per task, by Coop-Task trailer) and does whatever the prompt says — typically e2e/integration
// tests for the affected features. No built-in prompt; it runs only when enabled and a prompt is set.
type Verify struct {
	Enabled bool     `yaml:"enabled"` // run the post-signoff verify pass; false = off
	Agent   []string `yaml:"agent"`   // verify model ladder; empty = the signoff model
	Prompt  string   `yaml:"prompt"`  // SETS the verify prompt (no built-in); required when enabled
}

// Rung is one entry of an `agent:` ladder: EXACTLY one of Target or Preset is set.
type Rung struct {
	Target *agents.Target // a provider[:model][/effort][@account] target
	Preset string         // a preset name (its existence is resolved by the caller)
}

var presetNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// Load reads <repo>/.agent/loop.yaml. A missing file returns an empty Config (all built-in
// defaults) and a nil error. Unknown keys, malformed `agent:` rungs, and an enabled `between`
// with no prompt are errors — surfaced at load so a typo fails before the loop starts, not
// mid-run.
func Load(repo string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(File)))
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)                                             // an unknown key is a typo, not silently ignored
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) { // EOF = an all-comments/empty file
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	for _, s := range []struct {
		name  string
		rungs []string
	}{{"work.agent", c.Work.Agent}, {"signoff.agent", c.Signoff.Agent}, {"between.agent", c.Between.Agent}} {
		if _, err := Rungs(s.rungs); err != nil {
			return nil, fmt.Errorf("%s %s: %w", File, s.name, err)
		}
	}
	if c.Between.Enabled && strings.TrimSpace(c.Between.Prompt) == "" {
		return nil, fmt.Errorf("%s: between.enabled is true but between.prompt is empty — between has no built-in, so set its prompt", File)
	}
	if c.Verify.Enabled && strings.TrimSpace(c.Verify.Prompt) == "" {
		return nil, fmt.Errorf("%s: verify.enabled is true but verify.prompt is empty — verify has no built-in, so set its prompt (e.g. \"e2e-test the affected features\")", File)
	}
	return &c, nil
}

// Rungs classifies each `agent:` entry as a TARGET or a PRESET NAME: a bare identifier that is
// NOT a registered provider is a preset name (its existence is resolved by the caller, which has
// the presets dir); anything with a `:`/`/`/`@` or a known-provider name is parsed as a target.
// Nil entries → nil, nil.
func Rungs(entries []string) ([]Rung, error) {
	var out []Rung
	for _, e := range entries {
		s := strings.TrimSpace(e)
		if s == "" {
			return nil, fmt.Errorf("empty ladder entry")
		}
		// A bare word that isn't a provider is a preset name (frontier). A known provider (claude)
		// or anything carrying :/@/ is a target.
		if !strings.ContainsAny(s, ":/@") && !agents.Valid(s) {
			if !presetNameRe.MatchString(s) {
				return nil, fmt.Errorf("%q is neither a provider[:model][/effort][@account] target nor a valid preset name", s)
			}
			out = append(out, Rung{Preset: s})
			continue
		}
		t, err := agents.ParseTarget(s)
		if err != nil {
			return nil, err
		}
		tt := t
		out = append(out, Rung{Target: &tt})
	}
	return out, nil
}
