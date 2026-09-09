package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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

// SetupNetwork is the per-host preflight behind `coop net setup`: it builds (or
// reuses) the pinned gateway and locked client images for the bound Docker
// daemon, proves them with ONE smoke run through the ordinary launch engine,
// and records what it proved. Nothing else ever builds an image or installs
// tooling for a filtered launch.
//
// It is host-wide, not per project: the smoke runs against a private temporary
// directory, which leaves no approval behind — approvals are written by
// `coop net approve`, never by admitting a capture.
func SetupNetwork(ctx context.Context, cfg *config.Config, rt runtime.Runtime, out, errOut io.Writer) (networkstate.Qualification, error) {
	if ctx == nil || cfg == nil {
		return networkstate.Qualification{}, errors.New("network setup requires host configuration and a cancelable context")
	}
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}
	started := time.Now()
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
	setupStep(out, "authority", "%s", store.Path())

	candidate, clients, err := setupImages(ctx, rt, out, errOut)
	if err != nil {
		return networkstate.Qualification{}, err
	}
	smoke, err := store.BeginQualification(candidate, clients)
	if err != nil {
		return networkstate.Qualification{}, err
	}
	proof, err := runSetupSmoke(ctx, cfg, rt, store, smoke, project, out)
	if err != nil {
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
	record, err := smoke.Complete(SmokeDomain)
	if err != nil {
		return networkstate.Qualification{}, err
	}
	printSetupSummary(out, record, proof)
	return record, nil
}

// setupImages builds the pinned pair, or reports the exact pair already present.
// A rebuild of unchanged inputs is a Docker cache hit, so the honest distinction
// the operator cares about is whether the image existed before this run.
func setupImages(ctx context.Context, rt runtime.Runtime, out, errOut io.Writer) (networkstate.CandidateSpec, []networkstate.QualifiedClient, error) {
	// Setup is the one path that may create runtime state, so it binds with
	// launch authority. Admission keeps the read-only inventory binding.
	docker, err := runtime.BindDocker(ctx, rt, "", "")
	if err != nil {
		return networkstate.CandidateSpec{}, nil, err
	}
	defer docker.Close()
	info := docker.Info()
	binding := networkRuntimeBinding(info, docker.Endpoint())
	setupStep(out, "runtime", "docker %s · %s/%s · daemon %s", info.ServerVersion, binding.OS, binding.Architecture, binding.DaemonID)
	definition, _, closure, err := lockedImageDefinition(agents.ClientPlatform{OS: binding.OS, Architecture: binding.Architecture, Libc: "glibc"})
	if err != nil {
		return networkstate.CandidateSpec{}, nil, err
	}
	gatewayPresent := networkImagePresent(ctx, docker, gatewayimage.Tag(), gatewayimage.BuildLabel, gatewayimage.Fingerprint())
	clientPresent := networkImagePresent(ctx, docker, definition.Tag, "coop.clients.definition", definition.Labels["coop.clients.definition"])
	// The Docker build writes to the operator's terminal when there is one: a
	// first client build takes minutes, and silence reads as a hang. When both
	// images are already present the rebuild is a pure cache hit, so its progress
	// log is noise.
	var buildOut, buildErr *os.File
	if !clientPresent || !gatewayPresent {
		setupStep(out, "images", "building the pinned gateway and locked client images (first run takes several minutes)")
		buildOut, _ = out.(*os.File)
		buildErr, _ = errOut.(*os.File)
	}
	candidate, err := BuildNetworkCandidate(ctx, docker, buildOut, buildErr)
	if err != nil {
		return networkstate.CandidateSpec{}, nil, err
	}
	setupStep(out, "gateway", "%-6s %s", setupOrigin(gatewayPresent), candidate.GatewayImage)
	setupStep(out, "clients", "%-6s %s", setupOrigin(clientPresent), candidate.ClientImage)
	var qualified []networkstate.QualifiedClient
	for _, client := range closure.Clients {
		qualified = append(qualified, networkstate.QualifiedClient{Provider: client.Provider, Client: client.Client, Version: client.Version})
	}
	return candidate, qualified, nil
}

func setupOrigin(present bool) string {
	if present {
		return "reused"
	}
	return "built"
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
// claims to have proven.
func runSetupSmoke(ctx context.Context, cfg *config.Config, rt runtime.Runtime, store *networkstate.Store, smoke *networkstate.QualificationSmoke, project string, out io.Writer) (setupProof, error) {
	mode := egress.Filtered
	rules, err := egress.NormalizeRules([]egress.Rule{{To: egress.Destination{Domain: SmokeDomain}, Protocol: "tls", Ports: []int{443}}})
	if err != nil {
		return setupProof{}, err
	}
	policy, err := store.Admit(project, networkstate.Admission{InvocationMode: &mode,
		Operator: []egress.Input{{Origin: egress.Origin{Kind: "operator", Name: "net-setup"}, Rules: rules}}})
	if err != nil {
		return setupProof{}, err
	}
	home := cfg.HomeInBox
	if home == "" {
		home = "/home/node"
	}
	smokeCfg := &config.Config{ConfigDir: cfg.ConfigDir, BoxHome: cfg.BoxHome, HomeInBox: home, Egress: string(egress.Filtered),
		Memory: cfg.Memory, CPUs: cfg.CPUs, Pids: cfg.Pids, NoNewPrivileges: cfg.NoNewPrivileges}
	run, cancel := context.WithTimeout(ctx, smokeTimeout)
	defer cancel()
	setupStep(out, "smoke", "%s allowed; denied name, raw IP, metadata and DNS refused", SmokeDomain)
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
		return setupProof{}, errors.Join(errors.New("the network smoke run did not complete"), err)
	}
	if reason, named := smokeExpectations[code]; named {
		return setupProof{}, errors.New("network setup refused: " + reason)
	}
	if code != 0 {
		return setupProof{}, fmt.Errorf("the network smoke workload exited %d without reaching a verdict", code)
	}
	record, err := store.Execution(runID)
	if err != nil {
		return setupProof{}, err
	}
	if record.Receipt == nil {
		return setupProof{}, errors.New("the network smoke run sealed no receipt")
	}
	proof := setupProof{record: record, elapsed: elapsed}
	select {
	case proof.ready = <-ready:
	default:
	}
	return proof, nil
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

func setupStep(out io.Writer, label, format string, a ...any) {
	fmt.Fprintf(out, "  %-9s %s\n", label, ui.Dim(fmt.Sprintf(format, a...)))
}

func printSetupSummary(out io.Writer, record networkstate.Qualification, proof setupProof) {
	fmt.Fprintf(out, "\n%s\n", ui.Bold("network setup complete"))
	fmt.Fprintf(out, "  %-9s docker %s · %s/%s · daemon %s\n", "runtime", record.Candidate.Runtime.ServerVersion,
		record.Candidate.Runtime.OS, record.Candidate.Runtime.Architecture, record.Candidate.Runtime.DaemonID)
	fmt.Fprintf(out, "  %-9s %s\n", "gateway", record.Candidate.GatewayImage)
	fmt.Fprintf(out, "  %-9s %s\n", "clients", record.Candidate.ClientImage)
	for _, client := range record.Clients {
		fmt.Fprintf(out, "  %-9s %s %s %s\n", "", client.Provider, client.Client, client.Version)
	}
	fmt.Fprintf(out, "  %-9s %s allowed, denials enforced · gateway ready in %s · run %s\n", "smoke",
		SmokeDomain, proof.ready.Round(time.Millisecond), proof.elapsed.Round(time.Millisecond))
	fmt.Fprintf(out, "  %-9s %s\n", "record", record.ID)
}
