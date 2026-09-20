package networkgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
)

const (
	// OpenBrokerConfigPath is an open run's broker helper configuration: its routes, never a
	// credential. The secrets ride CredentialBrokerPath, as in a filtered guard.
	OpenBrokerConfigPath = "/run/coop-open-broker.json"
	// OpenBrokerHost is the name an open box reaches its helper by. The box's MCP configuration is
	// written before the helper has an address, and the box's own hosts entry binds the name to it;
	// every request must name the listener this way.
	OpenBrokerHost = "coop-broker"
)

// OpenBrokerConfig is the non-secret half of an open run's broker: the run and helper generation
// its secrets are bound to, and its routes in listener order.
type OpenBrokerConfig struct {
	Version int                     `json:"version"`
	RunID   string                  `json:"run_id"`
	Epoch   string                  `json:"gateway_epoch"`
	Brokers []CredentialBrokerRoute `json:"credential_brokers"`
}

// Validate holds an open helper to MCP routes only: its box's traffic passes no gateway, so a
// provider key is kept to filtered runs, where the gateway holds the agent to its route.
func (c OpenBrokerConfig) Validate() error {
	if c.Version != 1 || !lowerHex(c.RunID, 32) || !lowerHex(c.Epoch, 32) || len(c.Brokers) == 0 || !validBrokerRoutes(c.Brokers, nil, nil) {
		return Failure("credential_broker_configuration_invalid")
	}
	for _, route := range c.Brokers {
		// Both MCP transports, and nothing else: a provider key stays a filtered run's, and a
		// download route belongs to a client the gateway itself serves.
		if route.Kind != CredentialBrokerMCP && route.Kind != CredentialBrokerMCPSSE {
			return Failure("credential_broker_configuration_invalid")
		}
	}
	return nil
}

func ReadOpenBrokerConfig(reader io.Reader) (OpenBrokerConfig, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxLaunchConfigBytes+1))
	if err != nil || len(data) > MaxLaunchConfigBytes {
		return OpenBrokerConfig{}, Failure("credential_broker_configuration_invalid")
	}
	var value OpenBrokerConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Validate() != nil {
		return OpenBrokerConfig{}, Failure("credential_broker_configuration_invalid")
	}
	return value, nil
}

// openBrokerHost is the helper's one address on the network it shares with its box: where the box
// reaches it, and so the Host every request must carry. More than one would leave that ambiguous.
func openBrokerHost() (netip.Addr, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return netip.Addr{}, Failure("credential_broker_address_unavailable")
	}
	var addresses []net.Addr
	for _, network := range interfaces {
		if network.Flags&net.FlagUp == 0 || network.Flags&net.FlagLoopback != 0 {
			continue
		}
		found, err := network.Addrs()
		if err != nil {
			return netip.Addr{}, Failure("credential_broker_address_unavailable")
		}
		addresses = append(addresses, found...)
	}
	return oneIPv4(addresses)
}

func oneIPv4(addresses []net.Addr) (netip.Addr, error) {
	var found []netip.Addr
	for _, address := range addresses {
		network, ok := address.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(network.IP)
		if ip = ip.Unmap(); ok && ip.Is4() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			found = append(found, ip)
		}
	}
	if len(found) != 1 {
		return netip.Addr{}, Failure("credential_broker_address_unavailable")
	}
	return found[0], nil
}

// RunOpenBroker serves an open run's routes on the helper's own address, prints that address once
// every listener accepts, and stops when its stdin closes — the Coop that started it is gone,
// however it ended — or when ctx ends.
func RunOpenBroker(ctx context.Context, in io.Reader, out io.Writer) error {
	if verifyServiceRole("broker") != nil {
		return Failure("gateway_role_invalid")
	}
	config, err := readOpenBrokerConfigFile()
	if err != nil {
		return err
	}
	host, err := openBrokerHost()
	if err != nil {
		return err
	}
	file, err := os.Open(CredentialBrokerPath)
	if err != nil {
		return Failure("credential_broker_configuration_invalid")
	}
	info, statErr := file.Stat()
	secrets, readErr := readBrokerSecrets(file, config.RunID, config.Epoch, config.Brokers)
	_ = file.Close()
	if statErr != nil || !info.Mode().IsRegular() || readErr != nil {
		return Failure("credential_broker_configuration_invalid")
	}
	brokers := make([]*credentialBroker, len(config.Brokers))
	for i, secret := range secrets.Routes {
		if brokers[i], err = newOpenCredentialBroker(config.Brokers[i], host, i, secret); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if in != nil {
		go func() {
			_, _ = io.Copy(io.Discard, in)
			cancel()
		}()
	}
	return serveOpenBrokers(ctx, brokers, func() { _, _ = fmt.Fprintln(out, host) })
}

// serveOpenBrokers runs every listener until ctx ends or one fails, calling ready once they all
// accept. A stop is not a failure.
func serveOpenBrokers(ctx context.Context, brokers []*credentialBroker, ready func()) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	failures := make(chan error, len(brokers))
	var accepting atomic.Int32
	listening := func() {
		if int(accepting.Add(1)) == len(brokers) && ready != nil {
			ready()
		}
	}
	for _, broker := range brokers {
		workers.Go(func() { failures <- broker.Serve(ctx, listening) })
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-failures:
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
}

func readOpenBrokerConfigFile() (OpenBrokerConfig, error) {
	file, err := os.Open(OpenBrokerConfigPath)
	if err != nil {
		return OpenBrokerConfig{}, Failure("credential_broker_configuration_invalid")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return OpenBrokerConfig{}, Failure("credential_broker_configuration_invalid")
	}
	return ReadOpenBrokerConfig(file)
}
