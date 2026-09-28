// Package eval is the native evals library: it loads and validates a suite manifest, freezes the
// exact bytes of every input a comparison must be able to tell apart, and plans a run — all without
// launching a provider. The CLI (internal/cli/eval_cmd.go) owns command parsing and rendering and
// hands this package resolved configurations; this package never imports cli, sessions or the UI.
//
// v1 answers one question: "I changed a preset, a loop config or Coop itself — did it help?" So the
// records here are built around before/after comparison: a suite (the workload), its cases, the
// resolved configurations under test, and — later milestones — trials and runs. This file is the
// authoring schema and its strict loader; fingerprints are in fingerprint.go, planning in plan.go.
package eval

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SupportedVersion is the only suite manifest version v1 reads. A later, incompatible schema bumps
// this and refuses the old one by number rather than silently misreading it.
const SupportedVersion = 1

// Runner is which kind of system a suite evaluates: a single headless agent attempt, or the whole
// serial Coop loop over a task queue. A suite is one or the other — a loop target runs the entire
// loop, not ten standalone agent calls.
type Runner string

const (
	RunnerAgent Runner = "agent"
	RunnerLoop  Runner = "loop"
)

// Suite is one versioned YAML manifest: the workload a comparison holds fixed while the evaluated
// configuration changes. Paths are relative to the manifest's own directory (Dir), resolved once at
// load; nothing here is re-read between trials.
type Suite struct {
	Version    int    `yaml:"version"`
	Name       string `yaml:"name"`
	Runner     Runner `yaml:"runner"`
	LoopConfig string `yaml:"loop_config"` // loop suites only: the loop.yaml the scenario runs under
	Cases      []Case `yaml:"cases"`

	// Path and Dir are set by Load, never by YAML — the manifest cannot name its own location.
	Path string `yaml:"-"`
	Dir  string `yaml:"-"`
	// ContentDigest identifies the staged candidate inputs and hidden verifiers. It is absent
	// on a merely loaded suite; only StageSuite may make a comparison-ready workload.
	ContentDigest Fingerprint `yaml:"-"`
	// Load binds later staging to the exact directory and manifest it read. Neither value is
	// author-controlled YAML or serialized into a retained staged manifest.
	dirIdentity      os.FileInfo
	manifestIdentity os.FileInfo
	manifestHash     Fingerprint
}

// Case is one workload. An agent case is an instruction plus its initial files and an independent
// verifier; a loop case is a scenario — an initial repository (fixture), an ordinary Coop task
// queue (tasks) and an independent FINAL verifier. Every path is a directory or file beside the
// manifest; the verifier lives OUTSIDE the candidate's mounts and never enters its authority.
type Case struct {
	ID      string   `yaml:"id"`
	Timeout Duration `yaml:"timeout"`
	// Verifier is the independent final grader for this case — kept out of every candidate mount.
	Verifier string `yaml:"verifier"`

	// Agent-case fields.
	Instruction string `yaml:"instruction"` // the task text the model is given
	Files       string `yaml:"files"`       // an initial file tree the workspace starts from (optional)

	// Loop-case fields.
	Fixture string `yaml:"fixture"` // the initial repository tree the scenario starts from
	Tasks   string `yaml:"tasks"`   // an ordinary queue template, materialized at the trial's .agent/tasks
}

// Duration is a YAML-friendly time.Duration: it reads a Go duration string ("50m", "90s") and
// refuses a bare number or a non-positive value, so a budget is always an explicit, positive time.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("a timeout must be a duration string like \"50m\", not %q", node.Value)
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("timeout %q is not a duration (try \"50m\" or \"90s\")", raw)
	}
	if parsed <= 0 {
		return fmt.Errorf("timeout %q must be positive", raw)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// Load reads and strictly validates a suite manifest. It never launches anything and never reads a
// candidate's files — only the manifest and the shape of the paths it names. A malformed manifest,
// a duplicate id, an empty suite, a path that escapes the manifest's directory or a field that
// belongs to the other runner is refused BY NAME here, before any planning or provider work.
func Load(path string) (*Suite, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, dirIdentity, manifestIdentity, err := readManifest(abs)
	if err != nil {
		return nil, err
	}
	var suite Suite
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // an unknown key is a typo or an unsupported feature, never silently dropped
	if err := dec.Decode(&suite); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	suite.Path = abs
	suite.Dir = filepath.Dir(abs)
	suite.dirIdentity = dirIdentity
	suite.manifestIdentity = manifestIdentity
	suite.manifestHash = newHasher().bytes("manifest", data).sum()
	if err := suite.validate(); err != nil {
		return nil, err
	}
	return &suite, nil
}

// validate is the whole refusal surface, in the order a reader meets a manifest: version, name,
// runner, the loop_config rule, then each case. Every message names the field and the case.
func (s *Suite) validate() error {
	if s.Version != SupportedVersion {
		return fmt.Errorf("suite version %d is not supported; v1 reads version %d", s.Version, SupportedVersion)
	}
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("suite has no name")
	}
	switch s.Runner {
	case RunnerAgent:
		if s.LoopConfig != "" {
			return errors.New("loop_config belongs to a loop suite; an agent suite runs no loop")
		}
	case RunnerLoop:
		if strings.TrimSpace(s.LoopConfig) == "" {
			return errors.New("a loop suite needs loop_config: the loop.yaml its scenarios run under")
		}
		if err := s.relPath("loop_config", s.LoopConfig); err != nil {
			return err
		}
	case "":
		return errors.New("suite has no runner: choose \"agent\" or \"loop\"")
	default:
		return fmt.Errorf("runner %q is not supported: choose \"agent\" or \"loop\"", s.Runner)
	}
	if len(s.Cases) == 0 {
		return errors.New("suite has no cases")
	}
	seen := make(map[string]bool, len(s.Cases))
	for i := range s.Cases {
		c := &s.Cases[i]
		if err := s.validateCase(c); err != nil {
			return err
		}
		if seen[c.ID] {
			return fmt.Errorf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = true
	}
	// A verifier hidden from its own case must also be hidden from every other case. A shared
	// fixture directory that contains another case's verifier would expose its answers.
	for _, candidate := range s.Cases {
		for _, hidden := range s.Cases {
			for _, input := range []struct{ name, path string }{
				{"files", candidate.Files}, {"fixture", candidate.Fixture}, {"tasks", candidate.Tasks},
			} {
				if input.path != "" && overlaps(hidden.Verifier, input.path) {
					return fmt.Errorf("case %q %s %q overlaps case %q verifier %q; every grader must stay outside every candidate input", candidate.ID, input.name, input.path, hidden.ID, hidden.Verifier)
				}
			}
		}
	}
	return nil
}

func (s *Suite) validateCase(c *Case) error {
	if !validCaseID(c.ID) {
		return fmt.Errorf("case id %q is not a plain lower-case slug (a-z, 0-9, -)", c.ID)
	}
	if time.Duration(c.Timeout) <= 0 {
		return fmt.Errorf("case %q has no positive timeout", c.ID)
	}
	if strings.TrimSpace(c.Verifier) == "" {
		return fmt.Errorf("case %q has no verifier — the independent final grader is required", c.ID)
	}
	if err := s.relPath("case "+c.ID+" verifier", c.Verifier); err != nil {
		return err
	}
	// A case belongs to exactly its suite's runner; a field from the other kind is refused rather
	// than ignored, so a mis-authored case never runs a workload the author did not describe.
	switch s.Runner {
	case RunnerAgent:
		if c.Fixture != "" || c.Tasks != "" {
			return fmt.Errorf("case %q uses loop fields (fixture/tasks) in an agent suite", c.ID)
		}
		if strings.TrimSpace(c.Instruction) == "" {
			return fmt.Errorf("agent case %q has no instruction", c.ID)
		}
		if c.Files != "" {
			if err := s.relPath("case "+c.ID+" files", c.Files); err != nil {
				return err
			}
		}
	case RunnerLoop:
		if c.Instruction != "" || c.Files != "" {
			return fmt.Errorf("case %q uses agent fields (instruction/files) in a loop suite", c.ID)
		}
		for name, p := range map[string]string{"fixture": c.Fixture, "tasks": c.Tasks} {
			if strings.TrimSpace(p) == "" {
				return fmt.Errorf("loop case %q has no %s", c.ID, name)
			}
			if err := s.relPath("case "+c.ID+" "+name, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// overlaps reports whether two suite-relative paths are the same directory, or one contains the
// other, after cleaning. It is how the verifier is kept out of the tree the candidate is given.
func overlaps(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(ca, cb+sep) || strings.HasPrefix(cb, ca+sep)
}

// relPath refuses a reference that could reach outside the manifest's own directory, or the
// directory itself: an absolute path, a `..` escape, the suite dir (`.`, which holds the manifest
// and every verifier), or a symlink at any component. A manifest may only name real inputs that
// travel with it, so a suite copied elsewhere cannot silently pull in a grader or fixture from the
// author's machine.
func (s *Suite) relPath(field, p string) error {
	if filepath.IsAbs(p) {
		return fmt.Errorf("%s %q must be a path relative to the suite, not absolute", field, p)
	}
	clean := filepath.Clean(p)
	if clean == "." {
		return fmt.Errorf("%s %q is the suite directory itself; name a subdirectory or file", field, p)
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s %q escapes the suite directory with '..'", field, p)
	}
	if err := refuseSymlink(field, s.Dir, filepath.Join(s.Dir, clean)); err != nil {
		return err
	}
	return nil
}

// IsLoop reports whether this suite runs the whole Coop loop (as opposed to a single agent attempt).
func (s *Suite) IsLoop() bool { return s.Runner == RunnerLoop }

func validCaseID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return id[0] != '-' && id[len(id)-1] != '-'
}
