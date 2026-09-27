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
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkctl"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/ui"
	"github.com/AndrewDryga/coop/internal/workerconnector"
)

func defaultSessionStateRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".local", "state", "coop", "sessions"), nil
}

func sessionSocketPath(state, socket string) (string, string, error) {
	state, err := sessionStatePath(state)
	if err != nil {
		return "", "", err
	}
	if socket == "" {
		socket = filepath.Join(state, "control.sock")
	}
	socket, err = filepath.Abs(filepath.Clean(socket))
	return state, socket, err
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

func parseSessionDoctorFlags(args []string) (socket string, jsonOutput bool, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			if jsonOutput {
				return "", false, errors.New("sessions doctor: --json may be specified once")
			}
			jsonOutput = true
		case "--socket":
			if socket != "" || i+1 == len(args) || strings.HasPrefix(args[i+1], "-") {
				return "", false, errors.New("sessions doctor: --socket requires one value")
			}
			i++
			socket = args[i]
		default:
			return "", false, fmt.Errorf("sessions doctor: unknown flag %q", args[i])
		}
	}
	return socket, jsonOutput, nil
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
var sessionCommands = []string{"connect", "doctor", "compact"}

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
	case "doctor":
		socket, jsonOutput, err := parseSessionDoctorFlags(args[1:])
		if err != nil {
			return 2, err
		}
		return runSessionDoctor(socket, jsonOutput)
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

// serveLocalSession is the private API owned by the controller connection.
func serveLocalSession(ctx context.Context, cfg *config.Config, state, socket string, refresh sessionsvc.SourceRefresher, publish sessionsvc.ReviewPublisher, listening func()) error {
	if err := sessionsvc.EnsureAncestors(filepath.Dir(state)); err != nil {
		return err
	}
	serviceConfig := sessionsvc.Config{
		StateRoot: state, SourceConfig: cfg, Executable: os.Args[0],
		SourceRefresher: refresh,
		ReviewPublisher: publish,
		Host:            sessionHost(), Logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	}
	service, err := sessionsvc.NewService(serviceConfig)
	if err != nil {
		return err
	}
	defer service.Stop()
	if err := workerconnector.ReclaimInterruptedTransfers(state); err != nil {
		return err
	}
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
		ReviewGateFactory: defaultSessionReviewGate,
		Warnf:             ui.Warn,
	}
}

// sessionReviewGateHost is the fork host a daemon's review gate runs on: the service's runtime,
// detected on demand, and a settle every filtered gate repeats — quietly, and never answered once
// for the process, because the daemon outlives any single answer.
func sessionReviewGateHost(cfg *config.Config, rt runtime.Runtime) forkctl.Host {
	ensureRuntime := func() (runtime.Runtime, error) {
		if rt.Name != "" {
			return rt, nil
		}
		return runtime.Detect(cfg.RuntimeName)
	}
	return forkctl.Host{
		EnsureRuntime:      ensureRuntime,
		SettleFilteredRuns: func(gateRuntime runtime.Runtime) { settleNetworkRuns(gateRuntime) },
		// An upgrade's new base is built as a launch builds it, output to the daemon's log.
		EnsureBaseImage: func() error {
			gateRuntime, err := ensureRuntime()
			if err != nil {
				return err
			}
			if _, _, ok := box.ManagedBaseRepair(gateRuntime, cfg); !ok {
				return nil
			}
			return box.BuildManagedBase(gateRuntime, cfg, resolveVersion(), os.Stderr)
		},
	}
}

// defaultSessionReviewGate runs a review candidate through THIS repo's merge gate — the same pass/fail
// rule `coop fork merge` uses, so a session's verdict can't drift from the one a human gets — with the
// job's ordinary image (forkctl.JobGate), never a tag named after the job repository. The config and
// runtime are the service's, resolved by the time it asks; a runtime it hasn't detected yet is the
// zero value, which the control plane detects on demand.
func defaultSessionReviewGate(cfg *config.Config, rt runtime.Runtime) sessionsvc.ReviewGate {
	return sessionsvc.ReviewGateFunc(func(ctx context.Context, gateRepo, treeDir string) (sessionsvc.ReviewGateResult, error) {
		if err := ctx.Err(); err != nil {
			return sessionsvc.ReviewGateResult{}, err
		}
		fc := forkctl.New(cfg, rt, sessionReviewGateHost(cfg, rt))
		image, err := fc.JobGate(gateRepo)
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
	_, socket, err := sessionSocketPath("", socket)
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
// the remedy stays with THAT service: `coop sessions connect` alone would listen somewhere else.
func sessionDoctorFailure(result sessionDoctorResult, socketGiven bool, healthErr error) error {
	start := [2]string{"", "Run coop sessions connect to start it."}
	if socketGiven {
		start = [2]string{"", "Start the service that listens at " + result.Socket + "."}
	}
	switch {
	case healthErr == nil && !result.Ready:
		return ui.CommandFailed("Session service is not ready",
			"The service responded but cannot accept sessions yet.",
			[2]string{"", "Check the output of coop sessions connect."})
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
