package cli

// `coop sessions connect` — the one command a person runs to put this machine's sessions at a
// remote controller's disposal. Two internals stay separate underneath: the LOCAL SERVICE owns
// session storage, its socket and its process lifetime; the CONNECTOR maps that
// service's owner-private Unix API onto one outbound mutual-TLS poll stream. This command wires
// them together without making anyone run two terminals, and it owns only what it started.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/ui"
	"github.com/AndrewDryga/coop/internal/workerconnector"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// sessionReadyWait bounds how long autostart waits for a service it started to accept sessions,
// and sessionReadyPoll how often it asks. Vars so a test can drive both without a real wait.
var (
	sessionReadyWait = 30 * time.Second
	sessionReadyPoll = 50 * time.Millisecond
)

type sessionConnectOptions struct {
	Controller string
	TokenFile  string
	State      string
	CAFile     string
}

func parseSessionConnectFlags(args []string) (sessionConnectOptions, error) {
	const command = "coop sessions connect"
	const usage = command + " --controller <https-url> --token-file <path>"
	var options sessionConnectOptions
	for i := 0; i < len(args); i++ {
		var target *string
		switch args[i] {
		case "--controller":
			target = &options.Controller
		case "--token-file":
			target = &options.TokenFile
		case "--state":
			target = &options.State
		case "--ca-file":
			target = &options.CAFile
		default:
			if strings.HasPrefix(args[i], "-") {
				return options, ui.UnknownOption(args[i], command, "")
			}
			return options, ui.UnexpectedArgument(args[i], command, usage)
		}
		if *target != "" {
			return options, fmt.Errorf("%s: %s may be specified once", command, args[i])
		}
		if i+1 == len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
			return options, ui.MissingOptionValue(args[i], command, usage)
		}
		i++
		*target = args[i]
	}
	if options.Controller == "" {
		return options, ui.MissingOptionValue("--controller", command, usage)
	}
	for _, path := range []*string{&options.TokenFile, &options.CAFile} {
		if *path != "" {
			resolved, err := filepath.Abs(*path)
			if err != nil {
				return options, err
			}
			*path = resolved
		}
	}
	var err error
	options.State, err = sessionStatePath(options.State)
	return options, err
}

func runSessionConnect(cfg *config.Config, options sessionConnectOptions) (int, error) {
	state, socket, err := sessionSocketPath(options.State, "")
	if err != nil {
		return 1, err
	}
	transport, err := workerconnector.NewHTTPTransport(workerconnector.HTTPTransportConfig{
		BaseURL: options.Controller, CAFile: options.CAFile, EnrollmentTokenFile: options.TokenFile,
		IdentityFile: filepath.Join(state, "identity.json"),
	})
	if err != nil {
		return 1, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	jobSources, err := workerconnector.NewJobSourceStager(transport, state)
	if err != nil {
		return 1, err
	}
	owned, err := startLocalSessionService(ctx, cfg, state, socket, jobSources.RefreshDefault, transport.PublishReview)
	if err != nil {
		return 1, err
	}
	defer owned.stop()
	// Own the state root before enrollment can publish or replace a worker identity.
	ui.Note("Connecting to %s…", options.Controller)
	identity, err := transport.Identity(ctx)
	if err != nil {
		return 1, err
	}
	ui.Pass("Worker identity ready: %s", identity.ID)
	privateAPI, err := workerconnector.NewUnixAPI(socket, time.Minute)
	if err != nil {
		return 1, err
	}
	executor, err := workerconnector.NewExecutor(workerconnector.ExecutorConfig{
		API: privateAPI, BodyTransport: transport, JobSourceStager: jobSources,
		JournalDir: filepath.Join(state, "connector"), Now: time.Now, WorkerID: identity.ID,
	})
	if err != nil {
		return 1, err
	}
	sandbox := sha256.Sum256([]byte(cfg.BaseImage))
	hello := func(ctx context.Context, clock time.Time) workerproto.WorkerHello {
		capacity := workerconnector.LiveCapacity(ctx, privateAPI)
		return workerproto.WorkerHello{
			ID: identity.ID, WorkspaceRef: identity.WorkspaceRef, ProtocolVersion: "2",
			BuildVersion: resolveVersion(), ClockAt: clock.UTC(), State: capacity.State,
			SandboxDigest: hex.EncodeToString(sandbox[:]),
			Capabilities:  workerconnector.LiveCapabilities(ctx, privateAPI),
			Storage:       workerconnector.LiveStorage(ctx, privateAPI),
			Capacity:      capacity,
		}
	}
	runner, err := workerconnector.NewConnector(workerconnector.ConnectorConfig{
		Executor: executor, Hello: hello, Now: time.Now, Transport: transport,
	})
	if err != nil {
		return 1, err
	}
	err = runner.Run(ctx, time.Second, func(err error) { ui.Warn("%v", err) })
	if errors.Is(err, context.Canceled) {
		return 0, nil
	}
	return 1, err
}

// ownedSessionService is a local service THIS invocation started, and the only one it may stop.
type ownedSessionService struct {
	cancel context.CancelFunc
	done   <-chan error
}

func (o *ownedSessionService) stop() {
	o.cancel()
	<-o.done
}

// Each connection owns its private service and its controller credentials. A live service is
// never reused: it may belong to a different controller, and only its owner can stop it.
func startLocalSessionService(ctx context.Context, cfg *config.Config, state, socket string, refresh sessionsvc.SourceRefresher, publish sessionsvc.ReviewPublisher) (*ownedSessionService, error) {
	if _, listening := sessionServiceReady(socket); listening {
		return nil, fmt.Errorf("a session connection is already listening at %s; stop that connection or choose a different --state directory", socket)
	}
	ui.Note("  Starting the local session service…")
	serviceCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan error, 1)
	listening := make(chan struct{})
	go func() {
		done <- serveLocalSession(serviceCtx, cfg, state, socket, refresh, publish, func() { close(listening) })
	}()
	owned := &ownedSessionService{cancel: cancel, done: done}

	deadline := time.Now().Add(sessionReadyWait)
	ownsListener := false
	for {
		if ownsListener {
			if ready, _ := sessionServiceReady(socket); ready {
				ui.Pass("Local session service ready")
				return owned, nil
			}
		}
		select {
		case err := <-done: // it stopped before it was ready — its own error is the whole story
			cancel()
			if err == nil {
				return nil, errors.New("the local session service stopped before it was ready")
			}
			return nil, err
		case <-listening:
			ownsListener = true
			listening = nil
		case <-ctx.Done():
			owned.stop()
			return nil, ctx.Err()
		case <-time.After(sessionReadyPoll):
		}
		if time.Now().After(deadline) {
			owned.stop()
			return nil, fmt.Errorf("the local session service did not become ready within %s", sessionReadyWait)
		}
	}
}

// sessionServiceReady asks the service the same question `coop sessions doctor` asks, over the
// same socket: ready is a PROVED /readyz answer, and listening distinguishes a service that
// answered something from a socket nothing is serving.
func sessionServiceReady(socket string) (ready, listening bool) {
	client := sessionUnixHTTPClient(socket)
	var health sessionDoctorResult
	if err := sessionDoctorGet(client, "/healthz", &health); err != nil {
		return false, sessionSocketLive(socket)
	}
	var state sessionReadyDTO
	if err := sessionDoctorGet(client, "/readyz", &state); err != nil {
		return false, true
	}
	return state.Ready, true
}

// sessionSocketLive reports whether something is accepting on the socket. A socket file that
// refuses connections is a leftover, not a service; anything else — a permission error, a socket
// that accepts but speaks nonsense — is live and must not be disturbed.
func sessionSocketLive(socket string) bool {
	info, err := os.Lstat(socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return false
	}
	client := sessionUnixHTTPClient(socket)
	err = sessionDoctorGet(client, "/healthz", &struct{}{})
	return err == nil || !errors.Is(err, syscall.ECONNREFUSED)
}
