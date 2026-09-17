package box

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/consult"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/mcp"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/taskchannel"
	"github.com/AndrewDryga/coop/internal/ui"
)

// Container labels coop stamps on its boxes so it can find and tear them down later. The SET
// sites (assembleArgs, below) and the cleanup queries/removals MUST agree —
// a label renamed on only one side would orphan running containers — so both reference these.
const (
	LabelKey            = "coop"                 // every coop box: coop=box
	LabelBox            = "box"                  //   (its value)
	LabelSupervised     = "coop.supervised"      // a supervised inner box (build/update restart it): =1
	LabelOn             = "1"                    //   (its value)
	LabelSupervisor     = "coop.sup"             // value=<supervisor id>, so a supervisor kills only its own
	LabelFork           = "coop.fork"            // readable value=<fork name> for runtime diagnostics
	LabelForkOwner      = "coop.fork-owner"      // repo-scoped value, so stop never reaps another repo's namesake
	LabelForkGeneration = "coop.fork-generation" // immutable workspace incarnation; fences reused names
	LabelForkWorker     = "coop.fork-worker"     // detached loop only; foreground/ACP boxes are never stopped with it
	LabelRun            = "coop.run"             // value=<loop run id>, so cancellation reaps the daemon-owned box
	LabelExecution      = "coop.execution"       // exact host activity record; runtime cleanup never guesses by kind/name
	// LabelHost records the HOST PROCESS supervising this box, as
	// v1:<workspace-scope>:<pid>:<start-token> — the versioned form parseSupervisorLabel reads
	// back. Every other label above names a LOGICAL owner (a run, a supervisor id, a fork); none of
	// them says which process would have removed the box, so nothing could decide whether a box left
	// behind by a SIGKILLed coop is an orphan. Cleanup is pull-only (--rm never fires on SIGKILL),
	// and this is what a later invocation in the SAME workspace pulls on: see SurveyOrphanBoxes.
	LabelHost = "coop.host"
	// DescendantsDrainedExit and DescendantsTimedOutExit are emitted only by coop-entry in an
	// opted-in supervised run. Keep them outside ordinary provider conventions and remap raw
	// provider use in the entrypoint before the host sees it.
	DescendantsDrainedExit  = 190
	DescendantsTimedOutExit = 191
)

// RunSpec describes a single container run.
type RunSpec struct {
	Image   string
	Repo    string   // host repo to mount
	Workdir string   // where Repo mounts; empty defers to resolveWorkdir (the repo's real host path)
	Cmd     []string // command + args to run in the box
	// Mode is the execution mode, fixed at creation. Empty is ModeNormal — every field below
	// then means what it always did. A restricted mode (readonly, bare) takes the separate
	// launch in restricted.go: it honors Repo, Cmd, Agent, AgentCommand, Homes, the companions,
	// labels, tty and stdio, and refuses the rest rather than widening what the box can reach.
	Mode agents.ExecutionMode
	// PolicyRepo is the trusted source for .agent/project.yaml box policy. Empty uses Repo.
	PolicyRepo string
	// RepoReadOnly mounts Repo read-only. Maintenance checks can inspect an isolated candidate
	// without letting the command alter even that disposable tree.
	RepoReadOnly bool
	// RepoReadOnlyPaths remounts real descendant directories read-only after a writable Repo bind.
	// Review stages use it for task queues so source-fixing access never grants lifecycle access.
	RepoReadOnlyPaths []string
	// Review selects the trusted review-only compose file and literal environment. The disposable
	// candidate remains writable for ignored build output; callers verify source identity afterward.
	Review bool
	// FormatCorrection is the one exact-session review-envelope repair. It keeps the selected
	// provider credential and native session under the same in-box cwd, but overlays that cwd with
	// an empty read-only directory and omits project MCP, preset peers, and sibling services.
	FormatCorrection bool `json:"-"`
	// ReuseServices is set after the same logical run prepared an unchanged sibling stack, including
	// read-only review and credential/rate-limit retries. The new box still joins and inspects it.
	ReuseServices bool `json:"-"`
	// CompanionRepositories are policy-pinned snapshots mounted read-only at
	// /coop/repositories/<name>. The remote request surface cannot populate this field.
	CompanionRepositories []CompanionRepository

	Homes   bool // mount per-agent home dirs, env-file, INSTRUCTIONS, and MCP configs
	Network bool // join the sibling-services network if `coop up` created one
	Cache   bool // mount the shared dependency cache volume

	// Agent names the launched registered agent whose credential home and
	// env-file API key this run may mount — so a plain `coop claude` box can't read the
	// other providers' credentials. Empty for a raw/maintenance run (no agent session), which
	// mounts no agent credentials at all. ConsultLead (below) widens the scope to the
	// EXPLICIT peers in Peers, since the lead is told to invoke them. See
	// credentialScope. Ignored when Homes is false.
	Agent string
	// AgentCommand says Cmd is this registered agent's ordinary interactive/headless/session CLI,
	// so box.Run may apply adapter-owned command wiring from the validated MCP snapshot. ACP and
	// maintenance commands leave it false even when Agent scopes their credential home.
	AgentCommand bool
	// Login runs only the selected provider's sign-in flow. Project service/tool setup is not
	// part of authentication; network policy and secret-shadowing protections still apply.
	Login bool

	ForceNoTTY   bool               // ACP: attach stdin (-i) but never allocate a tty
	Serve        bool               // publish .agent/project.yaml serve.ports so a dev server in the box is reachable from the host
	servePorts   []int              // validated project policy carried into argument assembly by Run
	servePlan    []servePublication // each serve port's outcome, decided once by Run for the note and the publish args
	SupervisorID string             // non-empty for a supervised inner box: tags it coop.supervised=1
	// (build/update restart it) + coop.sup=<id> (its supervisor kills exactly its boxes)
	ShareACPSessions bool   // mount credential-independent ACP transcript dirs across account switches
	ForkName         string // non-empty for a detached fork loop's box: readable runtime label
	ForkOwner        string // repo-scoped label used by `coop fork stop`; required with ForkName
	ForkGeneration   string // immutable host-owned generation; empty only for legacy callers
	ForkWorker       bool   // this box belongs to the detached worker stopped by `coop fork stop`
	// ActivityRepo is the canonical project whose host-owned execution registry receives this
	// sandbox. ActivityKind empty preserves the ordinary unregistered maintenance-box behavior.
	ActivityRepo             string
	ActivityKind             forkspace.ExecutionKind
	ActivityRole             forkspace.ExecutionRole
	ActivityTask             *forkspace.ExecutionTaskRef
	ActivitySource           string
	ActivityReservationOwner string
	activityID               string
	RunID                    string // the loop run's id; when set, injected as COOP_RUN_ID so a consult peer can append its usage to .agent/runs/<id>.peers.jsonl
	Batch                    bool   // loop/doctor: no tty, stdin from /dev/null
	// LoopPresentation opts a batch loop attempt into the loop-owned grouped setup narration.
	// Batch keeps its execution meaning; other batch/quiet/ACP callers remain silent.
	LoopPresentation bool `json:"-"`
	// SuperviseDescendants keeps coop-entry alive after a successful provider exit long enough to
	// drain agent-owned background jobs. It is intentionally opt-in: an interactive box retains
	// the ordinary exec contract and never waits for a shell job the user started.
	SuperviseDescendants bool
	Quiet                bool      // suppress the "shadowed N secret path(s)" line (doctor)
	Stdout               io.Writer // capture output (doctor); nil means inherit os.Stdout
	Stderr               io.Writer // capture/discard the container's stderr; nil means inherit os.Stderr
	ExtraArgs            []string  // extra runtime args for this run (e.g. doctor's probe mount)

	// CapturedEgress is host-owned frozen network authority, produced by
	// AdmitNetwork before launch. It is never populated from a request or a
	// serialized spec: a box cannot grant itself network access.
	CapturedEgress *CapturedEgress `json:"-"`
	// OnNetworkReport, when set, receives this run's filtered-networking summary
	// once cleanup sealed the receipt — in EVERY mode, including the batch and
	// quiet ones this package prints nothing for. A caller that owns its own
	// output (the loop) surfaces the refusals there; a nil hook is the ordinary
	// run. Host-side only, like the capture it reports on.
	OnNetworkReport func(NetworkReport) `json:"-"`
	// NetworkClient is the client kind this run launches, so filtered admission
	// asks each provider for the endpoints that client actually needs. Empty is
	// the ordinary CLI; an ACP launch sets it explicitly.
	NetworkClient egress.Client
	// NetworkAdmission marks the synthetic whole-loop spec used only to freeze one
	// policy before any iteration starts. CredentialBrokerLoop says that loop has no
	// peer or preset execution shape, so its provider ladder may use the direct-run
	// broker qualification instead of being mistaken for an executable peer run.
	NetworkAdmission     bool `json:"-"`
	CredentialBrokerLoop bool `json:"-"`
	projectEnv           map[string]string
	// networkSmoke is the host preflight permit. Unexported on purpose: only the
	// in-package setup workflow can drive a smoke through this same engine, so
	// what it proves is exactly what a workload later gets.
	networkSmoke *networkSmokeLaunch

	// Ctx, when non-nil, makes the run cancelable: the container runs in its own process group
	// and canceling Ctx tears it down (SIGTERM→SIGKILL). The loop sets this so a second Ctrl-C
	// stops the current iteration now; every other caller leaves it nil — the plain, today's run.
	Ctx context.Context

	// OnRuntimeLaunch, when set, is called exactly once at the runtime-launch boundary: after
	// every host-side step (filesystem projection, sibling services, network inspection, argument
	// assembly) and immediately before the container starts. The loop arms its provider-attempt
	// watchdog here, so a slow host setup is never charged to the provider as silence — and a
	// deadline can only cancel work Ctx can actually reach. It must return promptly: the launch
	// waits on it. A nil hook is the ordinary run, signaling nothing.
	OnRuntimeLaunch func()

	// StartingNotice is optional interactive guidance directly below the Starting heading.
	// It is presentation only, never a runtime callback or serialized launch authority.
	StartingNotice string `json:"-"`

	// ConsultLead names the lead agent of a consult-capable run: it gets a
	// light, optional "second opinion" directive merged into its instruction file,
	// naming the EXPLICIT peers (Peers) it may consult read-only on hard calls. Scoped
	// to the lead so peers it spawns don't recurse. Empty = no consult directive.
	ConsultLead string

	// AssignedTask is the loop task this iteration owns. The box's prepare-commit-msg hook stamps
	// its Coop-Task trailer, so an agent that forgets one does not lose the whole completion.
	// Empty outside a loop work iteration — nothing is assigned, so nothing is stamped.
	AssignedTask string

	// TaskTools, when set, gives the box coop's task tools: a run-private channel (taskchannel.go)
	// serves this server — internal/taskmcp's, built by the caller from the queue it owns — the
	// box mounts the channel's socket read-only, and, with Homes, the `coop-tasks` MCP server is
	// bound into every provider projection. The loop sets it for a work iteration; nil everywhere
	// else means nothing is mounted or bound.
	TaskTools  TaskToolServer
	taskVolume string // the channel's run-private volume name, chosen by Run

	// Peers is the EXPLICIT peer set for this run — the targets named by repeatable
	// --peer (a normal run, ACP, or a loop run), each provider[:model] (no
	// account: a peer runs on its default). It REPLACES the old implicit "every authed
	// agent is a peer" policy: only these providers' credentials mount as peers, only
	// they are named in the consult directive, and the in-box coop-consult refuses any
	// other (COOP_PEERS). A preset's own consult/delegate roles join separately (their
	// role agents also mount + become consultable). Empty = the lead consults no ad-hoc peer.
	Peers []agents.Target

	// Preset, when set, is the loaded orchestration preset for this run: the lead's
	// instruction file gets the generated routing block (roles, modes, exact consult/
	// delegate invocations) instead of the generic consult directive, consult/delegate
	// role agents join the credential scope, and a delegate role mounts coop-delegate
	// plus its per-role contracts and env. The cli loads and applies the preset's
	// model/credential selections before calling Run.
	Preset *preset.Preset
}

func runServiceOwner(spec RunSpec) string {
	// A remote session already owns a dedicated reserved fork workspace. Its workspace-scoped
	// project is therefore private and, unlike the per-turn RunID, stable across the session.
	if spec.ActivityKind == forkspace.ExecutionRemoteSession {
		return ""
	}
	if spec.RunID != "" {
		return spec.RunID
	}
	return spec.ActivitySource
}

type CompanionRepository struct {
	Name       string `json:"name"`
	HostPath   string `json:"-"`
	BaseCommit string `json:"base_commit"`
}

type companionRepositoryEnvironment struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	BaseCommit string `json:"base_commit"`
}

// ttyMode is how stdin and the tty are wired for a run.
type ttyMode int

const (
	ttyNone        ttyMode = iota // no -i/-t; stdin not attached (batch, piped)
	ttyInteractive                // -it; an interactive terminal
	ttyStdinOnly                  // -i; stdin attached without a tty (ACP)
)

// extraMount is a generated host file mounted read-only at a box path.
type extraMount struct{ host, box string }

// instructionFile is the agent's native global instruction filename — where coop
// mounts the shared INSTRUCTIONS.md or the lead's augmented instructions — or ""
// for an unknown agent. Owned by each adapter.
func instructionFile(name string) string {
	if ag, ok := agents.Get(name); ok {
		return ag.InstructionFile()
	}
	return ""
}

type compositionArtifactOps struct {
	// parent is the directory generated artifacts are created in. Empty uses
	// the system temp dir; a filtered run points it at the execution's private
	// artifact directory so exact-owned cleanup covers everything it mounts.
	parent            string
	writeFile         func(parent, content string) (string, error)
	chmod             func(string, os.FileMode) error
	assembleAgentsDir func(parent string, files []genFile) (string, error)
	gitHookDir        func(parent string) (string, error)
}

func defaultCompositionArtifactOps() compositionArtifactOps {
	return compositionArtifactOps{
		writeFile: writeTempFile, chmod: os.Chmod, assembleAgentsDir: assembleAgentsDir,
		gitHookDir: gitHookDir,
	}
}

// ctxStep is the step-boundary Ctx check threaded between runWithCompositionArtifacts' discrete
// setup phases (filesystem projection, sibling services, network inspection, argument assembly —
// see the OnRuntimeLaunch boundary these same names describe). A canceled Ctx aborts HERE, at the
// next boundary, naming the phase it would have started — never mid-syscall: a wedged `compose up`
// or `network inspect` runs via plain exec with no context of its own, so there is no lever to pull
// inside one. A nil Ctx (every caller but the loop) or a live, uncanceled one always returns nil:
// zero behavior change unless a caller both sets Ctx AND cancels it.
func ctxStep(ctx context.Context, step string) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("box setup canceled before %s: %w", step, err)
	}
	return nil
}

// Run assembles and executes one container run, shadowing secrets and wiring up
// agent homes + MCP. It returns the container's exit code (with a nil error when
// the container merely exited non-zero); a non-nil error means it never started.
func Run(cfg *config.Config, rt runtime.Runtime, spec RunSpec) (int, error) {
	return runWithCompositionArtifacts(cfg, rt, spec, defaultCompositionArtifactOps())
}

// runWithNetworkSmoke is the private preflight entry point. The setup workflow
// drives the ordinary launch engine with a smoke permit; nothing outside this
// package can construct one, and RunSpec exposes no bypass.
func runWithNetworkSmoke(cfg *config.Config, rt runtime.Runtime, spec RunSpec, artifacts compositionArtifactOps, smoke *networkSmokeLaunch) (int, error) {
	spec.networkSmoke = smoke
	return runWithCompositionArtifacts(cfg, rt, spec, artifacts)
}

// runWithCompositionArtifacts keeps failure injection local to composition tests. Declared
// orchestration files are part of the requested program, so any assembly error must surface before
// the box and provider start.
func runWithCompositionArtifacts(cfg *config.Config, rt runtime.Runtime, spec RunSpec, artifacts compositionArtifactOps) (exitCode int, result error) {
	// Checked before any host work, not just between later phases: an already-canceled Ctx (the
	// loop's second Ctrl-C landing between iterations) must never begin projecting a box it would
	// only have to tear down.
	if err := ctxStep(spec.Ctx, "filesystem projection"); err != nil {
		return -1, err
	}
	// A restricted mode takes its own launch, whose whole point is that nothing below — homes,
	// caches, services, project policy, generated mounts — is assembled for it.
	if mode, err := executionMode(spec); err != nil {
		return -1, err
	} else if mode.Restricted() {
		return runRestricted(cfg, rt, spec, artifacts, mode)
	}
	if !spec.Homes {
		if spec.Preset != nil {
			return -1, fmt.Errorf("preset %q requires agent homes so its instructions, wrappers, and roles can mount", spec.Preset.Name)
		}
		if spec.ConsultLead != "" || len(spec.Peers) > 0 {
			return -1, fmt.Errorf("consult requires agent homes so its instructions, wrapper, and peer credentials can mount")
		}
	}
	if spec.Login {
		if !spec.Homes || !agents.Valid(spec.Agent) {
			return -1, errors.New("sign-in requires the selected provider's writable credential home")
		}
		spec.Preset, spec.Peers, spec.ConsultLead = nil, nil, ""
		spec.TaskTools, spec.AssignedTask, spec.CompanionRepositories = nil, "", nil
		spec.AgentCommand, spec.ShareACPSessions = false, false
		spec.Workdir = "/tmp" // native login must not load project config or warn about running in HOME
	}
	if (spec.ForkName == "") != (spec.ForkOwner == "") {
		return -1, errors.New("fork box requires both name and scoped owner")
	}
	if spec.ForkGeneration != "" && spec.ForkName == "" {
		return -1, errors.New("fork generation requires a fork name")
	}
	if spec.ForkWorker && spec.ForkGeneration == "" {
		return -1, errors.New("detached fork worker label requires a fork generation")
	}
	if spec.networkSmoke != nil && spec.CapturedEgress == nil {
		return -1, errors.New("network preflight requires a filtered capture")
	}
	policyRepo := projectPolicyRepo(spec)
	p, err := project.Load(policyRepo)
	if err != nil {
		return -1, err
	}
	cfg = applyProjectPolicy(cfg, p, &spec)
	projectEnv := p.Box.Env
	spec.projectEnv = projectEnv
	composeFile := ComposeFileAt(spec.Repo, p.ComposeRel())
	serviceOwner := runServiceOwner(spec)
	spec.servePorts = p.Serve.Ports
	if spec.Login {
		// Apply after project policy so a project's network toggle cannot turn services back on.
		spec.Network, spec.Serve = false, false
		projectEnv, composeFile, spec.servePorts = nil, "", nil
	}
	if spec.FormatCorrection {
		if !spec.AgentCommand || !spec.Batch || !spec.RepoReadOnly {
			return -1, errors.New("review format correction requires a read-only batch agent command")
		}
		spec.Preset, spec.Peers, spec.ConsultLead = nil, nil, ""
		spec.TaskTools, spec.AssignedTask, spec.CompanionRepositories = nil, "", nil
		projectEnv, composeFile, spec.servePorts = nil, "", nil
		spec.projectEnv = nil
		spec.Network, spec.Serve, spec.Cache = false, false, false
		spec.LoopPresentation, spec.Quiet = false, true
	}
	// Apply project policy before this last fail-closed check. Callers normally
	// admit first (which marks the resolved mode explicit), but a missed caller
	// must not turn a project's filtered request into an ordinary offline box.
	if cfg.Egress == "filtered" && spec.CapturedEgress == nil {
		return -1, errors.New("restricted networking requires host policy capture before box launch")
	}
	workdir := resolveWorkdir(spec, cfg)
	if spec.Homes {
		if err := ensureAgentHomes(cfg, spec); err != nil {
			return -1, err
		}
	}
	if spec.Review && !spec.FormatCorrection {
		if p.Review.Compose != "" {
			composeFile = ComposeFileAt(spec.Repo, p.Review.Compose)
		}
		spec.ExtraArgs = append(spec.ExtraArgs, "-e", "COOP_REVIEW=1")
		keys := make([]string, 0, len(p.Review.Env))
		for key := range p.Review.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			// Explicit -e arguments follow --env-file, so trusted review policy cannot be
			// weakened by an operator's ordinary agent environment.
			spec.ExtraArgs = append(spec.ExtraArgs, "-e", key+"="+p.Review.Env[key])
		}
	}
	var mcpSnapshot []byte
	mcpPresent := false
	if spec.Homes && !spec.Login && !spec.FormatCorrection {
		mcpSource := cfg.MCPFile
		if cfg.MCPFile != "" {
			var err error
			mcpSource, err = validateMCPSourceIsolation(cfg, spec)
			if err != nil {
				return -1, err
			}
		}
		var err error
		mcpSnapshot, mcpPresent, err = mcp.ReadValidatedSnapshot(mcpSource)
		if err != nil {
			return -1, fmt.Errorf("mcp.json: %w", err)
		}
	}
	if spec.TaskTools != nil {
		volume, err := taskChannelVolume()
		if err != nil {
			return -1, fmt.Errorf("task channel: %w", err)
		}
		spec.taskVolume = volume
		if spec.Homes {
			// Bound before the snapshot artifact is written below, so every provider projection —
			// claude's --mcp-config, codex TOML, gemini settings, ACP — carries the server unchanged.
			mcpSnapshot, err = mcp.BindTaskTools(mcpSnapshot, taskchannel.BoxSocketPath)
			if err != nil {
				return -1, fmt.Errorf("task channel: %w", err)
			}
			mcpPresent = true
		}
	}
	var mounts []Mount
	if !spec.Login {
		mounts, err = ComputeMounts(spec.Repo, workdir)
		if err != nil {
			return -1, err
		}
	}
	if spec.FormatCorrection {
		mounts = []Mount{{Kind: DirDecoy, Target: workdir, RO: true}}
	} else if spec.RepoReadOnly && len(mounts) > 0 {
		mounts[0].RO = true // ComputeMounts guarantees the primary repo bind is first
	}
	if !spec.Login && !spec.RepoReadOnly && len(spec.RepoReadOnlyPaths) > 0 {
		protected, err := repoReadOnlyPathMounts(spec.Repo, workdir, spec.RepoReadOnlyPaths)
		if err != nil {
			return -1, err
		}
		mounts = append(mounts, protected...)
	}
	companionMounts, companionEnvironment, err := companionRepositoryMounts(
		spec.CompanionRepositories,
	)
	if err != nil {
		return -1, err
	}
	mounts = append(mounts, companionMounts...)
	if len(companionEnvironment) > 0 {
		data, err := json.Marshal(companionEnvironment)
		if err != nil {
			return -1, fmt.Errorf("encode companion repository environment: %w", err)
		}
		spec.ExtraArgs = append(
			spec.ExtraArgs,
			"-e", "COOP_COMPANION_REPOSITORIES_JSON="+string(data),
		)
	}
	// Interactive launches and loop attempts are narrated in sections (launch_sections.go); other
	// embeddings keep their one-line log. A filtered run launches the qualified client image, not this
	// repo's — so a stale-image nudge would point at a rebuild that changes nothing about it.
	sections := newLaunchSections(spec)
	var nudges []string
	if !spec.Batch && !spec.Quiet && spec.CapturedEgress == nil {
		nudges = StalenessNudges(cfg, spec.Repo, spec.Image)
	}
	sections.box(nudges)
	if n := ShadowCount(mounts); sections.on {
		sections.secrets(n)
	} else if n > 0 && !spec.Quiet {
		ui.Note("shadowed %d secret path(s)", n)
	}
	// The sibling-services compose file is NOT shadowed: an in-box agent may author it, but coop
	// validates it host-side before auto-running it (box.ValidateComposeFile in EnsureServices), so
	// it can only ever declare a repo-scoped, loopback-only container — never host root. That
	// removes the read-only decoy that used to strand an empty .agent/compose.yml in the repo.
	if !sections.on {
		for _, nudge := range nudges {
			ui.Note("%s", nudge)
		}
	}

	// Registered before a filtered run's exact-owned cleanup so it runs AFTER it: the channel's
	// volume can only go once the box that mounted it is gone, and a filtered workload is removed
	// by that cleanup, not by --rm.
	var channel *taskChannel
	defer func() {
		if channel != nil {
			if err := channel.close(); err != nil {
				// The launch itself is unaffected: only live task updates are missing from it.
				ui.Warning("Task updates are unavailable in this box", err.Error(), "")
			}
		}
	}()

	var filtered *filteredExecution
	var execution forkspace.ExecutionRecord
	var interrupt *hostInterrupt // the host signal that cancelled this run, if one did
	// A failure from here to the main process is rendered once, under the section in progress,
	// after every cleanup below has said its piece — so the reason names all of it — and comes
	// back marked reported. A failure AFTER the main process started is the run's own; only a
	// cancellation the stop line already explained is marked, and only when teardown added
	// nothing a person still has to read.
	started := false
	var teardownErr error
	// The truthful teardown reason, taken where the main process returns and printed by the
	// filtered cleanup below — "has stopped" is a claim about the box, so it waits until the
	// removal that backs it is confirmed. Empty while the box runs, and for one that never started.
	stopped := ""
	defer func() {
		switch {
		case !started:
			result = sections.failed(result)
		case teardownErr == nil:
			result = sections.explained(result, interrupt)
		}
	}()
	if spec.CapturedEgress != nil {
		// COOP_RUN_ARGS and this run's own extra arguments are reduced to bind
		// mounts and environment assignments, checked by the same filtered
		// exposure rules as every other bind. Everything else is refused: an
		// unqualified runtime argument can undo the boundary the capture was
		// frozen for.
		extra, argErr := filteredExtraArgs(cfg.ExtraRunArgs, spec.ExtraArgs)
		if argErr != nil {
			return -1, argErr
		}
		spec.ExtraArgs = extra
		if spec.Ctx == nil {
			// A filtered box is torn down by THIS process, so an interrupt has to arrive
			// as a cancellation the cleanup below can act on — and teardown names the
			// signal it was (hostInterrupt).
			spec.Ctx, interrupt = newHostInterrupt()
		}
		filtered, err = prepareFilteredExecution(spec.Ctx, cfg, rt, spec, spec.CapturedEgress, composeFile, spec.networkSmoke, sections)
		if filtered != nil {
			defer func() {
				workload := filtered.workloadOutcome(exitCode, result, spec.Ctx.Err() != nil)
				// This process owns the box, its gateway, its volumes and its receipt: none of it
				// is --rm, so the stop is only real once cleanup says so. A teardown slow enough
				// to look like a hang says what it is waiting on while it runs.
				settled := func() {}
				if stopped != "" {
					settled = sections.stopping()
				}
				gone, cleanupErr := filtered.cleanup(workload)
				settled()
				if execution.ID != "" && gone {
					cleanupErr = errors.Join(cleanupErr, forkspace.EndExecution(spec.ActivityRepo, execution))
				}
				result, teardownErr = errors.Join(result, cleanupErr), cleanupErr
				// After sealing, so the summary reports the receipt's own
				// evidence rather than a snapshot cleanup was still amending.
				// The hook fires in every mode — the loop and every quiet
				// embedding surface the same facts in their own output — while
				// the full run projection belongs to an interactive box that
				// reached its main process: before that there is no traffic to
				// report, and the failure is the whole story.
				report := filtered.report()
				if report.RunID != "" && spec.OnNetworkReport != nil {
					spec.OnNetworkReport(report)
				}
				if sections.interactive && filtered.started() {
					// Only a confirmed removal earns the completed sentence; a cleanup that
					// could not finish leaves the box's fate to the error it just returned.
					if gone && cleanupErr == nil {
						sections.stopped(stopped)
					}
					filtered.printRun()
				}
			}()
		}
		if err != nil {
			return -1, err
		}
		// The workload runs the qualified client image, never a repo image.
		spec.Image = filtered.image
		artifacts.parent = filtered.runfiles
		if err := filtered.prepareCredentialBroker(artifacts); err != nil {
			return -1, err
		}
		if filtered.broker != nil {
			spec.Cmd = filtered.broker.candidate.command(spec.Cmd)
		}
	}
	var policy *egress.Snapshot
	if filtered != nil {
		policy = &filtered.policy
	}
	brokerProvider := ""
	if filtered != nil && filtered.broker != nil {
		brokerProvider = filtered.broker.candidate.provider
	}
	sections.internet(cfg, spec, policy, brokerProvider)
	// Whatever a box may reach is fully known before it starts, so the launch
	// instructions say it. An agent that learns its own boundary by being
	// refused burns a turn and reports policy as a broken tool or a dead host.
	networkNote := ""
	if filtered != nil {
		networkNote = networkInstructionNote(filtered.policy)
		if brokerProvider != "" {
			networkNote += "\nYour provider API is available only through Coop's session-bound credential broker; the reusable key is not in this box."
		}
	}

	// A single empty read-only file shadows every secret file; a single empty read-only
	// dir shadows every secret directory (an RO bind, not --tmpfs, so ordering cannot re-expose it).
	decoy, err := os.CreateTemp(artifacts.parent, "coop-decoy-")
	if err != nil {
		return -1, err
	}
	decoy.Close()
	defer os.Remove(decoy.Name())
	decoyDir, err := os.MkdirTemp(artifacts.parent, "coop-decoy-dir-")
	if err != nil {
		return -1, err
	}
	defer os.RemoveAll(decoyDir)

	mode := decideTTY(spec, ui.IsTerminal(os.Stdin))
	var stdin io.Reader
	if mode == ttyInteractive || mode == ttyStdinOnly {
		stdin = os.Stdin
	}
	stdout := spec.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := spec.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	// Generate agent configs into temp files that live for the container's run.
	var tmpFiles []string
	var tmpDirs []string
	defer func() {
		for _, f := range tmpFiles {
			os.Remove(f)
		}
		for _, d := range tmpDirs {
			os.RemoveAll(d)
		}
	}()
	if filtered != nil {
		for i := range mounts {
			if mounts[i].Kind != Policy {
				continue
			}
			data, err := os.ReadFile(mounts[i].Source)
			if err != nil {
				return -1, fmt.Errorf("snapshot %s: %w", CoopIgnoreFile, err)
			}
			path, err := artifacts.writeFile(artifacts.parent, string(data))
			if err != nil {
				return -1, fmt.Errorf("snapshot %s: %w", CoopIgnoreFile, err)
			}
			mounts[i].Source = path
			tmpFiles = append(tmpFiles, path)
		}
	}
	var mcpMounts []extraMount
	if filtered != nil {
		if mount, path, err := filtered.credentialBrokerMarkerMount(artifacts, cfg.HomeInBox); err != nil {
			return -1, err
		} else if path != "" {
			tmpFiles = append(tmpFiles, path)
			mcpMounts = append(mcpMounts, mount)
		}
	}
	rawMCP := false
	if mcpPresent {
		path, err := artifacts.writeFile(artifacts.parent, string(mcpSnapshot))
		if err != nil {
			return -1, fmt.Errorf("snapshot mcp.json: %w", err)
		}
		tmpFiles = append(tmpFiles, path)
		snapshotConfig := *cfg
		snapshotConfig.MCPFile = path
		cfg = &snapshotConfig
	}
	configAgents := credentialScope(cfg, spec)
	configForAgent := cfg
	if !mcpPresent {
		// Without shared MCP, ask only adapters whose homes this box mounts. Most return
		// nothing (or require MCP); an adapter may still supply an always-on box config.
		withoutMCP := *cfg
		withoutMCP.MCPFile = ""
		configForAgent = &withoutMCP
	}
	type generatedMCP struct {
		name   string
		wiring agents.MCPConfig
	}
	generated := make([]generatedMCP, 0, len(configAgents))
	var requiredMCPEnv []string
	for _, name := range configAgents {
		ag, _ := agents.Get(name)
		var wiring agents.MCPConfig
		var genErr error
		if spec.Login {
			wiring, genErr = ag.LoginConfig(configForAgent)
		} else {
			wiring, genErr = ag.MCP(configForAgent, workdir)
		}
		if genErr != nil {
			return -1, fmt.Errorf("assemble MCP config for %s: %w", name, genErr)
		}
		generated = append(generated, generatedMCP{name: name, wiring: wiring})
		requiredMCPEnv = append(requiredMCPEnv, wiring.RequiredEnv...)
	}

	for _, item := range generated {
		name, wiring := item.name, item.wiring
		if spec.Login && name == spec.Agent {
			spec.Cmd = append(spec.Cmd, wiring.CommandArgs...)
		}
		for _, env := range wiring.Env {
			spec.ExtraArgs = append(spec.ExtraArgs, "-e", env)
		}
		if mcpPresent && spec.AgentCommand && name == spec.Agent && len(wiring.CommandArgs) > 0 {
			spec.Cmd = append(spec.Cmd, wiring.CommandArgs...)
			rawMCP = true
		}
		if mcpPresent && nestedAgentCommand(spec, name) && len(wiring.NestedCommandEnv) > 0 {
			for _, env := range wiring.NestedCommandEnv {
				spec.ExtraArgs = append(spec.ExtraArgs, "-e", env)
			}
			rawMCP = true
		}
		for _, m := range wiring.Mounts {
			p, err := artifacts.writeFile(artifacts.parent, m.Content)
			if err != nil {
				if mcpPresent || spec.Login {
					return -1, fmt.Errorf("write MCP config for %s: %w", name, err)
				}
				continue
			}
			tmpFiles = append(tmpFiles, p)
			mcpMounts = append(mcpMounts, extraMount{p, m.BoxPath})
		}
	}

	// Second opinions: a normal lead may consult its authenticated peers read-only
	// on hard calls. The directive is merged into the lead's instruction file only
	// (so peers it spawns read their normal instructions and never recurse), and
	// only when a peer is actually authenticated. With NO authed peer the lead still
	// gets its base instructions mounted here (the box env note + the user's) — it is
	// excluded from instructionPlan as the lead, so mounting nothing would leave it
	// running with no instructions at all.
	var consultMounts []extraMount
	consultWired := false
	if spec.Homes && spec.ConsultLead != "" {
		content, file, wired, ok, err := leadInstructionMount(cfg, spec.ConsultLead, spec.Preset, peerProviders(spec.Peers), networkNote)
		if err != nil {
			return -1, fmt.Errorf("assemble lead instruction for %s: %w", spec.ConsultLead, err)
		}
		if ok {
			p, err := artifacts.writeFile(artifacts.parent, content)
			if err != nil {
				return -1, fmt.Errorf("assemble lead instruction for %s: %w", spec.ConsultLead, err)
			}
			tmpFiles = append(tmpFiles, p)
			consultMounts = append(consultMounts, extraMount{p, cfg.HomeInBox + "/." + spec.ConsultLead + "/" + file})
			consultWired = wired
		} else {
			return -1, fmt.Errorf("assemble lead instruction: provider %q has no instruction file", spec.ConsultLead)
		}
	}

	// coop-consult: mount the read-only wrapper whenever a consult directive was injected, so the
	// lead's `coop-consult <peer|role>` calls resolve.
	// It carries the per-agent session-id mechanics for cross-turn continuity.
	if consultWired {
		p, err := artifacts.writeFile(artifacts.parent, consult.ConsultWrapper())
		if err != nil {
			return -1, fmt.Errorf("assemble consult wrapper: %w", err)
		}
		tmpFiles = append(tmpFiles, p)
		if err := artifacts.chmod(p, 0o755); err != nil {
			return -1, fmt.Errorf("make consult wrapper executable: %w", err)
		}
		consultMounts = append(consultMounts, extraMount{p, consult.ConsultWrapperPath})
	}

	// Preset roles — the coop-delegate wrapper + delegate role env, generated native
	// subagents, and each consult-wired role's persona + env — are wired in one
	// place; fold what it produced into this run's mounts, args, and cleanup lists.
	proleMounts, proleArgs, proleFiles, proleDirs, err := presetRoleMounts(cfg, spec, artifacts)
	if err != nil {
		return -1, err
	}
	consultMounts = append(consultMounts, proleMounts...)
	spec.ExtraArgs = append(spec.ExtraArgs, proleArgs...)
	tmpFiles = append(tmpFiles, proleFiles...)
	tmpDirs = append(tmpDirs, proleDirs...)

	// Every agent gets the box environment note, then the user's instructions (a per-agent
	// override if present, else the shared INSTRUCTIONS.md), mounted at its native global path
	// — so it never burns a turn rediscovering the box. The consult lead is handled above, with its
	// augmented file.
	plan, err := instructionPlan(cfg, spec, networkNote)
	if err != nil {
		return -1, err
	}
	var instructionMounts []extraMount
	for _, it := range plan {
		p, err := artifacts.writeFile(artifacts.parent, it.content)
		if err != nil {
			return -1, fmt.Errorf("assemble instruction for %s: %w", it.agent, err)
		}
		tmpFiles = append(tmpFiles, p)
		instructionMounts = append(instructionMounts, extraMount{p, cfg.HomeInBox + "/." + it.agent + "/" + it.file})
	}
	// Synthesize workflow skills from the repo's shared source when it has no per-agent skills dir —
	// so a repo can omit committed adapter directories and still give each agent its skills, mounted
	// USER-level at ~/.<agent>/skills (writable copy, dies with the box). A project skills dir wins,
	// like the subagents mount.
	privateRoots := append(ConfigExposureRoots(cfg), projectPolicyRepo(spec))
	for _, companion := range spec.CompanionRepositories {
		privateRoots = append(privateRoots, companion.HostPath)
	}
	workflowAgents := configAgents
	if spec.Login || spec.FormatCorrection {
		workflowAgents = nil
	}
	synthMounts, synthDirs, err := synthSkillsMounts(spec.Repo, cfg.HomeInBox, workflowAgents, privateRoots...)
	if err != nil {
		return -1, err
	}
	tmpDirs = append(tmpDirs, synthDirs...)
	if spec.Homes {
		homeMounts, homeDirs, err := synthHomeFallbackMounts(spec.Repo, cfg.HomeInBox, workflowAgents, privateRoots...)
		if err != nil {
			return -1, err
		}
		synthMounts = append(synthMounts, homeMounts...)
		tmpDirs = append(tmpDirs, homeDirs...)
	}

	// Git environment: a curated ~/.gitconfig (your identity + signing off, since the
	// box holds no key) and your global gitignore, mounted into every box run. Without
	// it the agent would commit with no author and ignore none of your global patterns.
	var gitMounts []extraMount
	if spec.Homes && !spec.Login {
		// coop's own co-author trailer, applied by a prepare-commit-msg hook mounted into the box —
		// so a box commit is attributed to coop and its target, replacing whatever the agent CLI
		// stamps. Empty for a raw run (no agent), which then gets no hook and no trailer.
		coAuthor := boxCommitTrailer(cfg, spec)
		hooksPath := ""
		if coAuthor != "" || spec.AssignedTask != "" {
			dir, err := artifacts.gitHookDir(artifacts.parent)
			if err != nil {
				return -1, fmt.Errorf("prepare box Git hook: %w", err)
			}
			tmpDirs = append(tmpDirs, dir)
			hooksPath = filepath.Join(cfg.HomeInBox, boxGitHooksName)
			gitMounts = append(gitMounts, extraMount{dir, hooksPath})
		}
		excludesPath := ""
		if gi := hostGlobalGitignore(); gi != "" {
			if p, err := artifacts.writeFile(artifacts.parent, gi); err == nil {
				tmpFiles = append(tmpFiles, p)
				excludesPath = filepath.Join(cfg.HomeInBox, boxGitIgnoreName)
				gitMounts = append(gitMounts, extraMount{p, excludesPath})
			} else {
				ui.Warning("The box could not use your global Git ignore file", err.Error(), "")
			}
		}
		p, err := artifacts.writeFile(artifacts.parent, gitConfigForBox(coAuthor, hooksPath, excludesPath, spec.AssignedTask))
		if err != nil {
			return -1, fmt.Errorf("prepare box Git config: %w", err)
		}
		tmpFiles = append(tmpFiles, p)
		gitMounts = append(gitMounts, extraMount{p, cfg.HomeInBox + "/.gitconfig"})
	}

	// All required host artifacts now exist. Only after the whole set succeeds may the runtime or
	// first-run defaults have side effects; a broken selected-provider file must never start Docker
	// or leave an earlier provider home partially initialized.
	if filtered == nil {
		if _, err := selectCredentialBroker(cfg, spec); err != nil {
			return -1, err
		}
	}
	if err := rt.EnsureDaemon(); err != nil {
		return -1, err
	}
	if spec.Homes {
		if err := ensureAgentDefaults(cfg, spec, workdir); err != nil {
			return -1, err
		}
		// An ACP box shares the lead's session transcripts across credentials (see assembleArgs), so
		// ensure that shared store exists before it's mounted.
		if spec.ShareACPSessions {
			if ag, ok := agents.Get(runPrimary(spec)); ok {
				for _, name := range ag.ACPSessionDirs() {
					_ = os.MkdirAll(filepath.Join(acpSharedDir(cfg, runPrimary(spec)), name), 0o700)
				}
			}
		}
	}

	// The task channel is a runtime side effect too, so it starts only now — and before the box,
	// which must find the socket already listening.
	if spec.TaskTools != nil {
		script, err := writeTaskChannelScript(artifacts)
		if err != nil {
			return -1, err
		}
		tmpFiles = append(tmpFiles, script)
		channelCtx := spec.Ctx
		if channelCtx == nil {
			channelCtx = context.Background()
		}
		channel, err = startTaskChannel(channelCtx, rt, spec.Image, spec.taskVolume, spec.RunID, spec.TaskTools, script)
		if err != nil {
			return -1, err
		}
	}

	var authMarkers map[string]bool
	if filtered != nil {
		authMarkers = filtered.authMarkers
	}
	envFile, envTmp, err := prepareBoxEnvFileWithMarkers(cfg, spec, artifacts, projectEnv, authMarkers)
	if err != nil {
		return -1, err
	}
	if envTmp != "" {
		tmpFiles = append(tmpFiles, envTmp)
	}
	if filtered != nil && filtered.broker != nil {
		brokerEnv, err := filtered.credentialBrokerEnv(artifacts, envFile)
		if err != nil {
			return -1, err
		}
		envFile = brokerEnv
		tmpFiles = append(tmpFiles, brokerEnv)
	}

	if err := ctxStep(spec.Ctx, "sibling services"); err != nil {
		return -1, err
	}
	// Publish before any sibling service or runtime side effect. Fork-bound publication takes the
	// same lifecycle lock as rm/fresh/merge and validates the exact workspace generation, so either
	// the reservation wins and mutation refuses, or mutation wins and this launch fails closed.
	finish := func(code int, runErr error) (int, error) {
		// A filtered run ends its activity only after exact runtime cleanup has
		// confirmed the workload is gone.
		if execution.ID != "" && filtered == nil {
			if cleanupErr := forkspace.EndExecution(spec.ActivityRepo, execution); cleanupErr != nil {
				ui.Warning("Coop could not finish recording this box's activity", cleanupErr.Error(),
					"Inspect what was recorded with coop tasks watch --json.")
			}
			execution = forkspace.ExecutionRecord{}
		}
		return code, runErr
	}
	if spec.ActivityKind != "" {
		if !filepath.IsAbs(spec.ActivityRepo) || filepath.Clean(spec.ActivityRepo) != spec.ActivityRepo {
			return finish(-1, errors.New("sandbox activity requires a clean absolute canonical project repo"))
		}
		activity := forkspace.ExecutionSpec{
			Kind: spec.ActivityKind, Role: spec.ActivityRole, Workspace: spec.Repo,
			Task: spec.ActivityTask, SourceID: spec.ActivitySource,
			ReservationOwner: spec.ActivityReservationOwner,
		}
		if spec.ForkName != "" {
			activity.Fork = &forkspace.Identity{Name: spec.ForkName, Generation: forkspace.Generation(spec.ForkGeneration)}
		}
		var err error
		execution, err = forkspace.BeginExecution(spec.ActivityRepo, activity)
		if err != nil {
			return finish(-1, fmt.Errorf("publish sandbox activity: %w", err))
		}
		spec.activityID = execution.ID
	}
	if filtered != nil {
		options := assembleOptions(cfg, rt.SupportsInit(), spec, mounts, decoy.Name(), decoyDir, workdir, mode, rawMCP,
			mcpMounts, consultMounts, gitMounts, instructionMounts, synthMounts, "", envFile, boxLimits(cfg, rt)...)
		options, capturedEnv, err := captureRequiredMCPEnv(options, requiredMCPEnv, artifacts)
		if err != nil {
			return finish(-1, err)
		}
		if capturedEnv != "" {
			tmpFiles = append(tmpFiles, capturedEnv)
		}
		generated := append([]string{decoy.Name()}, tmpFiles...)
		for _, mount := range synthMounts {
			generated = append(generated, mount.host) // includes exact fallback-file leaves
		}
		if err := filtered.validateMounts(options, generated, append([]string{decoyDir}, tmpDirs...)); err != nil {
			return finish(-1, err)
		}
		sections.starting()
		code, launchErr := filtered.launch(spec.Ctx, spec, options, stdin, stdout, stderr)
		if started = filtered.started(); started {
			// The reason is only knowable here; the sentence it feeds prints from the teardown
			// above, once the box this process owns is actually gone.
			stopped = stopReason(code, launchErr, interrupt)
		}
		return finish(code, launchErr)
	}
	// Bring sibling services up first, so the box can reach them by name. Every launch path —
	// agent, ACP, loop, and fork — funnels through box.Run, so this one call covers
	// them all. Gated like the network join below (on the services net, online, compose-capable
	// runtime) plus COOP_AUTO_UP. Idempotent; progress goes to stderr (never stdout, which may
	// carry ACP/JSON) and only when not Quiet; a failure warns but never blocks the session.
	var servicePorts []ServicePort
	serviceCtx := spec.Ctx
	if serviceCtx == nil {
		serviceCtx = context.Background()
	}
	services := serviceLaunchOutcome{state: servicesNotConfigured}
	if composeFile != "" {
		services.state = servicesUnknown
	}
	serviceNetwork := cfg.ServicesNet
	if serviceNetwork == "" || serviceOwner != "" || spec.Review && composeFile != "" {
		serviceNetwork = ComposeProjectFor(spec.Repo, serviceOwner) + "_default"
	}
	servicesInspected := false
	if composeFile != "" && autoUpServices(cfg, spec, rt.Name) {
		projectRepo := spec.ActivityRepo
		if projectRepo == "" {
			projectRepo = spec.Repo
		}
		if cf := composeFile; cf != "" {
			sections.servicesPreparing()
			if !sections.on && !spec.Quiet {
				ui.Note("starting sibling services (%s)", filepath.Base(cf))
			}
			// Discard compose's own progress UI — it repaints with carriage returns and would overprint
			// the loop's live bar. coop's status line says what happened; `coop up` shows the live
			// output (and the real error) when you need to diagnose a failure. EnsureServices validates
			// the file first, so a refusal (an unsafe compose an agent wrote) surfaces here without
			// running anything host-dangerous. Loop launches stop; direct launches keep their existing
			// continue-without-services behavior.
			var composeStderr bytes.Buffer
			servicesInspected = true
			unlock, lockErr := forkspace.LockServiceLaunch(serviceCtx, projectRepo, true)
			if lockErr != nil {
				return finish(-1, fmt.Errorf("wait for a safe service launch: %w", lockErr))
			}
			started, err := startServicesFileContext(serviceCtx, rt, spec.Repo, cf, serviceOwner, serviceNetwork, io.Discard, &composeStderr, spec.RepoReadOnly, !sections.loop, false, nil, privateRoots...)
			unlock()
			sections.serviceSecrets(started.hidden, cf)
			servicePorts = started.ports
			if err != nil {
				var refused *ComposeRefused
				if sections.loop && errors.As(err, &refused) {
					sections.servicesRefused(err.Error())
					return finish(-1, ui.Reported(err))
				}
				if spec.Review {
					detail := strings.TrimSpace(composeStderr.String())
					if detail != "" {
						err = fmt.Errorf("%w: %s", err, detail)
					}
					return finish(-1, fmt.Errorf("start review services: %w", err))
				}
				sections.servicesFailed(boundedCause(composeStderr.String(), err), started.ports...)
				if !sections.on {
					if len(started.ports) > 0 {
						ui.Note("services: %v — continuing with observed service(s) %s (run 'coop up' to retry)", err, strings.Join(servicePortNames(started.ports), ", "))
					} else {
						ui.Note("services: %v — continuing without them (run 'coop up' to retry)", err)
					}
				}
				services = serviceLaunchOutcome{state: servicesFailed, err: err}
			} else {
				sections.services(started.names)
				services = serviceLaunchOutcome{state: servicesRunning}
			}
		}
	}

	if err := ctxStep(spec.Ctx, "network inspection"); err != nil {
		return finish(-1, err)
	}
	networkName := ""
	if cfg.Egress == "open" && spec.Network && rt.Silent("network", "inspect", serviceNetwork) {
		networkName = serviceNetwork
	}

	// Sidecar discovery + same-URL forwarders: whenever the box will join the services network,
	// tell it each expose'd sidecar's stable per-workspace host URL (COOP_SERVICE_<NAME>_URL) and
	// hand coop-entry the mappings (COOP_FORWARD) so localhost:<hostport> reaches the sidecar from
	// inside the box identically to the host browser. Gated like the network join, not on auto-up —
	// the services may already be running from `coop up`. A skipped start gets a fresh bounded
	// ownership/state/health/binding observation; config alone never becomes an advertised URL.
	if networkName != "" && rt.Name != "container" {
		if cf := composeFile; cf != "" {
			if !servicesInspected {
				var observeErr error
				servicePorts, observeErr = discoverObservedServicePorts(serviceCtx, rt, spec.Repo, cf, serviceOwner, serviceNetwork, spec.RepoReadOnly, privateRoots...)
				if observeErr != nil {
					services.err = errors.Join(services.err, observeErr)
				}
				if len(servicePorts) > 0 {
					services.state = servicesObserved
					sections.servicesObserved(servicePorts)
				}
			}
			if svc := servicePorts; len(svc) > 0 {
				spec.ExtraArgs = append(spec.ExtraArgs, "-e", "COOP_FORWARD="+forwardEnv(svc))
				for _, p := range svc {
					spec.ExtraArgs = append(spec.ExtraArgs, "-e",
						fmt.Sprintf("COOP_SERVICE_%s_URL=%s://localhost:%d", serviceEnvName(p.Service), p.Scheme, p.HostPort))
				}
			}
		}
	}

	// Decide each serve port ONCE, so the note below and the publish arguments agree, then tell the
	// agent what this box actually got. The instruction files exist but are not mounted yet; the
	// sidecars simply start after they are assembled, so their facts are appended here.
	joined := networkName != "" && rt.Name != "container"
	spec.servePlan = requestedServePublicationPlan(cfg, spec, hostPortFree)
	if err := appendInstructionNote(instructionMounts, servicesNote(services, servicePorts, joined, spec.servePlan)); err != nil {
		return finish(-1, err)
	}

	// networkName is the exact service network already inspected above. An unavailable network
	// withholds both the join and every in-box service URL/forwarder derived from it.

	if err := ctxStep(spec.Ctx, "argument assembly"); err != nil {
		return finish(-1, err)
	}
	limits := boxLimits(cfg, rt)
	args := assembleArgs(cfg, rt.SupportsInit(), spec, mounts, decoy.Name(), decoyDir, workdir, mode, rawMCP, mcpMounts, consultMounts, gitMounts, instructionMounts, synthMounts, networkName, envFile, limits...)
	optionEnd := len(args) - len(spec.Cmd) - 1
	options, capturedEnv, err := captureRequiredMCPEnv(args[:optionEnd], requiredMCPEnv, artifacts)
	if err != nil {
		return finish(-1, err)
	}
	if capturedEnv != "" {
		tmpFiles = append(tmpFiles, capturedEnv)
	}
	args = append(options, args[optionEnd:]...)
	activityRepo := spec.ActivityRepo
	if activityRepo == "" {
		activityRepo = spec.Repo
	}
	unlockMounts, err := forkspace.LockServiceLaunch(spec.Ctx, activityRepo, false)
	if err != nil {
		return finish(-1, fmt.Errorf("enter the sandbox mount window: %w", err))
	}
	defer unlockMounts()
	// The launch boundary: everything above is host work, everything below is the provider's.
	// A caller that clocks the provider starts counting HERE, never from Run's entry — an early
	// return above launched nothing, so it signals nothing.
	if spec.OnRuntimeLaunch != nil {
		spec.OnRuntimeLaunch()
	}
	sections.starting()
	if spec.Ctx != nil {
		code, runErr := rt.RunInterruptible(spec.Ctx, stdin, stdout, stderr, args...)
		if spec.Ctx.Err() == nil || spec.RunID == "" {
			return finish(code, runErr)
		}
		// Killing a Docker client does not guarantee its daemon-owned container exits.
		// The loop run id owns one box at a time, so remove that exact canceled box as a backstop.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, cleanupErr := rt.RemoveByLabel(cleanupCtx, LabelRun, spec.RunID)
		return finish(code, errors.Join(runErr, cleanupErr))
	}
	code, runErr := rt.Run(stdin, stdout, stderr, args...)
	// The plain client ran the box to its end, so its exit status is the main process's. A client
	// that could not start is the one case with no box to stop; the deferred narration above
	// renders that failure.
	// The plain client ran the box to its end and removed it (--rm), so the stop is confirmed the
	// moment it returns: there is no teardown of coop's own left to wait for.
	if started = runErr == nil; started {
		sections.stopped(stopReason(code, nil, nil))
	}
	return finish(code, runErr)
}

// prepareBoxEnvFile picks the env file a run passes to the runtime. Credential scope: the shared
// env file is passed in, but a scoped run strips token keys for agents it has no business reading
// and for a provider whose selected named account must not be shadowed by its global env token.
// Non-agent runtime vars always pass through; assembleArgs computes the same provider scope for
// the home mounts. tmp is the generated file the caller removes after the run ("" when the shared
// file is used as is, or none applies).
func prepareBoxEnvFile(cfg *config.Config, spec RunSpec, artifacts compositionArtifactOps, projectEnv map[string]string) (envFile, tmp string, err error) {
	return prepareBoxEnvFileWithMarkers(cfg, spec, artifacts, projectEnv, nil)
}

func prepareBoxEnvFileWithMarkers(cfg *config.Config, spec RunSpec, artifacts compositionArtifactOps, projectEnv map[string]string, markers map[string]bool) (envFile, tmp string, err error) {
	userEnvFile := ""
	drop := envKeysOutsideScopeWithMarkers(cfg, credentialScope(cfg, spec), markers)
	userEnv := map[string]string{}
	if spec.Homes && fileExists(cfg.EnvFile()) {
		userEnvFile = cfg.EnvFile()
		userEnv = EnvFileValues(userEnvFile)
	}
	if spec.Login {
		for _, name := range agents.Names() {
			if agent, ok := agents.Get(name); ok {
				for _, key := range agent.CredentialEnvKeys() {
					drop[key] = true
				}
			}
		}
	}
	profileEnv, err := scopedHostCredentialEnv(cfg, spec, userEnv)
	if err != nil {
		return "", "", err
	}
	switch {
	case len(projectEnv) > 0 || len(profileEnv) > 0:
		p, err := writeComposedEnvFile(artifacts.parent, projectEnv, profileEnv, userEnvFile, drop)
		if err != nil {
			return "", "", fmt.Errorf("prepare project box env: %w", err)
		}
		return p, p, nil
	case userEnvFile != "" && len(drop) == 0:
		return userEnvFile, "", nil
	case userEnvFile != "":
		p, err := writeFilteredEnvFile(artifacts.parent, userEnvFile, drop)
		if err != nil {
			// Fail closed: if the peer keys can't be stripped, omit the env file
			// entirely rather than leak them into a scoped box.
			// Values are never echoed: the cause names the failure, not the keys it was filtering.
			ui.Warning("The project environment was not loaded", err.Error(), "")
			return "", "", nil
		}
		return p, p, nil
	}
	return "", "", nil
}

// validateMCPSourceIsolation rejects a configured source that traverses any directory this run
// mounts wholesale, then returns the resolved source the caller must snapshot. Exposing the mutable
// or not-yet-created authority through a repository, companion, or credential home would defeat
// that boundary; reopening the original spelling would reintroduce a parent-symlink race.
func validateMCPSourceIsolation(cfg *config.Config, spec RunSpec) (string, error) {
	roots := []MCPSourceRoot{{Kind: "mounted repository", Path: spec.Repo}}
	for _, companion := range spec.CompanionRepositories {
		roots = append(roots, MCPSourceRoot{Kind: "mounted companion repository", Path: companion.HostPath})
	}
	for _, name := range credentialScope(cfg, spec) {
		roots = append(roots, MCPSourceRoot{Kind: "mounted " + name + " credential home", Path: cfg.AgentDir(name)})
	}
	if spec.ShareACPSessions {
		if primary := runPrimary(spec); primary != "" {
			roots = append(roots, MCPSourceRoot{Kind: "mounted " + primary + " ACP session store", Path: acpSharedDir(cfg, primary)})
		}
	}
	return ResolveMCPSource(cfg.MCPFile, roots)
}

// MCPSourceRoot is a directory exposed wholesale to a less-trusted process. ResolveMCPSource rejects
// a source that starts in or traverses one of these roots, including through symlink and case
// aliases, so the authority can be neither changed nor disclosed through that exposure.
type MCPSourceRoot struct {
	Kind string
	Path string
}

// ResolveMCPSource proves that source does not overlap any exposed root, then returns the canonical
// endpoint that the caller must pass to mcp.ReadValidatedSnapshot. Reopening source itself would
// reintroduce the parent-symlink race this resolution closes.
func ResolveMCPSource(sourcePath string, roots []MCPSourceRoot) (string, error) {
	source, err := resolveHostFileSource(sourcePath, "mcp.json", roots)
	if source.overlap != nil {
		return "", fmt.Errorf("mcp.json source %q is inside %s %q; move the file and set COOP_MCP_FILE to a path outside directories exposed to agents", sourcePath, source.overlap.Kind, source.overlap.Path)
	}
	if err != nil {
		return "", err
	}
	return source.path, nil
}

// resolvePathTrace walks one absolute Unix path component by component and records each entry
// before following it. EvalSymlinks exposes only the endpoint; that loses an intermediate symlink
// such as /repo/bridge -> /outside, even though a boxed agent can retarget the bridge. The trace is
// therefore part of the containment proof, and the endpoint from this same walk is the only path
// the caller may reopen.
func resolvePathTrace(value string) (string, []string, error) {
	if !filepath.IsAbs(value) {
		return "", nil, fmt.Errorf("path %q is not absolute", value)
	}
	components := pathComponents(value)
	resolved := filepath.VolumeName(value) + string(filepath.Separator)
	traversed := make([]string, 0, len(components))
	links := 0
	for len(components) > 0 {
		component := components[0]
		components = components[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			traversed = append(traversed, resolved)
			continue
		}

		candidate := filepath.Join(resolved, component)
		traversed = append(traversed, candidate)
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			for _, remaining := range components {
				if remaining == ".." {
					return "", nil, fmt.Errorf("missing path %q is followed by a parent component", candidate)
				}
				if remaining == "" || remaining == "." {
					continue
				}
				candidate = filepath.Join(candidate, remaining)
				traversed = append(traversed, candidate)
			}
			return candidate, traversed, nil
		}
		if err != nil {
			return "", nil, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			if len(components) > 0 && !info.IsDir() {
				return "", nil, fmt.Errorf("path component %q is not a directory", candidate)
			}
			resolved = candidate
			continue
		}

		links++
		if links > 255 {
			return "", nil, errors.New("too many symbolic links")
		}
		target, err := os.Readlink(candidate)
		if err != nil {
			return "", nil, err
		}
		if filepath.IsAbs(target) {
			resolved = filepath.VolumeName(target) + string(filepath.Separator)
		} else {
			resolved = filepath.Dir(candidate)
		}
		components = append(pathComponents(target), components...)
	}
	return filepath.Clean(resolved), traversed, nil
}

func pathComponents(value string) []string {
	volume := filepath.VolumeName(value)
	return strings.FieldsFunc(value[len(volume):], func(r rune) bool {
		return r == filepath.Separator
	})
}

// futurePathWithin uses inode identity whenever the mounted root exists, so alternate casing on a
// case-insensitive filesystem cannot evade containment. A future root has no inode yet; compare its
// cleaned components case-insensitively, which is conservative on case-sensitive filesystems.
func futurePathWithin(root, candidate string) (bool, error) {
	rootInfo, err := os.Stat(root)
	if errors.Is(err, syscall.ENOTDIR) {
		return false, nil // a non-directory root ancestor cannot expose a subtree
	}
	if errors.Is(err, os.ErrNotExist) {
		return pathComponentPrefix(root, candidate), nil
	}
	if err != nil {
		return false, err
	}
	probe := candidate
	for {
		info, statErr := os.Stat(probe)
		if statErr == nil {
			for {
				if os.SameFile(rootInfo, info) {
					return true, nil
				}
				parent := filepath.Dir(probe)
				if parent == probe {
					return false, nil
				}
				probe = parent
				info, statErr = os.Stat(probe)
				if statErr != nil {
					return false, statErr
				}
			}
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return false, statErr
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return false, statErr
		}
		probe = parent
	}
}

func pathComponentPrefix(root, candidate string) bool {
	rootVolume := filepath.VolumeName(root)
	candidateVolume := filepath.VolumeName(candidate)
	if !strings.EqualFold(rootVolume, candidateVolume) {
		return false
	}
	components := func(value, volume string) []string {
		value = strings.TrimPrefix(filepath.Clean(value)[len(volume):], string(filepath.Separator))
		if value == "" {
			return nil
		}
		return strings.Split(value, string(filepath.Separator))
	}
	rootParts := components(root, rootVolume)
	candidateParts := components(candidate, candidateVolume)
	if len(candidateParts) < len(rootParts) {
		return false
	}
	for index := range rootParts {
		if !strings.EqualFold(rootParts[index], candidateParts[index]) {
			return false
		}
	}
	return true
}

// MaxCompanionRepositories bounds the companion inventory one box may mount. Each entry becomes a
// bind mount and an env pair on the container argv, so the list is a real resource an untrusted
// caller would otherwise set. An inventory over the bound is REFUSED, never shortened to fit: a box
// silently missing repositories its caller believes it has is the worse failure.
const MaxCompanionRepositories = 64

func companionRepositoryMounts(
	repositories []CompanionRepository,
) ([]Mount, []companionRepositoryEnvironment, error) {
	if len(repositories) > MaxCompanionRepositories {
		return nil, nil, fmt.Errorf(
			"%d companion repositories exceeds the limit of %d",
			len(repositories), MaxCompanionRepositories,
		)
	}
	seen := make(map[string]bool, len(repositories))
	mounts := make([]Mount, 0, len(repositories))
	environment := make([]companionRepositoryEnvironment, 0, len(repositories))
	for _, repository := range repositories {
		if !validCompanionName(repository.Name) || seen[repository.Name] {
			return nil, nil, fmt.Errorf(
				"invalid or duplicate companion repository name %q",
				repository.Name,
			)
		}
		if repository.HostPath == "" || !filepath.IsAbs(repository.HostPath) ||
			filepath.Clean(repository.HostPath) != repository.HostPath {
			return nil, nil, fmt.Errorf(
				"companion repository %q must use an absolute clean host path",
				repository.Name,
			)
		}
		realPath, err := filepath.EvalSymlinks(repository.HostPath)
		if err != nil || realPath != repository.HostPath {
			return nil, nil, fmt.Errorf(
				"companion repository %q must name a real path",
				repository.Name,
			)
		}
		info, err := os.Stat(realPath)
		if err != nil || !info.IsDir() {
			return nil, nil, fmt.Errorf(
				"companion repository %q is not a directory",
				repository.Name,
			)
		}
		if !validCommitIdentity(repository.BaseCommit) {
			return nil, nil, fmt.Errorf(
				"companion repository %q has an invalid commit identity",
				repository.Name,
			)
		}
		seen[repository.Name] = true
		target := path.Join("/coop/repositories", repository.Name)
		mounts = append(mounts, Mount{
			Kind: Bind, Source: realPath, Target: target, RO: true,
		})
		environment = append(environment, companionRepositoryEnvironment{
			Name: repository.Name, Path: target,
			BaseCommit: repository.BaseCommit,
		})
	}
	return mounts, environment, nil
}

func validCompanionName(name string) bool {
	if name == "" || name == "primary" || len(name) > 48 {
		return false
	}
	for index, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			(index > 0 && (r == '-' || r == '_')) {
			continue
		}
		return false
	}
	return true
}

func validCommitIdentity(commit string) bool {
	if len(commit) != 40 && len(commit) != 64 {
		return false
	}
	for _, r := range commit {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func repoReadOnlyPathMounts(repo, workdir string, paths []string) ([]Mount, error) {
	repoAbs, err := filepath.Abs(repo)
	if err != nil {
		return nil, fmt.Errorf("resolve repository for read-only paths: %w", err)
	}
	repoResolved, err := filepath.EvalSymlinks(repoAbs)
	if err != nil {
		return nil, fmt.Errorf("resolve repository for read-only paths: %w", err)
	}
	seen := map[string]bool{}
	var mounts []Mount
	for _, path := range paths {
		pathAbs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve read-only repository path %q: %w", path, err)
		}
		rel, err := filepath.Rel(repoAbs, pathAbs)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("read-only repository path %q is not a real descendant of %q", path, repo)
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		current := repoAbs
		for _, component := range strings.Split(rel, string(filepath.Separator)) {
			current = filepath.Join(current, component)
			info, err := os.Lstat(current)
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf(
					"read-only repository path %q does not exist; create the configured task queue before a repository-writable review",
					path,
				)
			}
			if err != nil {
				return nil, fmt.Errorf("inspect read-only repository path %q: %w", path, err)
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("read-only repository path %q crosses a non-directory or symlink", path)
			}
		}
		target := filepath.Join(workdir, rel)
		resolved, err := filepath.EvalSymlinks(pathAbs)
		if err != nil {
			return nil, fmt.Errorf("resolve read-only repository path %q: %w", path, err)
		}
		resolvedRel, err := filepath.Rel(repoResolved, resolved)
		if err != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) ||
			filepath.Clean(filepath.Join(repoResolved, rel)) != resolved {
			return nil, fmt.Errorf("read-only repository path %q escapes through a symlink", path)
		}
		mounts = append(mounts, Mount{
			Kind: Bind, Source: pathAbs, Target: target, RO: true,
		})
	}
	return mounts, nil
}

func projectPolicyRepo(spec RunSpec) string {
	if spec.PolicyRepo != "" {
		return spec.PolicyRepo
	}
	return spec.Repo
}

// presetRoleMounts wires a preset's roles into the box and returns the mounts to add, the -e args
// to append to spec.ExtraArgs, and the temp files/dirs to clean up: the coop-delegate wrapper plus
// each delegate role's contract and COOP_DELEGATE_<ROLE>_* env; generated coop-<role> native
// subagents under a capable lead (mounted at the adapter-owned user-level destination); and each
// consult-wired role's persona + COOP_CONSULT_<ROLE>_* env (explicit consult roles plus natives
// degraded under an incapable lead). A run with no preset (or homes off) returns all-nil.
func presetRoleMounts(cfg *config.Config, spec RunSpec, artifacts compositionArtifactOps) (mounts []extraMount, extraArgs, tmpFiles, tmpDirs []string, err error) {
	defer func() {
		if err == nil {
			return
		}
		for _, path := range tmpFiles {
			_ = os.Remove(path)
		}
		for _, path := range tmpDirs {
			_ = os.RemoveAll(path)
		}
	}()
	if !spec.Homes || spec.Preset == nil {
		return
	}
	// The loop's run id, so a consult/delegate peer can append its token usage to
	// .agent/runs/<id>.peers.jsonl for the closing cost digest. Empty outside a loop.
	if spec.RunID != "" {
		extraArgs = append(extraArgs, "-e", "COOP_RUN_ID="+spec.RunID)
	}
	// coop-delegate: a preset with a write-capable delegate role mounts the wrapper (on
	// PATH), one generated contract file per delegate role, and the COOP_DELEGATE_<ROLE>_*
	// env the wrapper resolves a role's target ladder/contract from — so the box needs no
	// YAML parser and the wrapper enforces commit:never / concurrent:never itself.
	delegates := spec.Preset.Delegates()
	if len(delegates) > 0 {
		p, writeErr := artifacts.writeFile(artifacts.parent, preset.DelegateWrapper())
		if writeErr != nil {
			err = fmt.Errorf("assemble delegate wrapper: %w", writeErr)
			return
		}
		tmpFiles = append(tmpFiles, p)
		if chmodErr := artifacts.chmod(p, 0o755); chmodErr != nil {
			err = fmt.Errorf("make delegate wrapper executable: %w", chmodErr)
			return
		}
		mounts = append(mounts, extraMount{p, preset.DelegateWrapperPath})
		for _, role := range delegates {
			key := preset.EnvKey(role.Name)
			dst := cfg.HomeInBox + "/.coop/delegate/" + role.Name + ".md"
			cp, writeErr := artifacts.writeFile(artifacts.parent, preset.RoleContract(&role))
			if writeErr != nil {
				err = fmt.Errorf("assemble delegate role %q contract: %w", role.Name, writeErr)
				return
			}
			tmpFiles = append(tmpFiles, cp)
			mounts = append(mounts, extraMount{cp, dst})
			extraArgs = append(extraArgs,
				"-e", "COOP_DELEGATE_"+key+"_CONTRACT="+dst,
				"-e", "COOP_DELEGATE_"+key+"_TARGETS="+resolvedRoleTargetList(cfg, &role),
			)
		}
	}

	// Native roles under a capable lead run in-session as generated coop-<role> subagents.
	// Explicit consult roles and natives degraded under another lead are wired role-addressed so
	// `coop-consult <role>` runs the role's agent on its model, with its persona if any.
	lead := spec.ConsultLead
	if ag, ok := agents.Get(lead); ok {
		support := ag.NativeSubagents()
		// The adapter renders its native-role files and owns their in-home destination. They mount
		// from a disposable read-only directory, separate from the repo's own live artifacts.
		if gen := generatedSubagentFiles(spec.Preset, lead, support); len(gen) > 0 {
			dir, assembleErr := artifacts.assembleAgentsDir(artifacts.parent, gen)
			if assembleErr != nil {
				err = fmt.Errorf("assemble native roles for %s: %w", lead, assembleErr)
				return
			}
			tmpDirs = append(tmpDirs, dir)
			dst := strings.TrimRight(cfg.HomeInBox, "/") + "/" + strings.Trim(support.HomeDir, "/")
			mounts = append(mounts, extraMount{dir, dst})
		}
	}
	// Consult-wired roles: persona at ~/.coop/consult/<role>.md + the COOP_CONSULT_<ROLE>_*
	// env the wrapper resolves agent/model/persona from. coop-consult itself is mounted by Run
	// (a preset with consult-wired roles is consult-wired — leadInstructionMount); the roles'
	// agents join the credential scope (credentialScope).
	for _, role := range spec.Preset.ConsultRoles(lead) {
		key := preset.EnvKey(role.Name)
		if body := preset.ConsultBody(&role); body != "" {
			dst := cfg.HomeInBox + "/.coop/consult/" + role.Name + ".md"
			cp, writeErr := artifacts.writeFile(artifacts.parent, body)
			if writeErr != nil {
				err = fmt.Errorf("assemble consult role %q persona: %w", role.Name, writeErr)
				return
			}
			tmpFiles = append(tmpFiles, cp)
			mounts = append(mounts, extraMount{cp, dst})
			extraArgs = append(extraArgs, "-e", "COOP_CONSULT_"+key+"_CONTRACT="+dst)
		}
		extraArgs = append(extraArgs, "-e", "COOP_CONSULT_"+key+"_TARGETS="+resolvedRoleTargetList(cfg, &role))
	}
	return
}

// resolvedRoleTargetList materializes each blank role model/effort from that provider's run
// config before exporting the wrapper ladder. Provider-scoped COOP_PEER_MODEL_* may carry an
// ad-hoc peer's explicit override; a blank role target must not inherit that unrelated pin.
func resolvedRoleTargetList(cfg *config.Config, role *preset.Role) string {
	targets := role.Targets
	parts := make([]string, len(targets))
	for i, target := range targets {
		if target.Model == "" {
			target.Model = cfg.ModelFor(target.Provider)
		}
		if target.Effort == "" {
			target.Effort = cfg.EffortFor(target.Provider)
		}
		parts[i] = target.String()
	}
	return strings.Join(parts, " ")
}

// ensureAgentHomes pre-creates the credential-home dir for exactly
// the agents this run MOUNTS (credentialScope: the launched agent plus named consult peers)
// — not every agent. Pre-creating all three was a husk factory: every box
// run materialized each agent's active-profile dir, so a profile the user deleted (an empty
// "default" showing "not signed in" in `coop credentials`) kept reappearing, seeded with
// EnsureDefaults' settings files, recreated by runs that never involved that agent. An
// out-of-scope agent has no home mounted, so nothing in the box reads the dir anyway.
// Provider defaults remain provider-owned and are written only after every required input has
// validated. Their read/parse/write errors stop launch rather than treating broken user state as
// empty. Host-owned ancestor permissions are mandatory and are established before any credential
// read, mount, or runtime access.
func ensureAgentHomes(cfg *config.Config, spec RunSpec) error {
	for _, name := range credentialScope(cfg, spec) {
		if err := EnsureProfilesDir(cfg, name); err != nil {
			return fmt.Errorf("prepare %s credential root: %w", name, err)
		}
		profileRoot := filepath.Join(cfg.ConfigDir, name, "profiles")
		profileDir := cfg.AgentDir(name)
		if filepath.Dir(profileDir) != profileRoot {
			return fmt.Errorf("prepare %s credential: selected name does not resolve inside %s", name, profileRoot)
		}
		if err := config.EnsurePrivateDir(profileDir); err != nil {
			return fmt.Errorf("prepare %s credential: %w", name, err)
		}
	}
	return nil
}

func ensureAgentDefaults(cfg *config.Config, spec RunSpec, workdir string) error {
	for _, name := range credentialScope(cfg, spec) {
		if ag, ok := agents.Get(name); ok {
			if err := ag.EnsureDefaults(cfg, workdir); err != nil {
				return fmt.Errorf("prepare %s defaults: %w", name, err)
			}
		}
	}
	return nil
}

// resolveWorkdir picks where the repo mounts inside the box — and thus the
// agent's cwd. The default is the repo's real host path, so each agent's
// per-project session history (~/.<agent>/projects/<cwd>) is identical across
// `coop`, `coop loop`, and `coop acp`; a loop's thread is then visible and
// resumable when you open the same repo in an ACP editor like Zed. An explicit
// spec.Workdir (doctor's self-contained fixture) or COOP_WORKDIR (cfg.Workdir)
// overrides it, in that order.
func resolveWorkdir(spec RunSpec, cfg *config.Config) string {
	if spec.Workdir != "" {
		return spec.Workdir
	}
	return Workdir(cfg, spec.Repo)
}

// Workdir reports where a repo mounts inside the box for a normal run (the agent's cwd):
// the COOP_WORKDIR override if set, else the repo's own host path. It is the single source
// of truth for that decision so callers outside box — the loop's stream decoder, which
// shows tool-call paths relative to this root — stay in step with the real mount. The
// doctor's spec.Workdir fixture override isn't a normal-run concern, so it's not reflected.
func Workdir(cfg *config.Config, repo string) string {
	if cfg != nil && cfg.Workdir != "" {
		return cfg.Workdir
	}
	return repo
}

// boxEnvNote is the always-present environment briefing every agent receives up front, so it
// doesn't burn a turn rediscovering the box (the user's INSTRUCTIONS.md, if any, follows it).
// It states the ground truth the agents most often probe or trip over: the missing OS sandbox,
// what's installed (now that the image carries bare python/pip), where it may write, and that
// secrets are read-only decoys.
const boxEnvNote = `# Environment (coop box) — ground truth, don't reprobe it
You run inside a coop container: a Debian box that IS your sandbox and security boundary.
- OS-level sandboxing (bubblewrap) is intentionally absent. A "bubblewrap is required" notice
  is expected, not a bug — don't investigate or work around it, just proceed.
- Installed and ready: node, npm, yarn, python (= python3), pip, git, gcc/make, jq, rg, fd,
  curl, wget, perl, psql. Other toolchains (go, ruby, erlang, …) exist only if the repo pins
  them in .tool-versions, which is provisioned automatically on start.
- Playwright's Chromium system libraries are preinstalled. The browser binary downloads on
  first use (cached in ~/.cache, so once per machine): run "npx playwright install chromium"
  if it's missing. Launch headless and pass args: ['--no-sandbox'] — Chromium's own sandbox
  can't run here (the box already is the sandbox), so without it the launch fails.
- Write inside the repo (your working directory) — that's where your changes belong and your
  file-write tools work. Paths outside the repo may be refused; for scratch, write in-repo or
  use a shell command.
- Files that look like secrets (.env*, *.key, *.pem, id_rsa*, .ssh, …) are shadowed with empty
  read-only decoys. You can't read or write them, by design — don't try to bypass it. A shadowed
  path that is TRACKED therefore shows as modified in git status — that is the decoy, not your
  work. Never stage, commit, check out, or git restore those paths: it would replace real
  repository content with an empty file. Leave them untouched and unstaged, and don't reach for a
  blanket "git add -A" / "git commit -a" that would sweep them in.
- Sibling services (.agent/compose.yml) get those same decoys for any secret-looking file they
  bind. A service that dies reading a key or certificate it mounts from the repo ("missing BEGIN
  PRIVATE KEY" and the like) is hitting the decoy, not a broken file: say so and ask the human to
  run "coop up" outside the box and approve that file — no change you make in here can grant it.
`

// agentBaseInstructions is what an agent receives as its global instructions: the always-on
// box environment note, followed by the user's instructions — a per-agent override if present,
// else the shared INSTRUCTIONS.md. Consult and preset routing augment this; they do not replace it.
func agentBaseInstructions(cfg *config.Config, agent, file, network string) (string, error) {
	user := ""
	data, present, err := readOptionalRegularFile(filepath.Join(cfg.AgentDir(agent), file))
	if err != nil {
		return "", fmt.Errorf("read %s instructions: %w", agent, err)
	}
	if present {
		user = string(data)
	} else {
		data, present, err = readOptionalRegularFile(cfg.Instructions())
		if err != nil {
			return "", fmt.Errorf("read shared instructions: %w", err)
		}
		if present {
			user = string(data)
		}
	}
	if strings.TrimSpace(user) == "" {
		return boxEnvNote + network, nil
	}
	return boxEnvNote + network + "\n" + user, nil
}

// readOptionalRegularFile distinguishes a missing override from a present file Coop could not
// read. Symlinks to regular files retain their existing behavior; dangling links and special files
// are errors rather than silently erasing selected-provider instructions.
func readOptionalRegularFile(path string) ([]byte, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, linkErr := os.Lstat(path); errors.Is(linkErr, os.ErrNotExist) {
				return nil, false, nil
			} else if linkErr != nil {
				return nil, false, linkErr
			}
		}
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// instructionItem is one agent's global instruction file and the content it should hold.
type instructionItem struct{ agent, file, content string }

// synthSkillsMounts returns the user-level ~/.<agent>/skills mounts to synthesize from the repo's
// shared skills source — .agent/skills, or an established .claude/skills fallback — one per
// skills-capable agent whose repo has NO per-agent skills dir of its own. Each mount is a WRITABLE
// COPY, not a read-only bind of the host dir:
// some CLIs (codex) install their own system skills INTO the skills dir, which a :ro mount breaks —
// and the copy keeps the host's source pristine. The copies die with the box.
func synthSkillsMounts(repo, homeInBox string, agentNames []string, exposedRoots ...string) (mounts []extraMount, tmpdirs []string, retErr error) {
	sources, err := openRepositorySources(repo)
	if err != nil {
		return nil, nil, err
	}
	defer sources.root.Close()
	seen := map[string]bool{}
	var selected []string
	for _, ag := range agentNames {
		adapter, ok := agents.Get(ag)
		if seen[ag] || !ok || !adapter.SkillsCapable() {
			continue
		}
		seen[ag] = true
		present, err := sources.exists(filepath.Join("."+ag, "skills"), true)
		if err != nil {
			return nil, nil, fmt.Errorf("inspect project skills for %s: %w", ag, err)
		}
		if present {
			continue // the repo's own skills dir wins — synthesize nothing
		}
		selected = append(selected, ag)
	}
	if len(selected) == 0 {
		return nil, nil, nil
	}

	src := filepath.Join(".agent", "skills")
	present, err := sources.exists(src, true)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect shared skills source: %w", err)
	}
	if !present {
		// The old .claude source is only a compatibility fallback. Invalid links and other
		// unusable legacy shapes remain equivalent to absence.
		for _, candidate := range agents.EstablishedSkillsSources() {
			if info, legacyErr := sources.root.Lstat(candidate); legacyErr == nil && info.IsDir() {
				src, present = candidate, true
				break
			}
		}
		if !present {
			return nil, nil, nil
		}
	}
	source, err := sources.openTree(src)
	if err != nil {
		return nil, nil, fmt.Errorf("open shared skills source: %w", err)
	}
	defer source.Close()
	var preparedDirs []string
	defer func() {
		if retErr != nil {
			for _, dir := range preparedDirs {
				_ = os.RemoveAll(dir)
			}
			mounts, tmpdirs = nil, nil
		}
	}()
	for _, ag := range selected {
		dst, err := privateWorkspaceTempDir(repo, "coop-skills-"+ag+"-", exposedRoots...)
		if err != nil {
			return nil, nil, fmt.Errorf("prepare skills for %s: %w", ag, err)
		}
		if err := copySourceTree(dst, source); err != nil {
			_ = os.RemoveAll(dst)
			return nil, nil, fmt.Errorf("copy skills for %s from %s: %w", ag, src, err)
		}
		preparedDirs = append(preparedDirs, dst)
		mounts = append(mounts, extraMount{dst, homeInBox + "/." + ag + "/skills"})
	}
	return mounts, preparedDirs, nil
}

// synthHomeFallbackMounts copies each active adapter's declared fallback artifacts into
// ephemeral user-level mounts. Project artifacts suppress matching fallbacks independently;
// writable copies keep both the committed source and host credential profile untouched.
func synthHomeFallbackMounts(repo, homeInBox string, agentNames []string, exposedRoots ...string) (mounts []extraMount, tmpdirs []string, retErr error) {
	sources, err := openRepositorySources(repo)
	if err != nil {
		return nil, nil, err
	}
	defer sources.root.Close()
	var preparedDirs []string
	defer func() {
		if retErr != nil {
			for _, dir := range preparedDirs {
				_ = os.RemoveAll(dir)
			}
			mounts, tmpdirs = nil, nil
		}
	}()
	seen := map[string]bool{}
	for _, name := range agentNames {
		ag, ok := agents.Get(name)
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		for _, artifact := range ag.HomeFallbacks() {
			source := filepath.FromSlash(artifact.Source)
			projectArtifact := filepath.FromSlash(artifact.Project)
			projectPresent, err := sources.exists(projectArtifact, artifact.Dir)
			if err != nil {
				return nil, nil, fmt.Errorf("inspect project %s artifact for %s: %w", artifact.Project, name, err)
			}
			if projectPresent {
				continue
			}
			sourcePresent, err := sources.exists(source, artifact.Dir)
			if err != nil {
				return nil, nil, fmt.Errorf("inspect fallback %s for %s: %w", artifact.Source, name, err)
			}
			if !sourcePresent {
				continue
			}

			dst, err := privateWorkspaceTempDir(repo, "coop-home-"+name+"-", exposedRoots...)
			if err != nil {
				return nil, nil, fmt.Errorf("prepare fallback %s for %s: %w", artifact.Source, name, err)
			}
			host := dst
			if artifact.Dir {
				var tree *os.Root
				tree, err = sources.openTree(source)
				if err == nil {
					err = copySourceTree(dst, tree)
					_ = tree.Close()
				}
			} else {
				host = filepath.Join(dst, filepath.Base(source))
				var data []byte
				data, err = sources.readFile(source)
				if err == nil {
					err = os.WriteFile(host, data, 0o600)
				}
			}
			if err != nil {
				_ = os.RemoveAll(dst)
				return nil, nil, fmt.Errorf("copy fallback %s for %s: %w", artifact.Source, name, err)
			}
			preparedDirs = append(preparedDirs, dst)
			mounts = append(mounts, extraMount{host, filepath.Join(homeInBox, filepath.FromSlash(artifact.Target))})
		}
	}
	return mounts, preparedDirs, nil
}

// instructionPlan is the global instruction each non-lead agent should receive: the box env
// note plus the user's instructions (per agentBaseInstructions). The consult lead is excluded —
// it gets its augmented file instead. Pure (no temp files / mounts),
// so the selection and content are unit-testable; Run writes + mounts the result.
func instructionPlan(cfg *config.Config, spec RunSpec, network string) ([]instructionItem, error) {
	if !spec.Homes || spec.Login {
		return nil, nil
	}
	var out []instructionItem
	for _, agent := range credentialScope(cfg, spec) {
		if agent == spec.ConsultLead {
			continue
		}
		if file := instructionFile(agent); file != "" {
			content, err := agentBaseInstructions(cfg, agent, file, network)
			if err != nil {
				return nil, err
			}
			out = append(out, instructionItem{agent, file, content})
		}
	}
	return out, nil
}

// genFile is a coop-generated file: its base name and content.
type genFile struct{ name, content string }

// generatedSubagentFiles asks the active lead adapter to render each generated native role.
// Empty means no preset, no native roles, or no adapter capability.
func generatedSubagentFiles(p *preset.Preset, lead string, support agents.NativeSubagentSupport) []genFile {
	if p == nil || support.Render == nil {
		return nil
	}
	var out []genFile
	for _, role := range p.GeneratedNativeRoles(lead) {
		primary := role.Primary()
		fname, content := support.Render(agents.NativeSubagent{
			Name: preset.SubagentName(&role), Description: preset.NativeDescription(&role),
			Model: primary.Model, Effort: primary.Effort, Prompt: preset.NativeBody(&role),
		})
		out = append(out, genFile{fname, content})
	}
	return out
}

// assembleAgentsDir builds a host temp dir holding only adapter-rendered native role files. The
// caller mounts it read-only at the adapter-owned user-level destination and cleans it up.
func assembleAgentsDir(parent string, gen []genFile) (string, error) {
	dir, err := os.MkdirTemp(parent, "coop-agents-")
	if err != nil {
		return "", err
	}
	for _, g := range gen {
		// The name is adapter-rendered data, not a path: anything but a plain local leaf could
		// place a file outside the dir coop mounts. Refuse it rather than clean it into a name
		// that happens to land inside — a rewritten role file is not the role that was asked for.
		if !filepath.IsLocal(g.name) || filepath.Base(g.name) != g.name || g.name == "." {
			os.RemoveAll(dir)
			return "", fmt.Errorf("generated role file %q must be a plain file name", g.name)
		}
		if err := os.WriteFile(filepath.Join(dir, g.name), []byte(g.content), 0o644); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}

// leadInstructionMount builds the instruction file a consult lead receives: its base
// instructions (the box env note + the user's), the preset routing contract when selected,
// and the optional second-opinion directive naming explicit peers. content is ALWAYS at least
// the base — the lead is excluded from instructionPlan (it is meant to get this augmented file
// instead), so returning nothing would leave it running with no instructions at all. wired
// reports whether coop-consult is reachable through either a preset role or an explicit peer.
// ok is false only when the agent has no native instruction file. Pure, so the "no named peer
// still mounts the base" invariant is unit-tested without a container.
func leadInstructionMount(cfg *config.Config, lead string, p *preset.Preset, peers []string, network string) (content, file string, wired, ok bool, err error) {
	file = instructionFile(lead)
	if file == "" {
		return "", "", false, false, nil
	}
	base, err := agentBaseInstructions(cfg, lead, file, network)
	if err != nil {
		return "", "", false, false, err
	}
	peers = excluding(peers, lead)
	if p != nil {
		// The preset names its roles and exact invocations; explicit --peer values remain
		// independent optional second opinions and are appended rather than discarded.
		content = preset.LeadContract(p, lead)
		if tail := consult.LeadInstructions(base, peers); tail != "" {
			content += "\n" + tail + "\n"
		}
		return content, file, len(p.ConsultRoles(lead)) > 0 || len(peers) > 0, true, nil
	}
	return consult.LeadInstructions(base, peers), file, len(peers) > 0, true, nil
}

// decideTTY chooses the stdin/tty wiring. Stdin is attached only for an
// interactive terminal (-it) or ACP (-i); batch and piped runs get neither,
// matching the original tool's behavior.
func decideTTY(spec RunSpec, stdinIsTTY bool) ttyMode {
	switch {
	case spec.ForceNoTTY:
		return ttyStdinOnly
	case spec.Batch:
		return ttyNone
	case stdinIsTTY:
		return ttyInteractive
	default:
		return ttyNone
	}
}

// applyProjectPolicy returns cfg overlaid with the repo's committed .agent/project.yaml box: policy,
// and adjusts spec.Network to match. Each field applies ONLY where the user did not explicitly set
// its COOP_* (env or conf) — an explicit setting always wins. Because egress's built-in default is
// the loosest value ("open"), a committed policy can only ever TIGHTEN it (never widen an explicit
// "none" back to "open"). Works on a COPY of cfg, so the caller's shared Config — including the
// per-run model maps the loop mutates — is never touched (the maps are shared read-through; only the
// scalar box knobs are overwritten on the copy).
func applyProjectPolicy(cfg *config.Config, p *project.Project, spec *RunSpec) *config.Config {
	b := p.Box
	if boxPolicyEmpty(b) {
		return cfg // no box: section — nothing to overlay (gate: is resolved by fork_merge, not here)
	}
	c := *cfg
	if b.Egress != "" && !cfg.Explicit("COOP_EGRESS") {
		c.Egress = b.Egress // validated open|filtered|none by project.Load; AdmitNetwork marks its resolved posture explicit
	}
	if b.AutoUp != nil && !cfg.Explicit("COOP_AUTO_UP") {
		c.AutoUp = *b.AutoUp
	}
	if b.Network != nil && !cfg.Explicit("COOP_NETWORK") {
		spec.Network = *b.Network // the network toggle rides spec, set from cfg.Network by the caller
	}
	if b.Memory != "" && !cfg.Explicit("COOP_MEMORY") {
		c.Memory = b.Memory
	}
	if b.CPUs != "" && !cfg.Explicit("COOP_CPUS") {
		c.CPUs = b.CPUs
	}
	if b.Pids != "" && !cfg.Explicit("COOP_PIDS") {
		c.Pids = b.Pids
	}
	return &c
}

func boxPolicyEmpty(b project.Box) bool {
	return b.Dockerfile == "" && b.Compose == "" && len(b.Env) == 0 && b.Egress == "" &&
		b.AutoUp == nil && b.Network == nil && b.Memory == "" && b.CPUs == "" && b.Pids == ""
}

// boxLimits returns the resource + privilege caps that keep a runaway agent from
// harming the host: a pids cap (fork bombs), optional memory/cpu caps,
// no-new-privileges, and dropping all Linux capabilities. These are Docker's OCI-runtime flags;
// Apple's `container` CLI differs, so they're skipped there (its hardening is
// tracked separately). All are config-driven (COOP_PIDS/MEMORY/CPUS,
// COOP_NO_NEW_PRIVILEGES).
func boxLimits(cfg *config.Config, rt runtime.Runtime) []string {
	if !rt.SupportsRunLimits() {
		return nil
	}
	var a []string
	if cfg.NoNewPrivileges {
		a = append(a, "--security-opt", "no-new-privileges")
	}
	// Drop every Linux capability: the agent workloads (node, npm, asdf, git) need none of
	// Docker's default set, and dropping them tightens the posture for a repo .agent/Dockerfile
	// that runs USER root — root-in-container then holds no CAP_DAC_OVERRIDE / CAP_NET_RAW /
	// CAP_MKNOD / CAP_SYS_CHROOT to abuse. Add one back only if a concrete need appears.
	a = append(a, "--cap-drop", "ALL")
	switch cfg.Pids {
	case "", "0", "unlimited": // pids cap off
	default:
		a = append(a, "--pids-limit", cfg.Pids)
	}
	if cfg.Memory != "" {
		a = append(a, "--memory", cfg.Memory)
	}
	if cfg.CPUs != "" {
		a = append(a, "--cpus", cfg.CPUs)
	}
	return a
}

// appendPublish adds `-p 127.0.0.1:<host>:<container>` for each .agent/project.yaml serve.port, so a
// dev server in the box is reachable from the host browser. The host port is a stable function of the
// repo+port (project.HostPort), so it's the same every launch and distinct per project. Best-effort:
// a host port already in use is skipped (a box that can't publish one port still runs), and publishing
// needs egress open (a --network none box has nothing to bind), else all are skipped. Bound to
// localhost, not 0.0.0.0, so the port isn't exposed to the LAN. Mappings/skips are noted on stderr
// (the ACP server log or the terminal) — never stdout, which on ACP is the JSON-RPC wire.
// free reports whether a host port is bindable (hostPortFree in production), injected so the
// publish decision is unit-tested without claiming a real port.
func appendPublish(args []string, cfg *config.Config, spec RunSpec, free func(int) bool) []string {
	if len(spec.servePorts) == 0 {
		return args
	}
	if cfg.Egress != "open" {
		// A box with no network has nothing to bind; saying so is the whole answer.
		ui.Warning("Project ports were not published", "This run has internet access turned off.", "")
		return args
	}
	// Run decides publication once (the agent's note reads the same plan); a caller that assembled
	// arguments without going through Run decides here. Host ports come from the WORKSPACE path
	// (spec.Repo), not the policy repo: a fork inherits the parent's serve.ports but hashes to its
	// own host ports, so two forks — or a fork and its parent — never collide on one.
	plan := spec.servePlan
	if plan == nil {
		plan = servePublicationPlan(cfg, spec, free)
	}
	var published []servePublication
	for _, s := range plan {
		// The assigned host-facing URL is stable workspace discovery even when another process from
		// this workspace already owns the port. Only the current box's publish mapping is conditional.
		args = append(args, "-e", fmt.Sprintf("COOP_SERVE_URL_%d=http://localhost:%d", s.Port, s.Host))
		if !s.Published {
			// The host port and the box port can differ, so both are named: one is the URL to open,
			// the other is what the dev server inside the box listens on.
			ui.Warning(fmt.Sprintf("Could not publish box port %d", s.Port),
				fmt.Sprintf("Host port %d is already in use.", s.Host),
				"Free that port, then start the box again.")
			continue
		}
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", s.Host, s.Port))
		published = append(published, s)
	}
	if len(published) > 0 {
		ui.Section("Available on this host")
		for _, s := range published {
			ui.Note("  http://localhost:%d → box port %d", s.Host, s.Port)
		}
	}
	return args
}

// hostPortFree reports whether a TCP host port is bindable right now (best-effort; a race with another
// binder is caught by docker at run time, and in ACP the proxy respawns).
func hostPortFree(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// appendROMounts appends a read-only `-v host:box:ro` bind for each mount.
func appendROMounts(args []string, ms []extraMount) []string {
	for _, m := range ms {
		args = append(args, "-v", m.host+":"+m.box+":ro")
	}
	return args
}

// modelEnvArgs exports each scoped agent's resolved model into the box, two ways: the agent's
// own model env var (ModelEnv, e.g. claude's ANTHROPIC_MODEL) so a flagless adapter binary
// (claude-agent-acp) still honors the choice, and — only on a consult-capable run, where the
// coop-consult wrapper exists — COOP_PEER_MODEL_<AGENT>, which the wrapper expands into each
// peer's --model flag. Agents with no resolved model export nothing (the CLI's own default
// runs); the primary agent's command already carries --model, which beats its env var.
func modelEnvArgs(cfg *config.Config, spec RunSpec, scope []string) []string {
	consults := spec.ConsultLead != "" ||
		(spec.Preset != nil && len(spec.Preset.ConsultRoles(runPrimary(spec))) > 0)
	// An explicit peer target's :model pins that peer's model (COOP_PEER_MODEL_<X>); otherwise
	// the peer runs the config default (cfg.ModelFor). The lead isn't in Peers, so it always
	// falls through to cfg.ModelFor.
	peerModel := map[string]string{}
	peerEffort := map[string]string{}
	for _, p := range spec.Peers {
		if p.Model != "" {
			peerModel[p.Provider] = p.Model
		}
		if p.Effort != "" {
			peerEffort[p.Provider] = p.Effort
		}
	}
	var args []string
	for _, agent := range scope {
		ag, ok := agents.Get(agent)
		model := peerModel[agent]
		if model == "" {
			model = cfg.ModelFor(agent)
		}
		if model != "" {
			if ok {
				if env := ag.ModelEnv(); env != "" {
					args = append(args, "-e", env+"="+model)
				}
			}
			if consults {
				args = append(args, "-e", "COOP_PEER_MODEL_"+strings.ToUpper(agent)+"="+model)
			}
		}
		// Effort rides the same way but resolves independently — an agent may carry an effort
		// (env or peer flag) even when it takes the CLI's default model.
		effort := peerEffort[agent]
		if effort == "" {
			effort = cfg.EffortFor(agent)
		}
		if effort != "" {
			if ok {
				if env := ag.EffortEnv(); env != "" {
					args = append(args, "-e", env+"="+effort)
				}
			}
			if consults {
				args = append(args, "-e", "COOP_PEER_EFFORT_"+strings.ToUpper(agent)+"="+effort)
			}
		}
	}
	return args
}

// assembleArgs builds the full container-runtime argument list. It is pure given
// its inputs and the on-disk presence of the env/instruction files, so the whole
// run plan can be unit-tested without a container daemon. limits is the runtime's
// resource/privilege caps (see boxLimits).
// acpSharedDir is the credential-independent session-transcript store for an agent's ACP boxes: a
// mid-session credential/preset switch keeps the conversation because every credential's box mounts
// this same dir over its session store, so session/load still finds the transcript after the switch.
func acpSharedDir(cfg *config.Config, agent string) string {
	return filepath.Join(cfg.ConfigDir, agent, "acp-sessions")
}

func assembleArgs(cfg *config.Config, initProcess bool, spec RunSpec, mounts []Mount, decoy, decoyDir, workdir string, mode ttyMode, rawMCP bool, mcpMounts, consultMounts, gitMounts, instructionMounts, synthMounts []extraMount, networkName, envFile string, limits ...string) []string {
	// Egress fails CLOSED at the box boundary: full/services networking only when COOP_EGRESS is
	// explicitly "open" — any other value (the normalized "none", or a value that somehow skipped
	// config.normalizeEgress) cuts the box off the network entirely (--network none), so a missed
	// normalization can never silently grant outbound. "open" keeps the runtime's bridge (full
	// outbound) plus any services-net join; the agent needs npm/the model API, so it's opt-in.
	// A filtered run never reaches here: its network is the gateway's own namespace.
	network := "none"
	if cfg.Egress == "open" {
		network = networkName // "" → default bridge (full outbound); else the joined services net
	}
	args := append([]string{"run", "--rm"}, assembleOptions(cfg, initProcess, spec, mounts, decoy, decoyDir, workdir,
		mode, rawMCP, mcpMounts, consultMounts, gitMounts, instructionMounts, synthMounts, network, envFile, limits...)...)
	return append(append(args, spec.Image), spec.Cmd...)
}

// assembleOptions composes the workload options without a runtime verb, removal
// policy, image or command, so a filtered launch can create the same container
// inside its gateway namespace. Its networkName is already resolved: filtered
// callers pass "" and attach their controller namespace themselves. Never
// manufacture create arguments by stripping strings out of a run invocation.
func assembleOptions(cfg *config.Config, initProcess bool, spec RunSpec, mounts []Mount, decoy, decoyDir, workdir string, mode ttyMode, rawMCP bool, mcpMounts, consultMounts, gitMounts, instructionMounts, synthMounts []extraMount, networkName, envFile string, limits ...string) []string {
	var args []string
	if initProcess {
		// Docker's runtime-native contract: this PID 1 forwards signals to the workload and
		// reaps descendants orphaned by killed provider processes.
		args = append(args, "--init")
	}
	args = append(args, "--label", LabelKey+"="+LabelBox)
	// Who supervises this box: THIS process, scoped to the workspace it runs for. A host coop killed
	// by SIGKILL never fires --rm and leaves nobody watching, so a later invocation in the same
	// workspace reaps the box by proving this identity dead. Omitted when the host cannot produce a
	// stable identity — an unverifiable label is worse than none, and an unlabeled box is reported,
	// never reaped.
	if host := supervisorLabelValue(supervisorScope(spec), os.Getpid()); host != "" {
		args = append(args, "--label", LabelHost+"="+host)
	}
	if spec.RunID != "" {
		args = append(args, "--label", LabelRun+"="+spec.RunID)
	}
	if spec.activityID != "" {
		args = append(args, "--label", LabelExecution+"="+spec.activityID)
	}
	if spec.SupervisorID != "" {
		// A supervised inner box: coop.supervised=1 lets build/update restart it (the
		// editor reconnects); coop.sup=<id> lets its own supervisor kill exactly its
		// box(es) on teardown, so nothing is orphaned.
		args = append(args, "--label", LabelSupervised+"="+LabelOn, "--label", LabelSupervisor+"="+spec.SupervisorID)
	}
	if spec.ForkName != "" {
		// Keep the human name inspectable, but reap by the repo-scoped owner. Fork names are local to
		// a repo; using the readable label for cleanup would kill a namesake in another repository.
		args = append(args, "--label", LabelFork+"="+spec.ForkName, "--label", LabelForkOwner+"="+spec.ForkOwner)
		if spec.ForkGeneration != "" {
			args = append(args, "--label", LabelForkGeneration+"="+spec.ForkGeneration)
		}
		if spec.ForkWorker {
			args = append(args, "--label", LabelForkWorker+"="+LabelOn)
		}
	}
	switch mode {
	case ttyInteractive:
		// -e TERM propagates the host terminal type so the agents' TUIs render in
		// full color (without it the box reports a basic terminal — e.g. Gemini
		// warns about missing 256-color support).
		args = append(args, "-it", "-e", "TERM")
	case ttyStdinOnly:
		args = append(args, "-i")
	}
	// The image's clock is UTC; hand every box the HOST's timezone so agents render
	// clock times on the user's wall clock. Coop parses that prose back host-local when
	// scheduling a rate-limit wait ("try again at 4:28 PM"), so a UTC render would land
	// the wait hours off. TERM above is per-mode; the zone applies to every box.
	if tz := hostTimezone(); tz != "" {
		args = append(args, "-e", "TZ="+tz)
	}
	args = append(args, limits...) // resource/privilege caps (docker; nil elsewhere)
	args = append(args, RenderMounts(mounts, decoy, decoyDir)...)

	if spec.Homes {
		// Only the launched agent's credential home plus named consult peers — never every
		// agent's, so a plain run can't read the others'.
		scope := credentialScope(cfg, spec)
		for _, agent := range scope {
			args = append(args, "-v", cfg.AgentDir(agent)+":"+cfg.HomeInBox+"/."+agent)
		}
		// Synthesized skills: mounted READ-WRITE (a copy, so the host stays clean) so a CLI that
		// installs system skills into its skills dir isn't broken by a :ro mount.
		for _, m := range synthMounts {
			args = append(args, "-v", m.host+":"+m.box)
		}
		// An ACP box shares the LEAD's session transcripts across credentials, so
		// switching account/preset mid-session doesn't lose the conversation — session/load still finds
		// the transcript. The shared dir is credential-independent and shadows the profile's own copy.
		if spec.ShareACPSessions {
			primary := runPrimary(spec)
			if ag, ok := agents.Get(primary); ok {
				for _, name := range ag.ACPSessionDirs() {
					host := filepath.Join(acpSharedDir(cfg, primary), name)
					args = append(args, "-v", host+":"+cfg.HomeInBox+"/."+primary+"/"+name)
				}
			}
		}
		args = append(args, modelEnvArgs(cfg, spec, scope)...)
		// COOP_PRIMARY plus COOP_PEERS describe the mounted credential scope to role wrappers.
		// Direct consults are still restricted to peers; preset roles may deliberately use the
		// primary provider as one rung, so they validate against both.
		if len(scope) > 0 {
			args = append(args, "-e", "COOP_PRIMARY="+scope[0])
		}
		// COOP_PEERS is the space-separated peer set (scope minus the lead) — exactly the agents
		// whose credentials this box mounts as peers. The in-box coop-consult refuses any target
		// not in it, so a compromised lead can't consult (and thus can't drive) an agent the run
		// never named. Inert where the wrapper isn't mounted.
		if len(scope) > 1 {
			args = append(args, "-e", "COOP_PEERS="+strings.Join(scope[1:], " "))
		}
		// Each agent's own box env (Agent.BoxEnv) — claude points $CLAUDE_CONFIG_DIR at
		// the mounted ~/.claude and disables the bubblewrap env scrub. Exported for every
		// registered agent unconditionally (a var is inert where its agent isn't running),
		// so a new agent's env is a one-file adapter change, never a box.Run edit.
		for _, name := range agents.Names() {
			if a, ok := agents.Get(name); ok {
				for _, kv := range a.BoxEnv(cfg.HomeInBox) {
					args = append(args, "-e", kv)
				}
			}
		}
		// coop-consult reads COOP_CONSULT_TIMEOUT (seconds) for its per-peer timeout. Load has
		// already validated it; empty means the wrapper's unlimited default.
		if cfg.ConsultTimeout != "" {
			args = append(args, "-e", "COOP_CONSULT_TIMEOUT="+cfg.ConsultTimeout)
		}
		// Per-agent global instructions (the box env note + the user's, built in Run) at each
		// agent's native path. The consult lead is excluded there; its augmented file is the
		// consult mount just below.
		args = appendROMounts(args, instructionMounts)
		// The lead's augmented instructions and role wrappers.
		args = appendROMounts(args, consultMounts)
		// Your git environment: identity + signing-off + global gitignore.
		args = appendROMounts(args, gitMounts)
		if rawMCP {
			args = append(args, "-v", cfg.MCPFile+":"+cfg.MCPInBox+":ro")
		}
		args = appendROMounts(args, mcpMounts)
	}

	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	if spec.CapturedEgress == nil {
		// A filtered launch already folded COOP_RUN_ARGS into spec.ExtraArgs,
		// as bind mounts and environment assignments only (filteredExtraArgs).
		args = append(args, cfg.ExtraRunArgs...)
	}
	args = append(args, spec.ExtraArgs...)
	args = append(args, "-e", "COOP_BOX=1")
	if spec.SuperviseDescendants {
		// The provider cannot spoof these entrypoint-owned exits: coop-entry remaps a raw use
		// of either code to an ordinary failure before it reaches the host classifier.
		args = append(args, "-e", "COOP_SUPERVISE_DESCENDANTS=1")
	}
	// A filtered run publishes on its gateway controller instead: that container
	// owns the network namespace this box runs in (filteredPublish).
	if spec.Serve && spec.CapturedEgress == nil {
		args = appendPublish(args, cfg, spec, hostPortFree)
	}
	if networkName != "" {
		args = append(args, "--network", networkName)
	}
	if spec.Cache {
		args = append(args, "-v", "coop-cache:"+cfg.HomeInBox+"/.cache")
	}
	// The task channel's socket, read-only: the box connects to it and can neither replace nor
	// unlink it (connect needs write permission on the socket inode, which the helper set, not
	// on the mount).
	if spec.taskVolume != "" {
		args = append(args, "-v", taskChannelMount(spec.taskVolume))
	}
	// The base box provisions a repo's .tool-versions toolchain via asdf at run
	// time; persist ~/.asdf in a volume so installs survive the disposable box and
	// are reused across repos. Only the base image carries the asdf entrypoint.
	if spec.Homes && spec.Image == cfg.BaseImage {
		args = append(args, "-v", "coop-asdf:"+cfg.HomeInBox+"/.asdf")
	}
	return append(args, "-w", workdir)
}

// hostTimezone resolves the host's IANA zone name ("America/Merida"): $TZ when set,
// else the /etc/localtime symlink, else Debian-style /etc/timezone. Empty when none
// resolve — the box then keeps the image default (UTC).
func hostTimezone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(link, "/zoneinfo/"); i >= 0 {
			return link[i+len("/zoneinfo/"):]
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if tz := strings.TrimSpace(string(b)); tz != "" {
			return tz
		}
	}
	return ""
}

func writeTempFile(parent, content string) (string, error) {
	f, err := os.CreateTemp(parent, "coop-mcp-")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
