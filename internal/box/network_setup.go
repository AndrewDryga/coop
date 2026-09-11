package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/gatewayimage"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// SmokeDomain is the one destination the host preflight allows. It is a stable
// IANA-reserved example name, so a passing smoke proves enforcement rather than
// reachability of anything an operator might later care about.
const SmokeDomain = "example.com"

const smokeTimeout = 3 * time.Minute

// smokeExpectations name the fixture's distinct exit codes. The workload proves
// each one itself: the host never infers "denied" from a generic curl failure.
var smokeExpectations = map[int]string{
	31: "the allowed TLS destination was not reachable without proxy variables",
	32: "the allowed TLS destination returned an empty body",
	33: "a destination outside the policy was reachable",
	34: "a raw IP dial bypassed the allowed-name policy",
	35: "the link-local metadata address was reachable",
	36: "a denied name resolved through the gateway resolver",
}

// setupChecks are the five properties the smoke proves, in the order the script
// tests them; each names the exit codes that mean it failed. The script stops at
// the first failure, so an exit code says exactly which checks passed before it.
type setupCheck struct {
	property string
	codes    []int
}

var setupChecks = []setupCheck{
	{"approved TLS access to " + SmokeDomain + " works", []int{31, 32}},
	{"unapproved domains are blocked", []int{33}},
	{"direct IP connections cannot bypass domain rules", []int{34}},
	{"the cloud metadata address is blocked", []int{35}},
	{"DNS does not resolve unapproved domains", []int{36}},
}

// ErrNetworkSetupFailed marks a setup whose transcript already said everything:
// which check failed, that the host is not ready and that nothing was saved. A
// caller reports the exit status, not the verdict a second time.
var ErrNetworkSetupFailed = errors.New("this host is not ready for filtered runs")

// smokeScript runs entirely inside the qualified client image. curl gets no
// proxy variables on purpose: an arbitrary tool must work transparently, and a
// tool that only works because it was told about a proxy proves nothing.
const smokeScript = `set -eu
test "$COOP_BOX" = 1
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY http_proxy https_proxy all_proxy no_proxy
curl -q --proxy '' --noproxy '*' --fail --silent --show-error --max-time 20 https://` + SmokeDomain + ` -o /tmp/allowed.html || exit 31
test -s /tmp/allowed.html || exit 32
if curl -q --proxy '' --noproxy '*' --silent --max-time 5 https://example.org -o /dev/null; then exit 33; fi
if curl -q --proxy '' --noproxy '*' --silent --max-time 5 --resolve example.org:443:1.1.1.1 https://example.org -o /dev/null; then exit 34; fi
if curl -q --proxy '' --noproxy '*' --silent --max-time 3 http://169.254.169.254/ -o /dev/null; then exit 35; fi
if getent hosts example.org >/dev/null 2>&1; then exit 36; fi
`

// SetupNetwork is the per-host qualification behind `coop net setup`, and the
// one an ordinary filtered launch performs itself when this host has no current
// proof: it builds (or reuses) the pinned gateway and locked client images for
// the bound Docker daemon, proves them with ONE smoke run through the ordinary
// launch engine, and records what it proved. Nothing else ever builds an image
// or installs tooling for a filtered launch.
//
// It is host-wide, not per project: the smoke runs against a private temporary
// directory, which leaves no approval behind — approvals are written by
// `coop net approve`, never by admitting a capture. Setups serialize across
// processes, so two first launches never build and prove the same pair at once.
//
// The transcript on out is the whole result: what will happen in two sentences,
// then one doctor-style line per property the smoke actually proved, then the
// verdict. Colors follow out — a terminal gets them, a file or NO_COLOR does not.
func SetupNetwork(ctx context.Context, cfg *config.Config, rt runtime.Runtime, out, errOut io.Writer) (networkstate.Qualification, error) {
	if ctx == nil || cfg == nil {
		return networkstate.Qualification{}, errors.New("coop net setup needs host configuration and a cancelable context")
	}
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}
	root, err := NetworkStatePath()
	if err != nil {
		return networkstate.Qualification{}, err
	}
	scratch, err := os.MkdirTemp("", "coop-net-setup-")
	if err != nil {
		return networkstate.Qualification{}, err
	}
	defer os.RemoveAll(scratch)
	project, err := filepath.EvalSymlinks(scratch)
	if err != nil {
		return networkstate.Qualification{}, err
	}
	exposed := append([]string{project}, ConfigExposureRoots(cfg)...)
	store, err := networkstate.Open(root, exposed)
	if err != nil {
		return networkstate.Qualification{}, err
	}
	defer store.Close()
	var record networkstate.Qualification
	err = store.LockSetup(ctx, func() error {
		var err error
		record, err = qualifyNetworkHost(ctx, cfg, rt, store, project, out, errOut)
		return err
	})
	return record, err
}

func qualifyNetworkHost(ctx context.Context, cfg *config.Config, rt runtime.Runtime, store *networkstate.Store, project string, out, errOut io.Writer) (networkstate.Qualification, error) {
	started := time.Now()
	p := setupPalette(out)
	candidate, clients, err := setupImages(ctx, rt, store, p, out, errOut)
	if err != nil {
		return networkstate.Qualification{}, err
	}
	smoke, err := store.BeginQualification(candidate, clients)
	if err != nil {
		return networkstate.Qualification{}, err
	}
	fmt.Fprintf(out, "\n%s\n", p.Bold("Checking filtered access:"))
	proof, code, runErr := runSetupSmoke(ctx, cfg, rt, store, smoke, project, out)
	if err := writeSetupChecks(out, p, code, runErr); err != nil {
		return networkstate.Qualification{}, err
	}
	facts := map[string]string{
		"host_family": candidate.Runtime.HostFamily, "docker_endpoint": candidate.Runtime.Endpoint,
		"docker_daemon": candidate.Runtime.DaemonID, "docker_version": candidate.Runtime.ServerVersion,
		"docker_platform": candidate.Runtime.OS + "/" + candidate.Runtime.Architecture, "kernel": candidate.Runtime.KernelVersion,
		"client_image": candidate.ClientImage, "gateway_image": candidate.GatewayImage,
		"client_definition": candidate.ClientDefinition, "client_closure": candidate.ClientClosure,
		"gateway_source": candidate.GatewaySource, "node_base": candidate.NodeBase, "go_base": candidate.GoBase,
		"smoke_domain": SmokeDomain, "gateway_ready_ms": strconv.FormatInt(proof.ready.Milliseconds(), 10),
		"smoke_ms": strconv.FormatInt(proof.elapsed.Milliseconds(), 10),
		"setup_ms": strconv.FormatInt(time.Since(started).Milliseconds(), 10),
	}
	evidence, err := networkstate.QualificationEvidenceBytes(proof.record.Receipt.Snapshot, facts)
	if err != nil {
		return networkstate.Qualification{}, err
	}
	if _, err := smoke.RecordEvidence(proof.record.ID, evidence); err != nil {
		return networkstate.Qualification{}, err
	}
	return smoke.Complete(SmokeDomain)
}

// setupPalette colors the transcript for the stream it is written to, and not
// at all for a buffer or a file.
func setupPalette(out io.Writer) ui.Palette {
	if f, ok := out.(*os.File); ok {
		return ui.For(f)
	}
	return ui.Palette{}
}

// setupImages builds the pinned pair, or reports the exact pair already present.
// A rebuild of unchanged inputs is a Docker cache hit, so the honest distinction
// the operator cares about is whether the image existed before this run — and
// that is what the transcript says, in one sentence, before any build output.
func setupImages(ctx context.Context, rt runtime.Runtime, store *networkstate.Store, p ui.Palette, out, errOut io.Writer) (networkstate.CandidateSpec, []networkstate.QualifiedClient, error) {
	// Setup is the one path that may create runtime state, so it binds with
	// launch authority. Admission keeps the read-only inventory binding.
	docker, err := runtime.BindDocker(ctx, rt, "", "")
	if err != nil {
		return networkstate.CandidateSpec{}, nil, err
	}
	defer docker.Close()
	info := docker.Info()
	binding := networkRuntimeBinding(info, docker.Endpoint())
	definition, _, closure, err := lockedImageDefinition(agents.ClientPlatform{OS: binding.OS, Architecture: binding.Architecture, Libc: "glibc"})
	if err != nil {
		return networkstate.CandidateSpec{}, nil, err
	}
	gatewayPresent := networkImagePresent(ctx, docker, gatewayimage.Tag(), gatewayimage.BuildLabel, gatewayimage.Fingerprint())
	clientPresent := networkImagePresent(ctx, docker, definition.Tag, "coop.clients.definition", definition.Labels["coop.clients.definition"])
	fmt.Fprintf(out, "Setting up restricted networking using Docker %s on %s/%s.\n", info.ServerVersion, binding.OS, binding.Architecture)
	fmt.Fprintln(out, setupImageSentence(gatewayPresent, clientPresent))
	// The Docker build writes to the operator's terminal when there is one: a
	// first client build takes minutes, and silence reads as a hang. When both
	// images are already present the rebuild is a pure cache hit, so its progress
	// log is noise.
	var buildOut, buildErr *os.File
	if !clientPresent || !gatewayPresent {
		buildOut, _ = out.(*os.File)
		buildErr, _ = errOut.(*os.File)
	}
	candidate, err := BuildNetworkCandidate(ctx, docker, buildOut, buildErr)
	if err != nil {
		return networkstate.CandidateSpec{}, nil, err
	}
	if note := setupClientFiles(ctx, docker, store, candidate, closure); note != "" {
		fmt.Fprintf(out, "  %s\n", p.Dim(note))
	}
	var qualified []networkstate.QualifiedClient
	for _, client := range closure.Clients {
		qualified = append(qualified, networkstate.QualifiedClient{Provider: client.Provider, Client: client.Client, Version: client.Version})
	}
	return candidate, qualified, nil
}

// setupImageSentence says what happens to each image, honestly: "first setup"
// only when both are built, and each outcome by name when they differ.
func setupImageSentence(gatewayPresent, clientPresent bool) string {
	switch {
	case gatewayPresent && clientPresent:
		return "The gateway and client images are already available and will be reused."
	case !gatewayPresent && !clientPresent:
		return "The gateway and client images need to be built.\nThe first setup can take several minutes."
	case gatewayPresent:
		return "The gateway image will be reused; the client image needs to be built."
	default:
		return "The gateway image needs to be built; the client image will be reused."
	}
}

// setupClientFiles reads every pinned client entry point out of the locked image
// ONCE, here, where the daemon is already in hand. A project with its own
// Dockerfile then compares its build against this host's own record instead of
// copying a few hundred megabytes back out of an image that cannot have changed:
// an image id is a content address, so the same id is the same bytes.
//
// It is a memo, not a qualification, so it costs no line when it works. A read
// that fails is a secondary note and setup carries on, because the launch that
// needs those digests reads them itself and refuses by name when it cannot.
func setupClientFiles(ctx context.Context, docker filteredDocker, store *networkstate.Store, candidate networkstate.CandidateSpec, closure agents.ClientClosure) string {
	files := pinnedClientFiles(closure)
	if len(files) == 0 {
		return ""
	}
	if _, err := imageFileDigests(ctx, docker, store, candidate.ClientImage, files); err != nil {
		return fmt.Sprintf("the client entry points could not be recorded (%v) — a project Dockerfile reads them at launch instead", err)
	}
	return ""
}

func networkImagePresent(ctx context.Context, docker *runtime.Docker, ref, label, want string) bool {
	_, labels, err := docker.Image(ctx, ref)
	return err == nil && want != "" && labels[label] == want
}

type setupProof struct {
	record  networkstate.Execution
	ready   time.Duration
	elapsed time.Duration
}

// runSetupSmoke drives the ONE preflight run through the ordinary launch engine.
// Its configuration is built here rather than copied from the operator's: an
// image override or extra runtime argument must not reach what the record
// claims to have proven. It returns the workload's exit code — the verdict the
// script encodes — and separately whether the run reached one at all.
func runSetupSmoke(ctx context.Context, cfg *config.Config, rt runtime.Runtime, store *networkstate.Store, smoke *networkstate.QualificationSmoke, project string, out io.Writer) (setupProof, int, error) {
	mode := egress.Filtered
	rules, err := egress.NormalizeRules([]egress.Rule{{To: egress.Destination{Domain: SmokeDomain}, Protocol: "tls", Ports: []int{443}}})
	if err != nil {
		return setupProof{}, 0, err
	}
	policy, err := store.Admit(project, networkstate.Admission{InvocationMode: &mode,
		Operator: []egress.Input{{Origin: egress.Origin{Kind: "operator", Name: "net-setup"}, Rules: rules}}})
	if err != nil {
		return setupProof{}, 0, err
	}
	home := cfg.HomeInBox
	if home == "" {
		home = "/home/node"
	}
	smokeCfg := &config.Config{ConfigDir: cfg.ConfigDir, BoxHome: cfg.BoxHome, HomeInBox: home, Egress: string(egress.Filtered),
		Memory: cfg.Memory, CPUs: cfg.CPUs, Pids: cfg.Pids, NoNewPrivileges: cfg.NoNewPrivileges}
	run, cancel := context.WithTimeout(ctx, smokeTimeout)
	defer cancel()
	// The permit's callback runs on THIS goroutine, inside launch preparation, so
	// runID needs no synchronization; the watcher gets its own copy.
	var runID string
	registered := make(chan string, 1)
	ready := make(chan time.Duration, 1)
	started := time.Now()
	go watchGatewayReady(run, store, registered, ready, started)
	code, err := runWithNetworkSmoke(smokeCfg, rt, RunSpec{
		Repo: project, Workdir: "/workspace", Batch: true, Quiet: true, Ctx: run, Stdout: out, Stderr: out,
		Cmd:            []string{"sh", "-c", smokeScript},
		CapturedEgress: &CapturedEgress{Store: store, Project: project, Fingerprint: policy.Fingerprint},
	}, defaultCompositionArtifactOps(), &networkSmokeLaunch{
		authority:  smoke,
		registered: func(r networkstate.Execution) { runID = r.ID; registered <- r.ID },
	})
	elapsed := time.Since(started)
	if err != nil {
		return setupProof{}, 0, errors.Join(errors.New("the setup check did not finish"), err)
	}
	if code != 0 {
		return setupProof{}, code, nil
	}
	record, err := store.Execution(runID)
	if err != nil {
		return setupProof{}, 0, err
	}
	if record.Receipt == nil {
		return setupProof{}, 0, errors.New("the setup check sealed no receipt")
	}
	proof := setupProof{record: record, elapsed: elapsed}
	select {
	case proof.ready = <-ready:
	default:
	}
	return proof, 0, nil
}

// writeSetupChecks renders the verdicts the smoke reached and the final one.
// Only checks the script actually ran are claimed: a failure shows the passes
// before it and the failed property as its reason, and nothing after it. The
// error it returns is ErrNetworkSetupFailed, because the transcript is the
// report.
func writeSetupChecks(out io.Writer, p ui.Palette, code int, runErr error) error {
	failed := slices.IndexFunc(setupChecks, func(check setupCheck) bool { return slices.Contains(check.codes, code) })
	passed := len(setupChecks)
	switch {
	case runErr != nil || (code != 0 && failed < 0):
		passed = 0
	case failed >= 0:
		passed = failed
	}
	for _, check := range setupChecks[:passed] {
		fmt.Fprintf(out, "  %s %s\n", p.Green("✓"), check.property)
	}
	if runErr == nil && code == 0 {
		fmt.Fprintf(out, "\n%s\n", p.Bold(p.Green(fmt.Sprintf("✓ All %d checks passed — this host is ready for filtered runs", len(setupChecks)))))
		return nil
	}
	reason := ""
	switch {
	case runErr != nil:
		reason = runErr.Error()
	case failed >= 0:
		reason = smokeExpectations[code]
		fmt.Fprintf(out, "  %s %s\n", p.Red("✗"), reason)
	default:
		reason = fmt.Sprintf("the setup check exited %d without reaching a verdict", code)
	}
	fmt.Fprintf(out, "\n%s\n", p.Bold(p.Red("✗ "+ErrNetworkSetupFailed.Error())))
	if failed < 0 {
		// A reason joined from several errors is still one reason: every line of
		// it sits at the verdict's depth, so the runtime's own message reads as
		// part of the answer instead of falling out of the block.
		for _, line := range strings.Split(strings.TrimRight(reason, "\n"), "\n") {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}
	fmt.Fprintln(out, "  No setup was saved")
	return fmt.Errorf("%w: %s", ErrNetworkSetupFailed, reason)
}

// watchGatewayReady times the boundary the operator actually waits on: policy
// installed and the gateway answering, before the workload starts. It only
// reads the owner-private registry; it never touches the runtime.
func watchGatewayReady(ctx context.Context, store *networkstate.Store, registered <-chan string, ready chan<- time.Duration, started time.Time) {
	var runID string
	select {
	case runID = <-registered:
	case <-ctx.Done():
		return
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		r, err := store.Execution(runID)
		if err != nil {
			return
		}
		if r.ReadySequence != 0 {
			ready <- time.Since(started)
			return
		}
		if r.Receipt != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
