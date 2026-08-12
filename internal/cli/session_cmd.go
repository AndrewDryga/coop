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
	if state == "" {
		var err error
		state, err = defaultSessionStateRoot()
		if err != nil {
			return "", "", "", err
		}
	}
	if policy == "" {
		var err error
		policy, err = defaultSessionPolicyPath()
		if err != nil {
			return "", "", "", err
		}
	}
	state, err := filepath.Abs(filepath.Clean(state))
	if err != nil {
		return "", "", "", fmt.Errorf("resolve session state path: %w", err)
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

func parseSessionsFlags(args []string, command string) (state, policy, socket string, jsonOutput bool, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--json" && command == "doctor" {
			if jsonOutput {
				return "", "", "", false, errors.New("sessions doctor: --json may be specified once")
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
	if command == "doctor" && (state != "" || policy != "") {
		return "", "", "", false, errors.New("sessions doctor: only --socket and --json are supported")
	}
	return state, policy, socket, jsonOutput, nil
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
		return runSessionServe(state, policy, socket)
	case "doctor":
		_, _, socket, jsonOutput, err := parseSessionsFlags(args[1:], "doctor")
		if err != nil {
			return 2, err
		}
		return runSessionDoctor(socket, jsonOutput)
	default:
		return 2, fmt.Errorf("sessions: unknown command %q", args[0])
	}
}

func runSessionServe(state, policy, socket string) (int, error) {
	state, policy, socket, err := sessionCLIPaths(state, policy, socket)
	if err != nil {
		return 2, err
	}
	if err := sessionsvc.EnsureAncestors(filepath.Dir(state)); err != nil {
		return 1, err
	}
	service, err := sessionsvc.NewService(sessionsvc.Config{
		StateRoot: state, PolicyPath: policy, SourceConfig: config.Load(), Executable: os.Args[0],
		Host: sessionHost(), Logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	})
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
