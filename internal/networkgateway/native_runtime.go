package networkgateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/safefile"
)

const (
	NativeProxyAddress     = "127.0.0.1:15445"
	NativePrivateDirectory = "/run/coop-native"
	NativePublicCA         = "/run/coop-native-ca.pem"
)

// NativeRunConfig is guard-only. No canonical or refresh credential belongs here;
// current access travels in separately atomic, expiring snapshot files.
type NativeRunConfig struct {
	Version   int
	RunID     string
	Accounts  []NativeAccountConfig
	Downloads []CredentialBrokerRoute
}

type NativeAccountConfig struct {
	Binding NativeBrokerBinding
	Origins []NativeOriginConfig
}

type NativeOriginConfig struct {
	Host                 string
	Certificate, Key     []byte
	Requests             []NativeBrokerRequestLine
	AccountHeaders       map[string]string
	ClientAccountHeaders map[string]string
}

func readNativeDocument(name string, value any) error {
	root, err := safefile.OpenRoot(NativePrivateDirectory)
	if err != nil {
		return Failure("native_broker_configuration_invalid")
	}
	defer root.Close()
	raw, err := safefile.ReadRegular(root, name, 1<<20)
	if err != nil {
		return Failure("native_broker_configuration_invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil || decoder.Decode(new(any)) != io.EOF {
		return Failure("native_broker_configuration_invalid")
	}
	return nil
}

func readNativeRunConfig(runID string, origins []string) (NativeRunConfig, error) {
	var config NativeRunConfig
	if readNativeDocument("config.json", &config) != nil || config.Version != 1 ||
		!lowerHex(config.RunID, 32) || runID != "" && config.RunID != runID || len(config.Accounts) == 0 || len(config.Accounts) > 4 {
		return config, Failure("native_broker_configuration_invalid")
	}
	var seen []string
	providers := map[string]bool{}
	for _, account := range config.Accounts {
		binding := account.Binding
		if binding.RunID != config.RunID || binding.Epoch == 0 || binding.Account == "" || providers[binding.Provider] ||
			binding.Provider == "" || strings.Trim(binding.Provider, "abcdefghijklmnopqrstuvwxyz") != "" || len(account.Origins) == 0 {
			return config, Failure("native_broker_configuration_invalid")
		}
		providers[binding.Provider] = true
		for _, origin := range account.Origins {
			host, err := egress.NormalizeDomain(origin.Host, false)
			if err != nil || host != origin.Host || slices.Contains(seen, host) || len(origin.Requests) == 0 || len(origin.Requests) > 128 {
				return config, Failure("native_broker_configuration_invalid")
			}
			seen = append(seen, host)
		}
	}
	if len(config.Downloads) > 4 {
		return config, Failure("native_broker_configuration_invalid")
	}
	lines := map[BrokerRequestLine]bool{}
	for _, download := range config.Downloads {
		if download.Kind != CredentialBrokerDownload || !download.valid() {
			return config, Failure("native_broker_configuration_invalid")
		}
		for _, line := range download.Allow {
			if lines[line] {
				return config, Failure("native_broker_configuration_invalid")
			}
			lines[line] = true
		}
		if !slices.Contains(seen, download.Upstream) {
			seen = append(seen, download.Upstream)
		}
	}
	if origins != nil {
		slices.Sort(seen)
		expected := slices.Clone(origins)
		slices.Sort(expected)
		if !slices.Equal(seen, expected) {
			return config, Failure("native_broker_configuration_invalid")
		}
	}
	return config, nil
}

type nativeRunRuntime struct {
	config   NativeRunConfig
	dial     map[string]func(context.Context, string, string) (net.Conn, error)
	fallback http.Handler
}

func (r *nativeRunRuntime) serve(ctx context.Context, ready func()) error {
	var handler http.Handler = r.fallback
	if len(r.config.Downloads) != 0 {
		var downloads []*credentialBroker
		for _, route := range r.config.Downloads {
			if !route.valid() || route.Kind != CredentialBrokerDownload || r.dial[route.Upstream] == nil {
				return Failure("native_broker_configuration_invalid")
			}
			broker := &credentialBroker{route: route, address: NativeProxyAddress, events: NewGuardEvents(nil), slots: make(chan struct{}, maxCredentialBrokerFlows), lifecycle: ctx}
			broker.setTransport(r.dial[route.Upstream])
			defer broker.transport.CloseIdleConnections()
			downloads = append(downloads, broker)
		}
		ordinary := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.Host != NativeProxyAddress {
				ordinary.ServeHTTP(w, request)
				return
			}
			for _, broker := range downloads {
				if broker.route.Admits(request.Method, request.URL) {
					broker.handler().ServeHTTP(w, request)
					return
				}
			}
			http.Error(w, "native download refused", http.StatusForbidden)
		})
	}
	var brokers []*NativeBroker
	defer func() {
		for _, broker := range brokers {
			_ = broker.Close()
		}
	}()
	for _, account := range r.config.Accounts {
		var origins []NativeBrokerOrigin
		for _, origin := range account.Origins {
			certificate, err := tls.X509KeyPair(origin.Certificate, origin.Key)
			if err != nil {
				return Failure("native_broker_configuration_invalid")
			}
			origins = append(origins, NativeBrokerOrigin{Host: origin.Host, Certificate: certificate, Requests: origin.Requests,
				AccountHeaders: origin.AccountHeaders, ClientAccountHeaders: origin.ClientAccountHeaders, DialContext: r.dial[origin.Host]})
		}
		broker, err := NewNativeBroker(ctx, account.Binding, origins, func(ctx context.Context) (NativeAccessSnapshot, error) {
			if err := ctx.Err(); err != nil {
				return NativeAccessSnapshot{}, err
			}
			var snapshot NativeAccessSnapshot
			err := readNativeDocument(account.Binding.Provider+".json", &snapshot)
			return snapshot, err
		}, handler)
		if err != nil {
			return err
		}
		brokers = append(brokers, broker)
		handler = broker
	}
	listener, err := net.Listen("tcp4", NativeProxyAddress)
	if err != nil {
		return Failure("native_broker_listener_unavailable")
	}
	defer listener.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: MaxServiceProxyHeader,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	stop := context.AfterFunc(ctx, func() { _ = server.Close(); _ = listener.Close() })
	defer stop()
	defer server.Close()
	if ready != nil {
		ready()
	}
	err = server.Serve(listener)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Ordinary CONNECT traffic stays opaque. The native run namespace, not an
// exported bearer or an externally reachable listener, owns this entrance.
type nativeOrdinaryProxy struct {
	ctx       context.Context
	cancel    context.CancelFunc
	guard     *Guard
	slots     chan struct{}
	workers   sync.WaitGroup
	mu        sync.Mutex
	closing   bool
	transport *http.Transport
}

func newNativeOrdinaryProxy(ctx context.Context, guard *Guard) *nativeOrdinaryProxy {
	ctx, cancel := context.WithCancel(ctx)
	return &nativeOrdinaryProxy{ctx: ctx, cancel: cancel, guard: guard, slots: make(chan struct{}, MaxGuardFlows),
		transport: &http.Transport{Proxy: nil, MaxConnsPerHost: 32, MaxIdleConns: 32, IdleConnTimeout: time.Minute,
			ResponseHeaderTimeout: credentialBrokerTimeout}}
}

func (p *nativeOrdinaryProxy) close() {
	p.mu.Lock()
	p.closing = true
	p.mu.Unlock()
	p.cancel()
	p.transport.CloseIdleConnections()
	p.workers.Wait()
}

func (p *nativeOrdinaryProxy) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		http.Error(w, "proxy stopped", http.StatusServiceUnavailable)
		return
	}
	p.workers.Add(1)
	p.mu.Unlock()
	defer p.workers.Done()
	ctx, cancel := context.WithCancel(request.Context())
	stopRequest := context.AfterFunc(p.ctx, cancel)
	defer func() { stopRequest(); cancel() }()
	request = request.WithContext(ctx)
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		http.Error(w, "proxy busy", http.StatusServiceUnavailable)
		return
	}
	if request.Method != http.MethodConnect {
		if p.guard != nil || request.URL == nil || request.URL.Scheme != "http" || request.URL.Host == "" ||
			request.URL.User != nil || request.Header.Get("Proxy-Authorization") != "" {
			http.Error(w, "proxy request refused", http.StatusForbidden)
			return
		}
		proxy := httputil.ReverseProxy{Transport: p.transport, Rewrite: func(r *httputil.ProxyRequest) { r.Out.Header.Del("Proxy-Authorization") },
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				http.Error(w, "upstream unavailable", http.StatusBadGateway)
			}}
		proxy.ServeHTTP(w, request)
		return
	}
	host, portText, err := net.SplitHostPort(request.Host)
	port, portErr := strconv.Atoi(portText)
	name := host
	var nameErr error
	if p.guard != nil {
		name, nameErr = egress.NormalizeDomain(host, false)
	}
	if err != nil || portErr != nil || nameErr != nil || host == "" || strings.ContainsAny(host, "/\\\x00\r\n \t") || port < 1 || port > 65535 || request.URL == nil ||
		request.URL.Host != request.Host || request.URL.Path != "" || request.ContentLength > 0 ||
		len(request.TransferEncoding) != 0 || request.Header.Get("Proxy-Authorization") != "" ||
		p.guard != nil && !p.guard.policy.Domain(name, port).Allowed {
		http.Error(w, "proxy request refused", http.StatusForbidden)
		return
	}
	conn, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(p.ctx, func() { _ = conn.Close() })
	defer stop()
	pending := make([]byte, buffered.Reader.Buffered())
	if _, err = io.ReadFull(buffered.Reader, pending); err != nil {
		return
	}
	stream := &prefixedConn{Conn: conn, prefix: pending}
	admission, cancel := context.WithTimeout(p.ctx, GuardAdmissionTimeout)
	defer cancel()
	if p.guard != nil {
		if !proxyStatus(conn, "200 Connection Established") {
			return
		}
		hello, err := Inspect(admission, stream, p.guard.policy, port)
		if err != nil || hello.Name != name {
			return
		}
		p.guard.forwardTLS(p.ctx, admission, stream, EnvoyDataSocket, port, hello, "")
		return
	}
	upstream, err := (&net.Dialer{}).DialContext(admission, "tcp", net.JoinHostPort(name, portText))
	if err != nil {
		_ = proxyStatus(conn, "502 Bad Gateway")
		return
	}
	defer upstream.Close()
	stopUpstream := context.AfterFunc(p.ctx, func() { _ = upstream.Close() })
	defer stopUpstream()
	if proxyStatus(conn, "200 Connection Established") {
		pipeBoth(stream, upstream)
	}
}

func (g *GuardRuntime) prepareNative(config LaunchConfig, clock *BootClock, controller ControllerClient) ([]*Resolver, error) {
	if len(config.NativeOrigins) == 0 {
		return nil, nil
	}
	plan, err := readNativeRunConfig(config.RunID, config.NativeOrigins)
	if err != nil {
		return nil, err
	}
	g.native = &nativeRunRuntime{config: plan, dial: map[string]func(context.Context, string, string) (net.Conn, error){}}
	var resolvers []*Resolver
	for _, host := range config.NativeOrigins {
		var key [32]byte
		if _, err := rand.Read(key[:]); err != nil {
			return nil, err
		}
		rule := egress.Rule{To: egress.Destination{Domain: host}, Protocol: "tls", Ports: []int{443}}
		policy, err := egress.Compile("native-broker", egress.Filtered, []egress.Input{{Rules: []egress.Rule{rule}, Origin: egress.Origin{Kind: "broker", Name: host}}}, nil, false, key[:])
		if err != nil {
			return nil, err
		}
		resolver, err := NewResolver(policy, config.Protected, nil, clock, g.doh.Exchange)
		if err != nil {
			return nil, err
		}
		// Reuse the existing protected resolver, controller lease and recorded
		// Envoy flow; native access does not grant agent-direct egress.
		dialer := &credentialBroker{route: CredentialBrokerRoute{Upstream: host, Port: 443}, clock: clock, resolver: resolver, controller: controller, events: g.GuardEvents}
		g.native.dial[host] = dialer.dial
		resolvers = append(resolvers, resolver)
	}
	return resolvers, nil
}

// RunNativeBroker owns an open run's private namespace. The workload joins its
// exact container ID, while only this capless owner mounts the private state.
func RunNativeBroker(ctx context.Context, in io.Reader, out io.Writer) error {
	if verifyServiceRole("broker") != nil {
		return Failure("gateway_role_invalid")
	}
	plan, err := readNativeRunConfig("", nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if in != nil {
		go func() { _, _ = io.Copy(io.Discard, in); cancel() }()
	}
	ordinary := newNativeOrdinaryProxy(ctx, nil)
	defer func() { cancel(); ordinary.close() }()
	runtime := &nativeRunRuntime{config: plan, fallback: ordinary, dial: map[string]func(context.Context, string, string) (net.Conn, error){}}
	for _, account := range plan.Accounts {
		for _, origin := range account.Origins {
			runtime.dial[origin.Host] = (&net.Dialer{}).DialContext
		}
	}
	for _, download := range plan.Downloads {
		runtime.dial[download.Upstream] = (&net.Dialer{}).DialContext
	}
	return runtime.serve(ctx, func() { _, _ = io.WriteString(out, "ready\n") })
}
