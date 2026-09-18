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
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkctl"
	"github.com/AndrewDryga/coop/internal/networkstate"
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
	return filepath.Join(config.RootDir(), "session-policies.yaml"), nil
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

// sessionCommands are the `coop sessions` subcommands — the one list the dispatch below, the
// unknown-subcommand correction, the help router and shell completion all read.
var sessionCommands = []string{"connect", "serve", "doctor", "policies", "compact"}

func (a *app) cmdSessions(args []string) (int, error) {
	if len(args) == 0 {
		return groupHelp("sessions")
	}
	switch args[0] {
	case "connect":
		path, err := parseSessionConnectFlags(args[1:])
		if err != nil {
			return 2, err
		}
		return runSessionConnect(a.cfg, path)
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
		return 2, unknownSubcommandErr("sessions", args[0], sessionCommands)
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
		return 1, sessionCompactFailure(result, err)
	}
	// Two separate facts, each labeled rather than printed as an unexplained arrow: the logical
	// receipt bytes the rewrite saved, and the allocated database bytes VACUUM reclaimed. Zero
	// legacy receipts is not "nothing happened" — the backup was still made and verified.
	rows := [][2]string{}
	if result.CompactedOperations > 0 {
		ui.OK("Session data compacted")
		rows = append(rows,
			[2]string{"Retry records", fmt.Sprintf("%d compacted", result.CompactedOperations)},
			[2]string{"Record data", byteChange(result.ResultBytesBefore, result.ResultBytesAfter)})
	} else {
		ui.Note("No older retry records needed compaction.")
	}
	rows = append(rows,
		[2]string{"Database", byteChange(result.DatabaseBytesBefore, result.DatabaseBytesAfter)},
		[2]string{"Backup", result.BackupPath + " · " + ui.Bytes(uint64(result.BackupBytes))})
	ui.Note("")
	for _, row := range rows {
		ui.Note("  %s  %s", padRight(row[0], sessionLabelWidth(rows)), row[1])
	}
	ui.Note("")
	ui.Warn("The backup contains private session data")
	ui.Note("  Keep it protected until you no longer need it for recovery.")
	return 0, nil
}

func sessionLabelWidth(rows [][2]string) int {
	w := 0
	for _, row := range rows {
		if n := utf8.RuneCountInString(row[0]); n > w {
			w = n
		}
	}
	return w
}

func byteChange(before, after int64) string {
	return ui.Bytes(uint64(before)) + " → " + ui.Bytes(uint64(after))
}

// sessionCompactFailure says which STAGE failed, from what the run actually proved. A verified
// backup exists only once BackupPath is set (backupSQLiteDatabase removes its own incomplete
// output), and only ErrCompactionUnfinished proves the receipt rewrite committed — a partial
// count does not. Everything else keeps the bounded cause it came with.
func sessionCompactFailure(result sessionsvc.CompactionResult, err error) error {
	detail := sessionsvc.BoundedDetail(err.Error())
	switch {
	case errors.Is(err, sessionsvc.ErrCompactionUnfinished):
		// The marker's own sentence is the stage, which the headline already carries; what a
		// person still needs is the bounded reason under it.
		detail = strings.TrimPrefix(strings.TrimPrefix(detail, sessionsvc.ErrCompactionUnfinished.Error()), "\n")
		return ui.CommandFailed("Could not finish compacting session data",
			"Retry records were compacted, but database space could not be reclaimed.\n"+strings.TrimLeft(detail, ": "),
			[2]string{"Backup:", result.BackupPath})
	case strings.Contains(detail, "another session daemon owns this state root"):
		return ui.CommandFailed("Could not compact session data",
			"The session service is using this data directory.",
			[2]string{"", "Stop the service, then run the command again."})
	case strings.Contains(detail, "already exists"):
		return ui.CommandFailed("Could not compact session data", detail,
			[2]string{"", "Choose a new backup path."})
	case result.BackupPath == "":
		// Nothing was retained: backupSQLiteDatabase deletes its output when it cannot verify it.
		return ui.CommandFailed("Could not compact session data",
			"The session backup could not be verified.\n"+detail)
	default:
		return ui.CommandFailed("Could not compact session data",
			"The session database could not be prepared for compaction.\n"+detail,
			[2]string{"Backup:", result.BackupPath})
	}
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
	ApprovalRequired   bool     `json:"-"`
}

// sessionPolicyNetworkOf is the JSON projection AND, beside it, the compiled snapshot the human
// view reads. The projection's fields, names and digests are unchanged: a remote application
// verifies against them, so the human view takes the snapshot rather than widening the wire.
func sessionPolicyNetworkOf(cfg *config.Config, policy sessionsvc.Policy) (sessionPolicyNetwork, egress.Snapshot) {
	out := sessionPolicyNetwork{
		Mode: string(policy.Egress.Mode), ExportDestinations: policy.Egress.ExportDestinations,
	}
	if out.Mode == "" {
		out.Mode = "open (default)"
	}
	for _, rule := range policy.Egress.Rules {
		out.Rules = append(out.Rules, box.NetworkRuleText(rule))
	}
	resolved, snapshot, err := sessionsvc.ResolvePolicyNetworkSnapshot(cfg, policy)
	switch {
	case err != nil:
		out.Unresolved = err.Error()
		if resolved.Mode != "" {
			out.Mode = string(resolved.Mode)
		}
		var pending *networkstate.PendingApproval
		out.ApprovalRequired = errors.As(err, &pending)
	case resolved.Fingerprint != "":
		out.Mode, out.Fingerprint = string(resolved.Mode), resolved.Fingerprint
	default:
		out.Mode = string(resolved.Mode)
	}
	return out, snapshot
}

func runSessionPolicies(cfg *config.Config, policyPath string, jsonOutput bool) (int, error) {
	_, policyPath, _, err := sessionCLIPaths("", policyPath, "")
	if err != nil {
		return 2, err
	}
	policies, err := sessionsvc.LoadPolicies(policyPath, cfg)
	if err != nil {
		// The loader's own validation — ownership, writable ancestry, symlinks, limits, version,
		// names, repositories, accounts — keeps its cause; only the file is added in front of it.
		cause := policyPath + " " + err.Error()
		if strings.Contains(err.Error(), "must define at least one policy") {
			cause = policyPath + " must define at least one configuration."
		}
		return 1, ui.CommandFailed("Could not load remote session configurations", cause,
			[2]string{"Help:", "coop help sessions policies"})
	}
	result := sessionPoliciesResult{
		PolicyFile:             policyPath,
		PolicyDigests:          make(map[string]string, len(policies)),
		PolicyAuthorityDigests: make(map[string]string, len(policies)),
		PolicyNetworks:         make(map[string]sessionPolicyNetwork, len(policies)),
	}
	names := make([]string, 0, len(policies))
	snapshots := make(map[string]egress.Snapshot, len(policies))
	for name, policy := range policies {
		names = append(names, name)
		result.PolicyDigests[name] = sessionsvc.ResolvedPolicyDigest(policy)
		result.PolicyAuthorityDigests[name] = sessionsvc.ResolvedPolicyAuthorityDigest(policy)
		network, snapshot := sessionPolicyNetworkOf(cfg, policy)
		result.PolicyNetworks[name], snapshots[name] = network, snapshot
	}
	if jsonOutput { // the verification data a remote application reads — unchanged fields and digests
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return 1, err
		}
		return 0, nil
	}
	sort.Strings(names)
	views := make([]sessionConfigurationView, 0, len(names))
	for _, name := range names {
		views = append(views, sessionConfigurationViewOf(name, policies[name], result.PolicyNetworks[name], snapshots[name]))
	}
	renderSessionConfigurations(os.Stdout, ui.For(os.Stdout), tildeify(policyPath), views)
	return 0, nil
}

func runSessionServe(cfg *config.Config, state, policy, socket string) (int, error) {
	state, policy, socket, err := sessionCLIPaths(state, policy, socket)
	if err != nil {
		return 2, err
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if err := serveLocalSession(ctx, cfg, state, policy, socket, nil); err != nil {
		return 1, err
	}
	return 0, nil
}

// serveLocalSession runs the local session service until ctx ends: it owns the state root's lock,
// the policy file and the socket, and returns only when the server has shut down. listening, when
// given, is called once the socket is accepting — the caller still has to PROVE readiness before
// claiming it (see sessionServiceReady); binding a socket is not a promise to accept sessions.
// `coop sessions serve` and `coop sessions connect`'s autostart share this one body, so a service
// started either way is the same service.
func serveLocalSession(ctx context.Context, cfg *config.Config, state, policy, socket string, listening func()) error {
	if err := sessionsvc.EnsureAncestors(filepath.Dir(state)); err != nil {
		return err
	}
	// The optional `storage:` block in the same policy file. Absent, the daemon derives its limits
	// from the measured volume; present and incoherent, it refuses to start rather than discovering
	// the problem the first time the disk fills.
	storageLimits, configuredStorage, err := sessionsvc.LoadStorageLimits(policy)
	if err != nil {
		return err
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
		return err
	}
	defer service.Stop()
	if err := service.Start(ctx); err != nil {
		return err
	}
	listener, cleanup, err := sessionsvc.ListenSocket(state, socket)
	if err != nil {
		return err
	}
	defer cleanup()
	server := &http.Server{Handler: sessionsvc.NewHTTPHandler(service)}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	if listening != nil {
		listening()
	}
	select {
	case err := <-serveDone:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), sessionsvc.DefaultStopTimeout)
		shutdownErr := server.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			_ = server.Close()
			<-serveDone
			return shutdownErr
		}
		if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
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

// sessionReviewGateHost is the fork host a daemon's review gate runs on: the service's runtime,
// detected on demand, and a settle every filtered gate repeats — quietly, and never answered once
// for the process, because the daemon outlives any single answer.
func sessionReviewGateHost(cfg *config.Config, rt runtime.Runtime) forkctl.Host {
	return forkctl.Host{
		EnsureRuntime: func() (runtime.Runtime, error) {
			if rt.Name != "" {
				return rt, nil
			}
			return runtime.Detect(cfg.RuntimeName)
		},
		SettleFilteredRuns: func(gateRuntime runtime.Runtime) { settleNetworkRuns(gateRuntime) },
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
		fc := forkctl.New(cfg, rt, sessionReviewGateHost(cfg, rt))
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
	socketGiven := socket != "" // a named socket keeps its own remedy; the default's is the default service
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
	if jsonOutput { // the machine projection is unchanged, with no human header or footer around it
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return 1, err
		}
		if result.Error != "" || !result.Healthy || !result.Ready {
			return 1, nil
		}
		return 0, nil
	}
	if result.Error == "" && result.Healthy && result.Ready {
		ui.OK("Session service is ready")
		ui.Note("  %s", result.Socket)
		return 0, nil
	}
	return 1, sessionDoctorFailure(result, socketGiven, healthErr)
}

// sessionDoctorFailure names the ACTUAL reason the service could not be used. "Nothing is
// listening" is claimed only for a socket that refused the connection — a permission error, an
// unreadable socket or a malformed response each keeps its own cause. A service that answered but
// is not ready is a different failure from one that is not there. When the user named the socket,
// the remedy stays with THAT service: `coop sessions serve` alone would listen somewhere else.
func sessionDoctorFailure(result sessionDoctorResult, socketGiven bool, healthErr error) error {
	start := [2]string{"", "Run coop sessions serve to start it."}
	if socketGiven {
		start = [2]string{"", "Start the service that listens at " + result.Socket + "."}
	}
	switch {
	case healthErr == nil && !result.Ready:
		return ui.CommandFailed("Session service is not ready",
			"The service responded but cannot accept sessions yet.",
			[2]string{"", "Check the output of coop sessions serve."})
	case errors.Is(healthErr, syscall.ENOENT) || errors.Is(healthErr, syscall.ECONNREFUSED):
		return ui.CommandFailed("Session service is unavailable",
			"No service is listening at "+result.Socket+".", start)
	default:
		return ui.CommandFailed("Session service is unavailable", result.Error, start)
	}
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
