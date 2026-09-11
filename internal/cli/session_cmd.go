package cli

// Command wiring for `coop sessions`: flag parsing, the CLI's path defaults, the serve loop that
// owns signal handling, the doctor command that owns the terminal — and the Host adapters that
// hand internal/sessionsvc the three things a library cannot own for itself (the merge policy
// scan, the review gate built on this repo's merge image, and a warning line on the terminal).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkctl"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/ui"
)

func defaultSessionStateRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".local", "state", "coop", "sessions"), nil
}

func defaultSessionPolicyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".config", "coop", "session-policies.yaml"), nil
}

func sessionCLIPaths(state, policy, socket string) (string, string, string, error) {
	state, err := sessionStatePath(state)
	if err != nil {
		return "", "", "", err
	}
	if policy == "" {
		policy, err = defaultSessionPolicyPath()
		if err != nil {
			return "", "", "", err
		}
	}
	if socket == "" {
		socket = filepath.Join(state, "control.sock")
	} else {
		socket, err = filepath.Abs(filepath.Clean(socket))
		if err != nil {
			return "", "", "", fmt.Errorf("resolve session socket path: %w", err)
		}
	}
	policy, err = filepath.Abs(filepath.Clean(policy))
	if err != nil {
		return "", "", "", fmt.Errorf("resolve session policy path: %w", err)
	}
	return state, policy, socket, nil
}

func sessionStatePath(state string) (string, error) {
	if state == "" {
		var err error
		state, err = defaultSessionStateRoot()
		if err != nil {
			return "", err
		}
	}
	state, err := filepath.Abs(filepath.Clean(state))
	if err != nil {
		return "", fmt.Errorf("resolve session state path: %w", err)
	}
	return state, nil
}

func parseSessionsFlags(args []string, command string) (state, policy, socket string, jsonOutput bool, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--json" && (command == "doctor" || command == "policies") {
			if jsonOutput {
				return "", "", "", false, fmt.Errorf("sessions %s: --json may be specified once", command)
			}
			jsonOutput = true
			continue
		}
		var target *string
		switch arg {
		case "--state":
			target = &state
		case "--policies":
			target = &policy
		case "--socket":
			target = &socket
		default:
			return "", "", "", false, fmt.Errorf("sessions %s: unknown flag %q", command, arg)
		}
		if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
			return "", "", "", false, fmt.Errorf("sessions %s: flag %s requires a value", command, arg)
		}
		i++
		if *target != "" {
			return "", "", "", false, fmt.Errorf("sessions %s: flag %s may be specified once", command, arg)
		}
		*target = args[i]
	}
	switch command {
	case "doctor":
		if state != "" || policy != "" {
			return "", "", "", false, errors.New("sessions doctor: only --socket and --json are supported")
		}
	case "policies":
		if state != "" || socket != "" {
			return "", "", "", false, errors.New("sessions policies: only --policies and --json are supported")
		}
	}
	return state, policy, socket, jsonOutput, nil
}

func parseSessionCompactFlags(args []string) (state, backup string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		var target *string
		switch arg {
		case "--state":
			target = &state
		case "--backup":
			target = &backup
		default:
			return "", "", fmt.Errorf("sessions compact: unknown flag %q", arg)
		}
		if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
			return "", "", fmt.Errorf("sessions compact: flag %s requires a value", arg)
		}
		i++
		if *target != "" {
			return "", "", fmt.Errorf("sessions compact: flag %s may be specified once", arg)
		}
		*target = args[i]
	}
	if backup == "" {
		return "", "", errors.New("sessions compact: --backup <path> is required")
	}
	return state, backup, nil
}

func (a *app) cmdSessions(args []string) (int, error) {
	if len(args) == 0 {
		return groupHelp("sessions")
	}
	switch args[0] {
	case "serve":
		state, policy, socket, _, err := parseSessionsFlags(args[1:], "serve")
		if err != nil {
			return 2, err
		}
		return runSessionServe(a.cfg, state, policy, socket)
	case "doctor":
		_, _, socket, jsonOutput, err := parseSessionsFlags(args[1:], "doctor")
		if err != nil {
			return 2, err
		}
		return runSessionDoctor(socket, jsonOutput)
	case "policies":
		_, policy, _, jsonOutput, err := parseSessionsFlags(args[1:], "policies")
		if err != nil {
			return 2, err
		}
		return runSessionPolicies(a.cfg, policy, jsonOutput)
	case "compact":
		state, backup, err := parseSessionCompactFlags(args[1:])
		if err != nil {
			return 2, err
		}
		return runSessionCompact(state, backup)
	default:
		return 2, fmt.Errorf("sessions: unknown command %q", args[0])
	}
}

func runSessionCompact(state, backup string) (int, error) {
	state, err := sessionStatePath(state)
	if err != nil {
		return 2, err
	}
	backup, err = filepath.Abs(filepath.Clean(backup))
	if err != nil {
		return 2, fmt.Errorf("resolve session backup path: %w", err)
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	result, err := sessionsvc.CompactSessionState(ctx, state, backup)
	if err != nil {
		return 1, err
	}
	if result.CompactedOperations == 0 {
		ui.Note("no legacy turn retry receipts needed compaction")
	} else {
		ui.OK("compacted %s (%d -> %d bytes)", ui.Count(result.CompactedOperations, "turn retry receipt"), result.ResultBytesBefore, result.ResultBytesAfter)
	}
	ui.Note("database: %d -> %d bytes", result.DatabaseBytesBefore, result.DatabaseBytesAfter)
	ui.Note("backup: %s (%d bytes)", result.BackupPath, result.BackupBytes)
	ui.Warn("backup contains private session data; protect it and remove it after recovery is no longer needed")
	return 0, nil
}

type sessionPoliciesResult struct {
	PolicyFile             string                          `json:"policy_file"`
	PolicyDigests          map[string]string               `json:"policy_digests"`
	PolicyAuthorityDigests map[string]string               `json:"policy_authority_digests"`
	PolicyNetworks         map[string]sessionPolicyNetwork `json:"policy_networks"`
}

// sessionPolicyNetwork is what a policy's sessions may reach. It is printed beside the digests
// because it IS authority: an operator authorizing a fleet worker has to see the posture and the
// rules the same way they see the repository and the target.
//
// Fingerprint is the reach RESOLVED on this host — the project's remembered approval and the
// provider and MCP dependencies included — which is the value a placement pins and the daemon
// publishes. Unresolved carries the reason instead, for a policy this host cannot resolve at all;
// the daemon refuses to serve that policy, and this read says why.
type sessionPolicyNetwork struct {
	Mode               string   `json:"mode"`
	Rules              []string `json:"rules,omitempty"`
	ExportDestinations bool     `json:"export_destinations,omitempty"`
	Fingerprint        string   `json:"fingerprint,omitempty"`
	Unresolved         string   `json:"unresolved,omitempty"`
}

func sessionPolicyNetworkOf(cfg *config.Config, policy sessionsvc.Policy) sessionPolicyNetwork {
	out := sessionPolicyNetwork{
		Mode: string(policy.Egress.Mode), ExportDestinations: policy.Egress.ExportDestinations,
	}
	if out.Mode == "" {
		out.Mode = "open (default)"
	}
	for _, rule := range policy.Egress.Rules {
		out.Rules = append(out.Rules, box.NetworkRuleText(rule))
	}
	resolved, err := sessionsvc.ResolvePolicyNetwork(cfg, policy)
	switch {
	case err != nil:
		out.Unresolved = err.Error()
	case resolved.Fingerprint != "":
		out.Mode, out.Fingerprint = string(resolved.Mode), resolved.Fingerprint
	default:
		out.Mode = string(resolved.Mode)
	}
	return out
}

func runSessionPolicies(cfg *config.Config, policyPath string, jsonOutput bool) (int, error) {
	_, policyPath, _, err := sessionCLIPaths("", policyPath, "")
	if err != nil {
		return 2, err
	}
	policies, err := sessionsvc.LoadPolicies(policyPath, cfg)
	if err != nil {
		return 1, err
	}
	result := sessionPoliciesResult{
		PolicyFile:             policyPath,
		PolicyDigests:          make(map[string]string, len(policies)),
		PolicyAuthorityDigests: make(map[string]string, len(policies)),
		PolicyNetworks:         make(map[string]sessionPolicyNetwork, len(policies)),
	}
	names := make([]string, 0, len(policies))
	for name, policy := range policies {
		names = append(names, name)
		result.PolicyDigests[name] = sessionsvc.ResolvedPolicyDigest(policy)
		result.PolicyAuthorityDigests[name] = sessionsvc.ResolvedPolicyAuthorityDigest(policy)
		result.PolicyNetworks[name] = sessionPolicyNetworkOf(cfg, policy)
	}
	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return 1, err
		}
		return 0, nil
	}
	sort.Strings(names)
	renderSessionPolicies(os.Stdout, ui.For(os.Stdout), result, names)
	return 0, nil
}

// renderSessionPolicies prints one labeled block per policy (entity-blocks-with-labeled-fields):
// the two digests are separate facts a fleet operator copies into a worker config, so each gets
// its own labeled line instead of an unlabeled tab-separated row.
func renderSessionPolicies(w io.Writer, p ui.Palette, result sessionPoliciesResult, names []string) {
	fmt.Fprintf(w, "%s %s\n", p.Dim("Policy file:"), result.PolicyFile)
	for _, name := range names {
		fmt.Fprintln(w)
		fmt.Fprintln(w, p.Bold(p.Cyan(name)))
		fmt.Fprintf(w, "  %s     %s\n", p.Dim("Policy digest:"), result.PolicyDigests[name])
		fmt.Fprintf(w, "  %s  %s\n", p.Dim("Authority digest:"), result.PolicyAuthorityDigests[name])
		network := result.PolicyNetworks[name]
		fmt.Fprintf(w, "  %s           %s\n", p.Dim("Network:"), network.Mode)
		for _, rule := range network.Rules {
			fmt.Fprintf(w, "  %s             %s\n", p.Dim("allow:"), rule)
		}
		if network.ExportDestinations {
			fmt.Fprintf(w, "  %s            %s\n", p.Dim("export:"), "destinations are exported to the authorized worker")
		}
		switch {
		case network.Unresolved != "":
			fmt.Fprintf(w, "  %s       %s\n", p.Dim("Fingerprint:"), "unresolved on this host — "+network.Unresolved)
		case network.Fingerprint != "":
			fmt.Fprintf(w, "  %s       %s\n", p.Dim("Fingerprint:"), network.Fingerprint)
		}
	}
}

func runSessionServe(cfg *config.Config, state, policy, socket string) (int, error) {
	state, policy, socket, err := sessionCLIPaths(state, policy, socket)
	if err != nil {
		return 2, err
	}
	if err := sessionsvc.EnsureAncestors(filepath.Dir(state)); err != nil {
		return 1, err
	}
	// The optional `storage:` block in the same policy file. Absent, the daemon derives its limits
	// from the measured volume; present and incoherent, it refuses to start rather than discovering
	// the problem the first time the disk fills.
	storageLimits, configuredStorage, err := sessionsvc.LoadStorageLimits(policy)
	if err != nil {
		return 1, err
	}
	serviceConfig := sessionsvc.Config{
		StateRoot: state, PolicyPath: policy, SourceConfig: cfg, Executable: os.Args[0],
		Host: sessionHost(), Logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	if configuredStorage {
		serviceConfig.StorageLimits = &storageLimits
	}
	service, err := sessionsvc.NewService(serviceConfig)
	if err != nil {
		return 1, err
	}
	defer service.Stop()
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if err := service.Start(ctx); err != nil {
		return 1, err
	}
	listener, cleanup, err := sessionsvc.ListenSocket(state, socket)
	if err != nil {
		return 1, err
	}
	defer cleanup()
	server := &http.Server{Handler: sessionsvc.NewHTTPHandler(service)}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	select {
	case err := <-serveDone:
		if errors.Is(err, http.ErrServerClosed) {
			return 0, nil
		}
		return 1, err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), sessionsvc.DefaultStopTimeout)
		shutdownErr := server.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			_ = server.Close()
			<-serveDone
			return 1, shutdownErr
		}
		if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return 1, err
		}
		return 0, nil
	}
}

// sessionHost is everything the sessions service takes from the host that owns the terminal and
// the merge policy. Each field is one existing cli function, forwarded.
func sessionHost() sessionsvc.Host {
	return sessionsvc.Host{
		PolicyScan:        forkctl.PolicyScan,
		ReviewGateFactory: defaultSessionReviewGate,
		Warnf:             ui.Warn,
	}
}

// defaultSessionReviewGate runs a review candidate through THIS repo's merge gate — the same image
// and the same pass/fail rule `coop fork merge` uses, so a session's verdict can't drift from the
// one a human gets. The config and runtime are the service's, resolved by the time it asks; a
// runtime it hasn't detected yet is the zero value, which the control plane detects on demand.
func defaultSessionReviewGate(cfg *config.Config, rt runtime.Runtime) sessionsvc.ReviewGate {
	return sessionsvc.ReviewGateFunc(func(ctx context.Context, gateRepo, treeDir string) (sessionsvc.ReviewGateResult, error) {
		if err := ctx.Err(); err != nil {
			return sessionsvc.ReviewGateResult{}, err
		}
		fc := forkctl.New(cfg, rt, forkctl.Host{EnsureRuntime: func() (runtime.Runtime, error) {
			if rt.Name != "" {
				return rt, nil
			}
			return runtime.Detect(cfg.RuntimeName)
		}})
		image, err := fc.MergeGate(gateRepo)
		if err != nil {
			return sessionsvc.ReviewGateResult{Configured: true, StartupError: sessionsvc.SanitizeReviewText(err.Error(), sessionsvc.MaxReviewErrorBytes)}, nil
		}
		if image == "" {
			return sessionsvc.ReviewGateResult{}, nil
		}
		if err := ctx.Err(); err != nil {
			return sessionsvc.ReviewGateResult{}, err
		}
		passed, err := fc.ReviewGatePasses(gateRepo, treeDir, image)
		if err != nil {
			return sessionsvc.ReviewGateResult{Configured: true, StartupError: sessionsvc.SanitizeReviewText(err.Error(), sessionsvc.MaxReviewErrorBytes)}, nil
		}
		return sessionsvc.ReviewGateResult{Configured: true, Passed: passed}, nil
	})
}

// sessionReadyDTO decodes /readyz. The service owns the endpoint's shape; the doctor is a client
// of it like any other, so it reads the field it needs rather than importing the server's struct.
type sessionReadyDTO struct {
	Ready bool `json:"ready"`
}

type sessionDoctorResult struct {
	Socket  string `json:"socket"`
	Healthy bool   `json:"healthy"`
	Ready   bool   `json:"ready"`
	Error   string `json:"error,omitempty"`
}

func runSessionDoctor(socket string, jsonOutput bool) (int, error) {
	_, _, socket, err := sessionCLIPaths("", "", socket)
	if err != nil {
		return 2, err
	}
	result := sessionDoctorResult{Socket: socket}
	client := sessionUnixHTTPClient(socket)
	healthErr := sessionDoctorGet(client, "/healthz", &result)
	if healthErr == nil {
		result.Healthy = true
	}
	var ready sessionReadyDTO
	readyErr := sessionDoctorGet(client, "/readyz", &ready)
	if readyErr == nil {
		result.Ready = ready.Ready
	}
	if healthErr != nil {
		result.Error = sessionsvc.BoundedDetail(healthErr.Error())
	}
	if readyErr != nil {
		if result.Error != "" {
			result.Error += "; "
		}
		result.Error += sessionsvc.BoundedDetail(readyErr.Error())
	}
	if result.Error == "" && !result.Ready {
		result.Error = "session service is not ready"
	}
	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return 1, err
		}
	} else if result.Error != "" {
		ui.Error("session service is unavailable or unready: %s", result.Error)
	} else {
		ui.OK("session service is healthy and ready at %s", result.Socket)
	}
	if result.Error != "" || !result.Healthy || !result.Ready {
		return 1, nil
	}
	return 0, nil
}

func sessionUnixHTTPClient(socket string) *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func sessionDoctorGet(client *http.Client, path string, value any) error {
	request, err := http.NewRequest(http.MethodGet, "http://unix"+path, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("connect to session socket: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("session endpoint returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(value); err != nil {
		return fmt.Errorf("decode session endpoint: %w", err)
	}
	return nil
}
