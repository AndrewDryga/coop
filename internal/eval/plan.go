package eval

import (
	"errors"
	"fmt"
	"time"
)

// ConfigKind is how an evaluated configuration was named on the command line: a bare target
// (provider[:model][/effort][@account]) or a preset. They evaluate different systems — a preset
// carries roles, delegation and fallbacks a bare target does not — so a comparison never conflates
// them, and the plan keeps them labelled.
type ConfigKind string

const (
	ConfigTarget ConfigKind = "target"
	ConfigPreset ConfigKind = "preset"
)

// Configuration is one system under test: the CLI resolves a positional into it (parsing the target
// or loading the preset) and hands it here. Milestone 1 carries its identity and label; the frozen
// content, Coop build identity and configuration fingerprint that let an edited-but-same-named
// preset compare old vs new are added when configuration freezing lands (milestone 2).
type Configuration struct {
	Kind  ConfigKind
	Label string // the exact positional the user wrote, e.g. "codex:gpt-5.6/xhigh" or "frontier"
}

func (c Configuration) String() string { return c.Label }

// Options are the run knobs the CLI parsed. Defaults (one repetition, one worker, no cost limit)
// live in the CLI; this package validates whatever it is handed.
type Options struct {
	Jobs    int           // requested concurrent workers
	Repeat  int           // repetitions per case x configuration
	Timeout time.Duration // whole-invocation deadline, including preparation, grading and cleanup
	// LoopConfigOverride replaces a loop suite's loop_config for this run, so two loop recipes can be
	// compared through separate runs. Empty means use the suite's own loop_config. Agent suites refuse it.
	LoopConfigOverride string
}

// Plan is the fixed matrix a run will execute: every (case x configuration x repetition) is one
// trial. It is built and shown BEFORE any provider work, so a user sees the case count, the
// configurations, the concurrency actually available and the total deadline up front — and so
// invalid input is refused here, never after a launch.
type Plan struct {
	Suite      *Suite
	Configs    []Configuration
	Repeat     int
	Jobs       int // effective workers, never more than the number of trials
	Timeout    time.Duration
	LoopConfig string // the loop.yaml this run uses (override or the suite's), for a loop suite
}

// BuildPlan validates the request and returns the matrix. It launches nothing. It refuses an empty
// configuration set, a non-positive repeat/jobs, a loop-config override on an agent suite, and a
// total timeout too small to give every trial its own case budget once (a plan that cannot fit its
// own work is a mistake worth naming before paid work, not a silent under-run).
func BuildPlan(s *Suite, configs []Configuration, opts Options) (*Plan, error) {
	if s == nil {
		return nil, errors.New("no suite")
	}
	if len(configs) == 0 {
		return nil, errors.New("name at least one target or preset to evaluate")
	}
	repeat := opts.Repeat
	if repeat <= 0 {
		return nil, fmt.Errorf("repeat must be positive, not %d", repeat)
	}
	jobs := opts.Jobs
	if jobs <= 0 {
		return nil, fmt.Errorf("jobs must be positive, not %d", jobs)
	}
	if opts.LoopConfigOverride != "" && !s.IsLoop() {
		return nil, errors.New("--loop-config applies only to a loop suite")
	}
	loopConfig := s.LoopConfig
	if opts.LoopConfigOverride != "" {
		loopConfig = opts.LoopConfigOverride
	}
	trials := len(s.Cases) * len(configs) * repeat
	if trials == 0 {
		return nil, errors.New("the plan has no trials")
	}
	if jobs > trials {
		jobs = trials // never reserve more workers than there is work
	}
	if opts.Timeout <= 0 {
		return nil, errors.New("a run needs a positive --timeout covering preparation, work, grading and cleanup")
	}
	if longest := s.longestCaseTimeout(); opts.Timeout < longest {
		return nil, fmt.Errorf("--timeout %s is smaller than the longest case budget %s; no trial could finish",
			opts.Timeout, longest)
	}
	return &Plan{
		Suite: s, Configs: configs, Repeat: repeat, Jobs: jobs,
		Timeout: opts.Timeout, LoopConfig: loopConfig,
	}, nil
}

// Trials is the total number of (case x configuration x repetition) attempts.
func (p *Plan) Trials() int { return len(p.Suite.Cases) * len(p.Configs) * p.Repeat }

// longestCaseTimeout is the largest single-case budget; a run must at least be able to run one.
func (s *Suite) longestCaseTimeout() time.Duration {
	var longest time.Duration
	for _, c := range s.Cases {
		if d := time.Duration(c.Timeout); d > longest {
			longest = d
		}
	}
	return longest
}
