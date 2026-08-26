package cli

import (
	"errors"
	"fmt"
	"strconv"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/loop"
	"github.com/AndrewDryga/coop/internal/loopcfg"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// parseLoopArgs resolves `coop loop`'s leading who-runs positional — a TARGET
// (provider[:model][/effort][@account,…]) OR a PRESET NAME (validated by cmdLoop's loadRunPreset) —
// and its flags. Model + account come from the target (`--model`/`--credential` are retired);
// `--peer`/`--tasks` are pre-extracted by cmdLoop. hasTarget is false and presetName "" when no
// positional was given (a loop.yaml work.agent then supplies the lead).
func parseLoopArgs(args []string, def bool) (t agents.Target, hasTarget bool, presetName string, debugOnFail, preflight, noMCP bool, maxTasks int, err error) {
	preflight = def
	t, hasTarget, presetName, rest, err := takeHeadWho(args)
	if err != nil {
		return agents.Target{}, false, "", false, preflight, false, 0, err
	}
	for i := 0; i < len(rest); i++ {
		x := rest[i]
		switch x {
		case "--debug-on-fail":
			debugOnFail = true
		case "--preflight":
			preflight = true
		case "--no-preflight":
			preflight = false
		case "--no-mcp":
			noMCP = true
		case "--max-tasks":
			if maxTasks > 0 {
				return t, hasTarget, presetName, debugOnFail, preflight, noMCP, maxTasks, errors.New("coop loop: --max-tasks may be specified only once")
			}
			if i+1 >= len(rest) {
				return t, hasTarget, presetName, debugOnFail, preflight, noMCP, 0, errors.New("coop loop: --max-tasks requires a positive integer")
			}
			i++
			n, convErr := strconv.Atoi(rest[i])
			if convErr != nil || n <= 0 {
				return t, hasTarget, presetName, debugOnFail, preflight, noMCP, 0, fmt.Errorf("coop loop: --max-tasks must be a positive integer, got %q", rest[i])
			}
			maxTasks = n
		default:
			return t, hasTarget, presetName, debugOnFail, preflight, noMCP, maxTasks, fmt.Errorf("coop loop: unexpected argument %q (usage: coop loop [<agent>[:model][/effort][@account,...] | <preset>] [--tasks <path>] [--peer <agent>]... [--max-tasks <n>] [--preflight|--no-preflight] [--no-mcp] [--debug-on-fail])", x)
		}
	}
	return t, hasTarget, presetName, debugOnFail, preflight, noMCP, maxTasks, nil
}

func (a *app) cmdLoop(args []string) (int, error) {
	flags, rest, err := tasks.ExtractTasksFlags(args)
	if err != nil {
		return 2, err
	}
	peerVals, rest, err := extractPeer(rest)
	if err != nil {
		return 2, err
	}
	peers, err := a.resolvePeers("--peer", peerVals)
	if err != nil {
		return 2, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	// .agent/loop.yaml is the committed loop config; a bad file fails fast, before any box work.
	lc, err := loopcfg.Load(repo)
	if err != nil {
		return 2, err
	}
	// preflight defaults to loop.yaml preflight.enabled; --preflight/--no-preflight override.
	t, hasTarget, presetName, debugOnFail, preflight, noMCP, maxTasks, err := parseLoopArgs(rest, lc.Preflight.Enabled)
	if err != nil {
		return 2, err
	}
	// --no-mcp: this one run mounts no MCP anywhere (the committed form is loop.yaml `mcp: false`,
	// honored inside loop.Control.Run so fork loops get it too). Blanking MCPFile is the single switch
	// the box snapshot boundary keys off, so Claude's direct args and every provider-native config
	// all stay out of the boxes.
	if noMCP {
		a.cfg.MCPFile = ""
	}
	// A positional preset name: its lead agent leads, its roles seed the run, and its models
	// ladder becomes the rotation. A positional target instead pins the one-off ladder.
	p, err := a.loadRunPreset(presetName)
	if err != nil {
		return 2, err
	}
	// .agent/loop.yaml work.agent is the committed default work ladder — used ONLY when the launch
	// names no positional who (no target and no preset). Its rungs are targets or preset names (a preset
	// rung runs the loop under that preset: its roles + lead ladder, exhausted before the next rung);
	// the first rung sets the lead agent.
	var workLadder []agents.Target
	workAgent := ""
	if !hasTarget && p == nil && len(lc.Work.Agent) > 0 {
		workAgent, p, workLadder, err = a.resolveWorkAgent(lc.Work.Agent)
		if err != nil {
			return 2, err
		}
	}
	agent := presetLeadAgent(p, t.Provider, hasTarget)
	if agent == "" {
		agent = workAgent // loop.yaml work.agent's first rung supplied the lead
	}
	if agent == "" { // provider required — no positional who (target/preset), no loop.yaml work.agent
		return 2, noProviderErr("loop")
	}
	a.applyPreset(p, agent)
	queues, err := tasks.TaskQueues(a.cfg, repo, flags)
	if err != nil {
		return 2, err
	}
	// The rotation ladder: the positional target (its model + account ladder) wins; else the
	// loop.yaml work.agent ladder; else the preset lead's ladder; else the default (agent model
	// across all signed-in accounts). expandLadder turns it into concrete one-account rungs.
	var rungs []agents.Target
	switch {
	case hasTarget:
		rungs = []agents.Target{t}
	case len(workLadder) > 0:
		rungs = workLadder
	case p != nil && agent == p.LeadAgent:
		rungs = p.LeadLadder
	}
	rot, err := a.buildRotation(agent, rungs)
	if err != nil {
		return -1, err
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	return a.loopctl().Run(loop.RunSpec{ // local loop: no fork label, no fork owner
		Repo: repo, Image: img, Agent: agent,
		Rotation: rot, Queues: queues, Preset: a.preset, Peers: peers,
		DebugOnFail: debugOnFail, Preflight: preflight, MaxTasks: maxTasks,
	})
}

// loopctl builds the loop engine for one run: the config and runtime it works with, the version
// -ldflags pinned to this package, and the three behaviors the engine needs from the CLI that
// owns the process (see internal/loop's Host doc). One Control per invocation, like forkctl().
func (a *app) loopctl() *loop.Control {
	return loop.New(a.cfg, a.rt, resolveVersion(), loop.Host{
		SweepOrphanBoxes: a.sweepOrphanBoxes,
		SignUnpushed:     a.signUnpushed,
		BuildRotation:    a.buildRotation,
	})
}

// resolveWorkAgent turns a .agent/loop.yaml work.agent ladder into the lead agent, an optional
// preset to apply (the FIRST preset rung — its roles wire the run), and the concrete target ladder
// to rotate: each preset rung expands to its lead ladder (nested — exhausted before the next rung),
// each target rung is itself. The first rung sets the lead agent. A bad preset name errors.
func (a *app) resolveWorkAgent(rungs []string) (agent string, p *preset.Preset, targets []agents.Target, err error) {
	rs, err := loopcfg.Rungs(rungs)
	if err != nil {
		return "", nil, nil, err
	}
	for _, r := range rs {
		if r.Preset != "" {
			pr, perr := a.loadRunPreset(r.Preset)
			if perr != nil {
				return "", nil, nil, fmt.Errorf("work.agent: %w", perr)
			}
			if agent == "" {
				agent = pr.LeadAgent
			}
			if p == nil {
				p = pr // apply the first preset rung's roles for the run
			}
			targets = append(targets, pr.LeadLadder...)
			continue
		}
		if agent == "" {
			agent = r.Target.Provider
		}
		targets = append(targets, *r.Target)
	}
	return agent, p, targets, nil
}
