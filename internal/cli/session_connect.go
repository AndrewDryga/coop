package cli

// `coop sessions connect` — the one command a person runs to put this machine's sessions at a
// remote controller's disposal. Two internals stay separate underneath: the LOCAL SERVICE owns
// session storage, the policy file, its socket and its process lifetime; the CONNECTOR maps that
// service's owner-private Unix API onto one outbound mutual-TLS poll stream. This command wires
// them together without making anyone run two terminals, and it owns only what it started.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
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

func parseSessionConnectFlags(args []string) (string, error) {
	if len(args) != 2 || args[0] != "--config" || args[1] == "" {
		return "", ui.MissingOptionValue("--config", "coop sessions connect", "coop sessions connect --config /path/to/worker.json")
	}
	// A relative path is resolved here, so nobody has to type an absolute one: the loader below
	// still requires the absolute form, because everything it reads is owner-private.
	path, err := filepath.Abs(filepath.Clean(args[1]))
	if err != nil {
		return "", fmt.Errorf("resolve worker configuration: %w", err)
	}
	return path, nil
}

func runSessionConnect(cfg *config.Config, configurationPath string) (int, error) {
	// The configuration is validated FIRST: a bad file must never start a service, and the paths
	// it names decide which service this machine is even allowed to reuse.
	configuration, err := workerconnector.LoadConfig(configurationPath, resolveVersion(), time.Now().UTC())
	if err != nil {
		return 1, sessionConnectFailure(configurationPath, err)
	}
	state, policy, socket, err := sessionConnectPaths(configuration)
	if err != nil {
		return 1, sessionConnectFailure(configurationPath, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ui.Note("%s", ui.Bold("Connecting to the remote controller"))
	owned, err := ensureLocalSessionService(ctx, cfg, state, policy, socket)
	if err != nil {
		return 1, err
	}
	if owned != nil {
		// Ctrl-C stops the connector and, only because this invocation created it, this service.
		// A service that was already running is left exactly as it was found.
		defer owned.stop()
	}
	ui.Note("  Connecting to %s…", configuration.ResponderURL)

	privateAPI, err := workerconnector.NewUnixAPI(socket, configuration.RequestTimeout)
	if err != nil {
		return 1, err
	}
	transport, err := workerconnector.NewHTTPTransport(workerconnector.HTTPTransportConfig{
		BaseURL: configuration.ResponderURL, CAFile: configuration.CAFile,
		EnrollmentTokenFile: configuration.EnrollmentTokenFile,
		IdentityFile:        configuration.IdentityFile, RenewBefore: configuration.RenewBefore,
		Timeout: configuration.RequestTimeout, WorkerID: configuration.Hello.ID,
		WorkspaceRef: configuration.Hello.WorkspaceRef,
	})
	if err != nil {
		return 1, err
	}
	executor, err := workerconnector.NewExecutor(workerconnector.ExecutorConfig{
		API: privateAPI, ArtifactTransport: transport, JournalDir: configuration.JournalDir, Now: time.Now,
		WorkerID: configuration.Hello.ID,
	})
	if err != nil {
		return 1, err
	}
	hello := func(ctx context.Context, clock time.Time) workerproto.WorkerHello {
		current := configuration.Hello
		current.ClockAt = clock.UTC()
		current.Capabilities = workerconnector.LiveCapabilities(ctx, privateAPI, current.Capabilities)
		// The daemon owns the storage measurement and the allocation decision behind it; the
		// connector carries whichever one it can read right now, and nothing when it cannot.
		current.Storage = workerconnector.LiveStorage(ctx, privateAPI)
		return current
	}
	runner, err := workerconnector.NewConnector(workerconnector.ConnectorConfig{
		Executor: executor, Hello: hello, Now: time.Now, Transport: transport,
	})
	if err != nil {
		return 1, err
	}
	// A healthy connection says nothing: the connector has no success callback, and a spawned
	// process is not a registration. Only failures are narrated, each with its retry promise.
	err = runner.Run(ctx, configuration.PollInterval, func(err error) { ui.Warn("%v", err) })
	if errors.Is(err, context.Canceled) {
		return 0, nil
	}
	return 1, err
}

// sessionConnectPaths resolves which local service this configuration is about: the state root and
// policy file it names (or the documented defaults), and the socket. A configured `coop_socket`
// must AGREE with the resolved state root — the socket is the service's front door, and pointing
// it at a service that stores its sessions somewhere else would connect a controller to a machine
// whose policies nobody checked. The state root is never inferred from an arbitrary socket parent.
func sessionConnectPaths(configuration workerconnector.Config) (state, policy, socket string, err error) {
	state, policy, defaultSocket, err := sessionCLIPaths(configuration.SessionStateDir, configuration.SessionPolicyPath, "")
	if err != nil {
		return "", "", "", err
	}
	socket = defaultSocket
	if configuration.CoopSocket != "" {
		socket, err = filepath.Abs(filepath.Clean(configuration.CoopSocket))
		if err != nil {
			return "", "", "", fmt.Errorf("resolve session socket path: %w", err)
		}
		if !sessionSocketUnder(state, socket) {
			return "", "", "", fmt.Errorf("coop_socket %s is not inside the session data directory %s", socket, state)
		}
	}
	return state, policy, socket, nil
}

func sessionSocketUnder(state, socket string) bool {
	rel, err := filepath.Rel(state, socket)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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

// ensureLocalSessionService reuses a ready local service or starts one, and returns non-nil only
// for a service it started. A service that is listening but not ready is NOT absent: its socket is
// left alone and no second service is started, because unlinking a live socket would cut off
// whoever is already talking to it. A race on the state root resolves through the storage lock —
// the loser re-checks readiness and reuses the winner.
func ensureLocalSessionService(ctx context.Context, cfg *config.Config, state, policy, socket string) (*ownedSessionService, error) {
	if ready, listening := sessionServiceReady(socket); ready {
		ui.Note("  Using the running local session service")
		return nil, nil
	} else if listening {
		return nil, fmt.Errorf("a local session service is already listening at %s but is not ready for sessions.\n\nCheck the output of that service, or run coop sessions doctor", socket)
	}
	ui.Note("  Starting the local session service…")
	serviceCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan error, 1)
	go func() { done <- serveLocalSession(serviceCtx, cfg, state, policy, socket, nil) }()
	owned := &ownedSessionService{cancel: cancel, done: done}

	deadline := time.Now().Add(sessionReadyWait)
	for {
		if ready, _ := sessionServiceReady(socket); ready {
			ui.Pass("Local session service ready")
			return owned, nil
		}
		select {
		case err := <-done: // it stopped before it was ready — its own error is the whole story
			cancel()
			if err == nil {
				return nil, errors.New("the local session service stopped before it was ready")
			}
			// Another process won the state root between the probe and the start. That is not a
			// failure: re-check readiness and use the service that won.
			if strings.Contains(err.Error(), "owns this state root") {
				if ready, _ := sessionServiceReady(socket); ready {
					ui.Note("  Using the running local session service")
					return nil, nil
				}
			}
			return nil, err
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

// sessionConnectFailure is the shape every rejected configuration shares: what could not start,
// the file and the loader's OWN bounded cause under it, and the page that explains the file.
func sessionConnectFailure(path string, err error) error {
	return ui.CommandFailed("Could not start the worker", path+" could not be used:\n"+err.Error(),
		[2]string{"Help:", "coop help sessions connect"})
}
