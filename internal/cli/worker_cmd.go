package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/workerconnector"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func (a *app) cmdWorker(args []string) (int, error) {
	if len(args) == 0 {
		return groupHelp("worker")
	}
	if args[0] != "connect" {
		return 2, fmt.Errorf("worker: unknown command %q", args[0])
	}
	configurationPath, err := parseWorkerConnectFlags(args[1:])
	if err != nil {
		return 2, err
	}
	return runWorkerConnect(configurationPath)
}

func parseWorkerConnectFlags(args []string) (string, error) {
	if len(args) != 2 || args[0] != "--config" || args[1] == "" {
		return "", errors.New("usage: coop worker connect --config <absolute-path>")
	}
	path, err := filepath.Abs(filepath.Clean(args[1]))
	if err != nil {
		return "", fmt.Errorf("resolve worker configuration: %w", err)
	}
	return path, nil
}

func runWorkerConnect(configurationPath string) (int, error) {
	configuration, err := workerconnector.LoadConfig(configurationPath, resolveVersion(), time.Now().UTC())
	if err != nil {
		return 1, err
	}
	privateAPI, err := workerconnector.NewUnixAPI(configuration.CoopSocket, configuration.RequestTimeout)
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
	hello := func(clock time.Time) workerproto.WorkerHello {
		current := configuration.Hello
		current.ClockAt = clock.UTC()
		return current
	}
	connector, err := workerconnector.NewConnector(workerconnector.ConnectorConfig{
		Executor: executor, Hello: hello, Now: time.Now, Transport: transport,
	})
	if err != nil {
		return 1, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = connector.Run(ctx, configuration.PollInterval, func(err error) {
		fmt.Fprintf(os.Stderr, "coop worker: %v\n", err)
	})
	if errors.Is(err, context.Canceled) {
		return 0, nil
	}
	return 1, err
}
