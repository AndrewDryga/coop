package networkgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

const (
	LaunchConfigPath        = "/run/coop-network.json"
	ControllerSocket        = "/ipc/controller/c.sock"
	EnvoyAdminSocket        = "/private/admin.sock"
	RuntimeSocket           = "/private/runtime.sock"
	EnvoyHealthTimeout      = 750 * time.Millisecond
	EnvoyStartTimeout       = 10 * time.Second
	EnvoyStopTimeout        = 2 * time.Second
	MaxLaunchConfigBytes    = 4 << 20
	MaintenanceResolver     = "1.1.1.1"
	MaintenanceResolverName = "cloudflare-dns.com"
	CredentialBrokerPort    = 15580
	CredentialBrokerAddress = "127.0.0.1:15580"
	CredentialBrokerPath    = "/run/coop-credential-broker.json"
)

// LaunchConfig contains a previously verified host capture, never an owner key,
// credential or agent-supplied executable/path. The direct read-only file mount
// and trusted image establish its provenance; the gateway cannot approve it.
type LaunchConfig struct {
	Version   int             `json:"version"`
	RunID     string          `json:"run_id"`
	Epoch     string          `json:"gateway_epoch"`
	Policy    egress.Snapshot `json:"policy"`
	Protected []netip.Prefix  `json:"protected"`
	// Services binds each approved `service:` grant to the ONE container
	// address the host read from the runtime at launch. The box never resolves
	// a service name itself, so a sidecar that moves cannot widen the grant.
	Services []ServiceBinding `json:"services,omitempty"`
	// ServiceProxyClients are the exact named containers on Coop's internal
	// service network that may reach the guard's approved-TLS CONNECT endpoint.
	ServiceProxyClients []ServiceProxyClient `json:"service_proxy_clients,omitempty"`
	// Serve is this project's published container ports. They are ingress the
	// operator asked for, not egress authority.
	Serve []int `json:"serve,omitempty"`
	// Ingress is the bridge gateway address host-published traffic is NAT'd
	// from — the ONE source a served port accepts. Without it a sibling
	// container on the same bridge could reach a port the host published on
	// loopback only.
	Ingress netip.Addr `json:"ingress,omitempty"`
	// Broker is non-secret helper-only authority derived from the selected adapter. The reusable
	// credential lives in a separate guard-only mount and never in this controller-readable file.
	Broker *CredentialBrokerRoute `json:"credential_broker,omitempty"`
}

// CredentialBrokerRoute is one exact provider endpoint, not a user policy or forward-proxy rule.
type CredentialBrokerRoute struct {
	Provider string `json:"provider"`
	Upstream string `json:"upstream"`
	Header   string `json:"header"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	Port     int    `json:"port"`
}

func (r CredentialBrokerRoute) valid() bool {
	name, err := egress.NormalizeDomain(r.Upstream, false)
	return err == nil && name == r.Upstream && r.Provider != "" && len(r.Provider) <= 32 &&
		strings.Trim(r.Provider, "abcdefghijklmnopqrstuvwxyz0123456789-") == "" &&
		r.Header != "" && strings.ToLower(r.Header) == r.Header &&
		r.Method == "POST" && strings.HasPrefix(r.Path, "/") &&
		!strings.ContainsAny(r.Path, "?#\x00\r\n") && r.Port == 443
}

type ServiceBinding struct {
	Name    string     `json:"name"`
	RuleID  string     `json:"rule_id"`
	Address netip.Addr `json:"address"`
}

type ServiceProxyClient struct {
	Name    string     `json:"name"`
	Address netip.Addr `json:"address"`
}

func ReadLaunchConfig(reader io.Reader) (LaunchConfig, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxLaunchConfigBytes+1))
	if err != nil || len(data) > MaxLaunchConfigBytes {
		return LaunchConfig{}, Failure("gateway_configuration_invalid")
	}
	var value LaunchConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Validate() != nil {
		return LaunchConfig{}, Failure("gateway_configuration_invalid")
	}
	return value, nil
}

func (c LaunchConfig) Validate() error {
	if c.Version != 1 || !lowerHex(c.RunID, 32) || !lowerHex(c.Epoch, 32) || !lowerHex(c.Policy.Fingerprint, 64) || c.Policy.Version != egress.Version ||
		len(c.Policy.Grants) > egress.MaxGrants || len(c.Protected) > MaxProtectedRanges || c.Policy.RequireSupported() != nil {
		return Failure("gateway_configuration_invalid")
	}
	for _, grant := range c.Policy.Grants {
		// The host authenticated this immutable capture. Reapplying today's
		// suffix catalog here would change authority across helper upgrades.
		rules, err := egress.CanonicalRules([]egress.Rule{grant.Rule})
		if err != nil || len(rules) != 1 || !lowerHex(grant.ID, 32) {
			return Failure("gateway_configuration_invalid")
		}
		original, _ := json.Marshal(grant.Rule)
		normalized, _ := json.Marshal(rules[0])
		if !bytes.Equal(original, normalized) {
			return Failure("gateway_configuration_invalid")
		}
	}
	for _, prefix := range c.Protected {
		if !prefix.IsValid() || prefix != prefix.Masked() || prefix.Addr().Is4In6() {
			return Failure("gateway_configuration_invalid")
		}
	}
	if len(c.Services) > egress.MaxGrants {
		return Failure("gateway_configuration_invalid")
	}
	for _, binding := range c.Services {
		if !lowerHex(binding.RuleID, 32) || binding.Name == "" || !binding.Address.Is4() {
			return Failure("gateway_configuration_invalid")
		}
	}
	if !validServiceProxyClients(c.Protected, c.ServiceProxyClients) || len(c.ServiceProxyClients) != 0 && slices.Contains(c.Serve, ServiceProxyPort) {
		return Failure("gateway_configuration_invalid")
	}
	if validServePorts(c.Serve, c.Policy.TLSPorts()) != nil {
		return Failure("gateway_configuration_invalid")
	}
	if len(c.Serve) != 0 && (!c.Ingress.Is4() || !c.Ingress.IsValid()) {
		return Failure("gateway_configuration_invalid")
	}
	if c.Broker != nil && (!c.Broker.valid() || slices.Contains(c.Serve, CredentialBrokerPort) || slices.Contains(c.Policy.TLSPorts(), CredentialBrokerPort)) {
		return Failure("gateway_configuration_invalid")
	}
	// One construction, one meaning: the same grant/binding/port checks the
	// controller applies decide whether this configuration is launchable.
	if _, err := addressGrants(c.Policy, c.Services); err != nil {
		return Failure("gateway_configuration_invalid")
	}
	return nil
}

func (c LaunchConfig) identity(clock *BootClock) Identity {
	return Identity{Clock: clock.Domain(), RunID: c.RunID, Epoch: c.Epoch, PolicyFingerprint: c.Policy.Fingerprint}
}

func RunController(ctx context.Context, config LaunchConfig) error {
	if config.Validate() != nil || verifyServiceRole("controller") != nil {
		return Failure("gateway_configuration_invalid")
	}
	clock, err := OpenBootClock()
	if err != nil {
		return err
	}
	// A fresh volume/generation is mandatory. Never remove a stale socket and
	// accidentally bootstrap another controller beneath an existing workload.
	if err := os.Mkdir("/ipc/controller", 0710); err != nil {
		return Failure("controller_socket_unavailable")
	}
	c, err := NewController(config.identity(clock), config.Policy, config.Protected, config.Services, config.ServiceProxyClients,
		config.Serve, config.Ingress, config.Broker, clock, applyKernelRules)
	if err != nil {
		return err
	}
	if err := c.Initialize(ctx, netip.MustParseAddr(MaintenanceResolver)); err != nil {
		return err
	}
	c.kernel.read = readKernelCounters
	return c.ServeControl(ctx, ControllerSocket)
}

func applyKernelRules(ctx context.Context, rules string) error {
	command := exec.CommandContext(ctx, "/usr/sbin/nft", "-f", "-")
	command.Stdin = strings.NewReader(rules)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	command.WaitDelay = time.Second
	if command.Run() != nil {
		return Failure("enforcement_unavailable")
	}
	return nil
}

// GuardRuntime owns one immutable gateway generation. Its sources are private
// in-process inputs to the observer; they confer no controller mutation API.
type GuardRuntime struct {
	Identity    Identity
	GuardEvents *GuardEvents
	EnvoyEvents *EnvoyEvents
	guard       *Guard
	broker      *credentialBroker
	doh         *DoH
	collector   *Collector
	phase       atomic.Uint32
	started     atomic.Bool
}

const (
	gatewayStarting uint32 = iota
	gatewayReady
	gatewayStopped
)

func (g *GuardRuntime) markReady() { g.phase.CompareAndSwap(gatewayStarting, gatewayReady) }
func (g *GuardRuntime) stopReady() { g.phase.Store(gatewayStopped) }

func NewGuardRuntime(config LaunchConfig) (*GuardRuntime, error) {
	if config.Validate() != nil || verifyServiceRole("guard") != nil {
		return nil, Failure("gateway_configuration_invalid")
	}
	clock, err := OpenBootClock()
	if err != nil {
		return nil, err
	}
	doh, err := NewDoH(netip.MustParseAddrPort(MaintenanceResolver+":443"), MaintenanceResolverName, nil)
	if err != nil {
		return nil, err
	}
	r, err := NewResolver(config.Policy, config.Protected, config.Services, clock, doh.Exchange)
	if err != nil {
		doh.Close()
		return nil, err
	}
	events := NewGuardEvents(clock)
	identity := config.identity(clock)
	controller := ControllerClient{Path: ControllerSocket, Identity: identity, Clock: clock}
	guard, err := NewGuard(config.Policy, clock, r, controller, events)
	if err != nil {
		doh.Close()
		return nil, err
	}
	guard.serviceProxyClients = slices.Clone(config.ServiceProxyClients)
	envoyEvents := NewEnvoyEvents(clock)
	var broker *credentialBroker
	if config.Broker != nil {
		file, openErr := os.Open(CredentialBrokerPath)
		if openErr != nil {
			doh.Close()
			return nil, Failure("credential_broker_configuration_invalid")
		}
		info, statErr := file.Stat()
		secret, readErr := ReadCredentialBrokerSecret(file, config)
		_ = file.Close()
		if statErr != nil || !info.Mode().IsRegular() || readErr != nil {
			doh.Close()
			return nil, Failure("credential_broker_configuration_invalid")
		}
		broker, err = newCredentialBroker(config, secret, clock, doh, events, controller)
		if err != nil {
			doh.Close()
			return nil, err
		}
	}
	var brokerResolvers []*Resolver
	if broker != nil {
		brokerResolvers = append(brokerResolvers, broker.resolver)
	}
	collector, err := NewCollector(guard, envoyEvents, doh, brokerResolvers...)
	if err != nil {
		doh.Close()
		return nil, err
	}
	return &GuardRuntime{Identity: identity, GuardEvents: events, EnvoyEvents: envoyEvents, guard: guard, broker: broker, doh: doh, collector: collector}, nil
}

func (g *GuardRuntime) Run(ctx context.Context) (result error) {
	if !g.started.CompareAndSwap(false, true) {
		return Failure("gateway_generation_used")
	}
	defer g.doh.Close()
	for _, path := range []string{EnvoyDataSocket, EnvoyAdminSocket, RuntimeSocket, RuntimeObserveSocket} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return Failure("gateway_socket_occupied")
		}
	}
	if err := prepareObservationDirectory(ObservationDirectory); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	observerCtx, stopObserver := context.WithCancel(context.WithoutCancel(ctx))
	var observers sync.WaitGroup
	observers.Go(func() { g.collector.Run(observerCtx, func() bool { return g.phase.Load() == gatewayReady }) })
	observers.Go(func() { _ = g.serveObservation(observerCtx, RuntimeObserveSocket, servicePeer) })
	defer func() {
		stopObserver()
		observers.Wait()
		// Registered before process/forwarder cleanup defers: these run first,
		// then this final sample consumes the already-drained terminal events.
		final, done := context.WithTimeout(context.Background(), ControlTimeout)
		defer done()
		if err := g.doh.Shutdown(final); err != nil {
			result = errors.Join(result, err)
		}
		g.collector.sample(final, false, true, g.guard.clock.instant())
		result = g.finishObservation(ObservationDirectory, result)
	}()
	// Linux parent-death signaling follows the creating OS thread, not the Go
	// goroutine. Keep that thread alive until its exact Envoy child is reaped.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	command := exec.Command("/usr/local/bin/envoy", "--disable-hot-restart", "--concurrency", "1", "--log-level", "error",
		"--file-flush-interval-msec", "100", "--file-flush-min-size-kb", "1", "--config-yaml", EnvoyBootstrap)
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C"}
	command.SysProcAttr = envoyProcessAttributes()
	command.Stdout, command.Stderr = g.EnvoyEvents, io.Discard
	command.WaitDelay = time.Second
	if err := command.Start(); err != nil {
		g.EnvoyEvents.Finish(Failure("gateway_unavailable"))
		return Failure("gateway_unavailable")
	}
	processDone := make(chan struct{})
	go func() { g.EnvoyEvents.Finish(command.Wait()); close(processDone) }()
	defer func() {
		g.stopReady()
		cancel()
		select {
		case <-processDone:
			return
		default:
		}
		_ = command.Process.Signal(syscall.SIGTERM)
		timer := time.NewTimer(EnvoyStopTimeout)
		defer timer.Stop()
		select {
		case <-processDone:
		case <-timer.C:
			_ = command.Process.Kill()
			select {
			case <-processDone:
			case <-time.After(time.Second):
				result = errors.Join(result, Failure("gateway_shutdown_unconfirmed"))
			}
		}
	}()
	health := newEnvoyHealth(EnvoyAdminSocket, g.guard.clock)
	defer health.transport.CloseIdleConnections()
	startup, done := context.WithTimeout(ctx, EnvoyStartTimeout)
	defer done()
	for health.check(startup) != nil {
		select {
		case <-processDone:
			return Failure("gateway_unavailable")
		case <-startup.Done():
			return Failure("gateway_start_timeout")
		case <-time.After(20 * time.Millisecond):
		}
	}
	var workers sync.WaitGroup
	defer func() { g.stopReady(); cancel(); workers.Wait() }()
	failures := make(chan error, 4)
	needed := int32(1)
	if g.broker != nil {
		needed++
	}
	var readyParts atomic.Int32
	partReady := func() {
		if readyParts.Add(1) == needed {
			g.markReady()
		}
	}
	workers.Go(func() { failures <- g.guard.Serve(ctx, partReady) })
	if g.broker != nil {
		workers.Go(func() { failures <- g.broker.Serve(ctx, partReady) })
	}
	workers.Go(func() { failures <- health.watch(ctx) })
	workers.Go(func() { failures <- g.serveReadiness(ctx) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-processDone:
		return Failure("gateway_unavailable")
	case err := <-failures:
		return err
	}
}

type envoyHealth struct {
	client    *http.Client
	transport *http.Transport
	clock     *BootClock
}

func newEnvoyHealth(path string, clock *BootClock) *envoyHealth {
	transport := &http.Transport{Proxy: nil, MaxConnsPerHost: 1, MaxIdleConns: 1, MaxIdleConnsPerHost: 1,
		MaxResponseHeaderBytes: 4096, ResponseHeaderTimeout: EnvoyHealthTimeout, DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}}
	return &envoyHealth{clock: clock, transport: transport, client: &http.Client{Transport: transport, Timeout: EnvoyHealthTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (h *envoyHealth) check(ctx context.Context) error {
	started := h.clock.instant()
	if !started.Valid() {
		return Failure("clock_unavailable")
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://gateway/ready", nil)
	response, err := h.client.Do(request)
	if err != nil {
		return Failure("gateway_unavailable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 33))
	completed := h.clock.instant()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "LIVE\n" || !completed.Valid() || completed.Before(started) || completed.Sub(started) > EnvoyHealthTimeout {
		return Failure("gateway_unavailable")
	}
	return nil
}

func (h *envoyHealth) watch(ctx context.Context) error {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		if err := h.check(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (g *GuardRuntime) serveReadiness(ctx context.Context) error {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: RuntimeSocket, Net: "unix"})
	if err != nil {
		return Failure("gateway_socket_occupied")
	}
	defer listener.Close()
	if os.Chmod(RuntimeSocket, 0600) != nil {
		return Failure("gateway_socket_occupied")
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	slots := make(chan struct{}, 8)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			return Failure("gateway_unavailable")
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		workers.Go(func() {
			defer func() { <-slots; _ = conn.Close() }()
			if conn.SetDeadline(time.Now().Add(ControlTimeout)) != nil || !servicePeer(conn) {
				return
			}
			var request controlRequest
			if readControl(conn, &request) != nil || request.Version != 1 || request.Identity != g.Identity || request.Operation != "ready" || request.Lease != nil || request.AfterBoot != 0 {
				return
			}
			_ = json.NewEncoder(conn).Encode(controlReply{Identity: g.Identity, Version: 1, Ready: g.phase.Load() == gatewayReady})
		})
	}
}

// ProbeRuntime is a host-exec readiness probe, not a guard heartbeat. It cannot
// keep a failed guard alive. The socket is absent from the agent's filesystem.
func ProbeRuntime(ctx context.Context, config LaunchConfig) error {
	if config.Validate() != nil || verifyServiceRole("guard") != nil {
		return Failure("gateway_configuration_invalid")
	}
	clock, err := OpenBootClock()
	if err != nil {
		return err
	}
	client := ControllerClient{Path: RuntimeSocket, Identity: config.identity(clock), Clock: clock}
	_, err = client.callPath(ctx, controlRequest{Version: 1, Operation: "ready"}, RuntimeSocket)
	return err
}
