// coop-net runs only inside the trusted gateway image. It is not an agent tool
// or a second host CLI. Every path and executable is fixed by the release.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/AndrewDryga/coop/internal/networkgateway"
)

func main() {
	if err := run(); err != nil {
		// Gateway failures are fixed reason codes; parser/process errors must
		// not leak private configuration or provider data through Docker logs.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return networkgateway.Failure("gateway_role_invalid")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// An open run's broker helper has no gateway launch configuration, only its routes.
	switch os.Args[1] {
	case "broker":
		return networkgateway.RunOpenBroker(ctx, os.Stdin, os.Stdout)
	}
	file, err := os.Open(networkgateway.LaunchConfigPath)
	if err != nil {
		return networkgateway.Failure("gateway_configuration_invalid")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return networkgateway.Failure("gateway_configuration_invalid")
	}
	config, err := networkgateway.ReadLaunchConfig(file)
	if err != nil {
		return err
	}
	switch os.Args[1] {
	case "controller":
		return networkgateway.RunController(ctx, config)
	case "guard":
		gateway, err := networkgateway.NewGuardRuntime(config)
		if err != nil {
			return err
		}
		return gateway.Run(ctx)
	case "probe":
		return networkgateway.ProbeRuntime(ctx, config)
	case "snapshot":
		value, err := networkgateway.ObserveRuntime(ctx, config)
		if err != nil {
			return err
		}
		if json.NewEncoder(os.Stdout).Encode(value) != nil {
			return networkgateway.Failure("observation_unavailable")
		}
		return nil
	default:
		return networkgateway.Failure("gateway_role_invalid")
	}
}
