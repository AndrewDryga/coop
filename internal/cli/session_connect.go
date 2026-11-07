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
	// Only the first connection needs the token, as the help says.
	const usage = command + " --controller <https-url> [--token-file <path>]"
	const example = command + " --controller https://controller.example.com --token-file /path/to/token"
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
			return options, ui.RepeatedOption(args[i], command)
		}
		if i+1 == len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
			return options, ui.MissingOptionValue(args[i], command, example)
		}
		i++
		*target = args[i]
	}
	if options.Controller == "" {
		return options, ui.MissingArgument("--controller", command, usage)
	}
	// Refused here, before a local service starts. The address is not echoed: one with a user name
	// can carry a password.
	if workerconnector.CheckControlPlaneURL(options.Controller) != nil {
		return options, &ui.UsageError{
			Headline: fmt.Sprintf("Invalid value for %q in %q", "--controller", command),
			Cause:    "Use the controller's https:// address, such as https://controller.example.com.\nLeave out any path, query or user name.",
			Rows: [][2]string{
				{"Example:", example},
				{"Help:", ui.HelpCommand(command)},
			},
		}
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

// tokenFileRefusal is the block for a token file the worker cannot use: the check it failed, and
// what fixes it, as a command when one does.
func tokenFileRefusal(err error) error {
	cause := "Coop found: " + err.Error() + "."
	var rows [][2]string
	var unusable *workerconnector.TokenFileError
	if errors.As(err, &unusable) {
		cause = sentence(unusable.Problem)
		if unusable.Advice != "" {
			cause += "\n" + unusable.Advice
		}
		if unusable.Shared {
			if word, ok := ui.ShellWord(unusable.Path); ok {
				rows = append(rows, [2]string{"Make it private:", "chmod 600 " + word})
			} else {
				cause += "\nMake it private with chmod 600."
			}
		}
	}
	rows = append(rows, [2]string{"Help:", ui.HelpCommand("coop sessions connect")})
	return ui.CommandFailed("Could not use the enrollment token file", cause, rows...)
}

// identityRefusal is the block for a controller that will not give this worker an identity: it
// refused the enrollment token, or its certificate comes from an authority this machine does not
// trust. Both stay that way however long the worker waits. Any other failure keeps its own text.
func identityRefusal(err error, options sessionConnectOptions) error {
	help := [2]string{"Help:", ui.HelpCommand("coop sessions connect")}
	var status *workerconnector.IdentityStatusError
	switch {
	case errors.As(err, &status) && status.TokenRefused():
		return ui.CommandFailed("The controller refused the enrollment token",
			fmt.Sprintf("It answered HTTP %d to the enrollment request.\n", status.Status)+
				"Get a new token from the controller and save it in the file you gave --token-file.", help)
	case workerconnector.ControllerCertificateUntrusted(err):
		store := "the system trust store"
		if options.CAFile != "" {
			store = ui.SafeInline(options.CAFile) // --ca-file replaces the system roots, so the file is the whole store
		}
		return ui.CommandFailed("This machine does not trust the controller's certificate",
			"The authority that signed it is not in "+store+".\n"+
				"If you trust that authority, give --ca-file its certificate file.", help)
	}
	return err
}

// firstConnectionRefusal refuses, before anything starts or is contacted, a connection that has to
// enroll but has no usable token: a state folder holding no saved identity enrolls, and enrolling
// needs one. A saved identity needs none, since enrolling removed the token, so a service restarting
// with the same flag or without it is fine.
func firstConnectionRefusal(state string, options sessionConnectOptions) error {
	if _, err := os.Lstat(filepath.Join(state, "identity.json")); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if options.TokenFile == "" {
		return noTokenRefusal(state, options)
	}
	if err := workerconnector.CheckEnrollmentTokenFile(options.TokenFile); err != nil {
		return tokenFileRefusal(err)
	}
	return nil
}

// noTokenRefusal is the block for a connection with no saved identity and no --token-file. It shows
// this command with a token file added, so it keeps the --state and --ca-file it was given: without
// them the command would enroll in another folder, or fail the certificate check. The address is
// one the transport already accepted, so it has no user name to leak.
func noTokenRefusal(state string, options sessionConnectOptions) error {
	command := "coop sessions connect --controller " + shellWord(options.Controller)
	if options.CAFile != "" {
		command += " --ca-file " + shellWord(options.CAFile)
	}
	if def, err := sessionStatePath(""); err != nil || state != def {
		command += " --state " + shellWord(state)
	}
	return ui.CommandFailed("A first connection needs an enrollment token",
		"No worker identity is saved in "+ui.SafeInline(state)+".\n"+
			"Save the token the controller gave you in a file and pass it with --token-file.",
		[2]string{"Example:", command + " --token-file /path/to/token"},
		[2]string{"Help:", ui.HelpCommand("coop sessions connect")})
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
	// Only after the transport has accepted the address, which a refusal may repeat in its command.
	if err := firstConnectionRefusal(state, options); err != nil {
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
	identity, err := waitForIdentity(ctx, transport.Identity, ui.Warn, time.After)
	if errors.Is(err, context.Canceled) {
		return 0, nil
	}
	if err != nil {
		return 1, identityRefusal(err, options)
	}
	ui.Pass("Worker identity ready: %s", identity.ID)
	privateAPI, err := workerconnector.NewUnixAPI(socket, time.Minute)
	if err != nil {
		return 1, err
	}
	executor, err := workerconnector.NewExecutor(workerconnector.ExecutorConfig{
		API: privateAPI, BodyTransport: transport, JobSourceStager: jobSources,
		JournalDir: connectorJournalDir(state), Now: time.Now, WorkerID: identity.ID,
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

// waitForIdentity keeps a first connection going while the controller cannot be reached, as the
// connector does afterwards, so a worker can start before its controller. Everything else, such as a
// refused token or a certificate it does not trust, stops the command: waiting cannot fix those.
func waitForIdentity(ctx context.Context, identity func(context.Context) (workerconnector.WorkerIdentity, error),
	warn func(string, ...any), after func(time.Duration) <-chan time.Time) (workerconnector.WorkerIdentity, error) {
	wait := 2 * time.Second
	for {
		id, err := identity(ctx)
		if err == nil || !workerconnector.ControllerUnreachable(err) || ctx.Err() != nil {
			return id, err
		}
		warn("Could not reach the controller (%s). Trying again in %s.", ui.SafeInline(err.Error()), wait)
		select {
		case <-ctx.Done():
			return id, ctx.Err()
		case <-after(wait):
		}
		wait = min(wait*2, 30*time.Second)
	}
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
