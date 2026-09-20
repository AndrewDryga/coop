package networkgateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

const (
	maxCredentialBrokerSecret = MaxCredentialBrokerRoutes * (33 << 10) // a 32 KiB credential and its envelope per route
	maxCredentialBrokerBody   = 64 << 20
	maxBrokerDownloadBody     = 1 << 20
	maxCredentialBrokerFlows  = 32
	credentialBrokerTimeout   = 30 * time.Second
)

// CredentialBrokerSecrets is mounted into the capless guard only. It is deliberately separate
// from LaunchConfig, which is also readable by the privileged controller. Routes[i] is the secret
// of LaunchConfig.Brokers[i], bound to this one gateway generation.
type CredentialBrokerSecrets struct {
	Version int                      `json:"version"`
	RunID   string                   `json:"run_id"`
	Epoch   string                   `json:"gateway_epoch"`
	Routes  []CredentialBrokerSecret `json:"routes"`
}

// CredentialBrokerSecret is one route's temporary capability and the real credential it stands for,
// bound to its route by name.
type CredentialBrokerSecret struct {
	Name       string `json:"name"`
	Substitute string `json:"substitute"`
	Credential string `json:"credential"`
}

func ReadCredentialBrokerSecrets(reader io.Reader, config LaunchConfig) (CredentialBrokerSecrets, error) {
	return readBrokerSecrets(reader, config.RunID, config.Epoch, config.Brokers)
}

// readBrokerSecrets reads the secrets of routes, bound to one run and one broker generation.
func readBrokerSecrets(reader io.Reader, runID, epoch string, routes []CredentialBrokerRoute) (CredentialBrokerSecrets, error) {
	invalid := Failure("credential_broker_configuration_invalid")
	data, err := io.ReadAll(io.LimitReader(reader, maxCredentialBrokerSecret+1))
	if err != nil || len(data) == 0 || len(data) > maxCredentialBrokerSecret || len(routes) == 0 {
		return CredentialBrokerSecrets{}, invalid
	}
	var value CredentialBrokerSecrets
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF ||
		value.Version != 2 || value.RunID != runID || value.Epoch != epoch || len(value.Routes) != len(routes) {
		return CredentialBrokerSecrets{}, invalid
	}
	// A substitute opens exactly one listener: were two routes to share one, a capability issued
	// for one account would unlock the other's key.
	substitutes := make(map[string]bool, len(value.Routes))
	for i, route := range value.Routes {
		if route.Name != routes[i].Name {
			return CredentialBrokerSecrets{}, invalid
		}
		// A download route fetches public bytes: it must carry NO credential and no capability, so
		// a secrets file that hands one something to send is refused outright.
		if routes[i].Kind == CredentialBrokerDownload {
			if route.Substitute != "" || route.Credential != "" {
				return CredentialBrokerSecrets{}, invalid
			}
			continue
		}
		if len(route.Substitute) < 32 || len(route.Substitute) > 256 ||
			len(route.Credential) < 8 || len(route.Credential) > 32<<10 || route.Substitute == route.Credential || substitutes[route.Substitute] ||
			strings.ContainsAny(route.Substitute, "\x00\r\n") || strings.ContainsAny(route.Credential, "\x00\r\n") {
			return CredentialBrokerSecrets{}, invalid
		}
		substitutes[route.Substitute] = true
	}
	return value, nil
}

type credentialBroker struct {
	route      CredentialBrokerRoute
	address    string // this route's own listener, as every request must name it (Host)
	listen     string // where it listens when that is not address: an open helper's own IP
	secret     CredentialBrokerSecret
	clock      *BootClock
	resolver   *Resolver
	controller ControllerClient
	events     *GuardEvents
	transport  *http.Transport
	proxy      *httputil.ReverseProxy
	slots      chan struct{}
	peerCursor atomic.Uint64
	lifecycle  context.Context
}

func newCredentialBroker(config LaunchConfig, index int, secret CredentialBrokerSecret, clock *BootClock, doh *DoH, events *GuardEvents, controller ControllerClient) (*credentialBroker, error) {
	if index < 0 || index >= len(config.Brokers) || !config.Brokers[index].valid() || clock == nil || doh == nil || events == nil ||
		secret.Name != config.Brokers[index].Name {
		return nil, Failure("credential_broker_configuration_invalid")
	}
	route := config.Brokers[index]
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, Failure("credential_broker_unavailable")
	}
	rule := egress.Rule{To: egress.Destination{Domain: route.Upstream}, Protocol: "tls", Ports: []int{route.Port}}
	policy, err := egress.Compile("broker", egress.Filtered, []egress.Input{{Rules: []egress.Rule{rule}, Origin: egress.Origin{Kind: "broker", Name: route.Name}}}, nil, false, key[:])
	if err != nil {
		return nil, Failure("credential_broker_configuration_invalid")
	}
	resolver, err := NewResolver(policy, config.Protected, nil, clock, doh.Exchange)
	if err != nil {
		return nil, Failure("credential_broker_configuration_invalid")
	}
	b := &credentialBroker{route: route, address: CredentialBrokerAddress(index), secret: secret, clock: clock, resolver: resolver,
		controller: controller, events: events, slots: make(chan struct{}, maxCredentialBrokerFlows)}
	b.setTransport(b.dial)
	return b, nil
}

// newOpenCredentialBroker is route index of an open run's helper: listening on the helper's own
// address, it dials the route's one upstream directly — an open box's traffic passes no gateway, so
// there is no lease to take and no policy to hold it to. Only MCP routes: provider keys stay a
// filtered run's.
func newOpenCredentialBroker(route CredentialBrokerRoute, host netip.Addr, index int, secret CredentialBrokerSecret) (*credentialBroker, error) {
	if !route.valid() || route.Kind != CredentialBrokerMCP || secret.Name != route.Name || !host.Is4() {
		return nil, Failure("credential_broker_configuration_invalid")
	}
	port := strconv.Itoa(CredentialBrokerPort + index)
	// It listens on its own address and answers to the NAME the box was given: the box's MCP
	// configuration is written before this helper exists, so the name is what can be written down.
	b := &credentialBroker{route: route, address: net.JoinHostPort(OpenBrokerHost, port), listen: net.JoinHostPort(host.String(), port),
		secret: secret, events: NewGuardEvents(nil), slots: make(chan struct{}, maxCredentialBrokerFlows)}
	b.setTransport(b.dialDirect)
	return b, nil
}

func (b *credentialBroker) setTransport(dial func(context.Context, string, string) (net.Conn, error)) {
	// A provider streams its headers at once. An MCP server answering a tool call with JSON sends
	// them only when the tool finishes — minutes for a long runbook — and a 502 there makes the
	// agent retry a mutation, so only the run's own lifetime bounds that wait.
	headerTimeout := credentialBrokerTimeout
	if b.route.Kind == CredentialBrokerMCP {
		headerTimeout = 0
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            dial,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, ServerName: b.route.Upstream},
		ForceAttemptHTTP2:      true,
		DisableCompression:     true,
		MaxConnsPerHost:        maxCredentialBrokerFlows,
		MaxIdleConns:           maxCredentialBrokerFlows,
		MaxIdleConnsPerHost:    maxCredentialBrokerFlows,
		IdleConnTimeout:        time.Minute,
		TLSHandshakeTimeout:    credentialBrokerTimeout,
		ResponseHeaderTimeout:  headerTimeout,
		MaxResponseHeaderBytes: 1 << 20,
	}
	b.transport = transport
	b.setProxy(transport)
}

func (b *credentialBroker) setProxy(transport http.RoundTripper) {
	b.proxy = &httputil.ReverseProxy{
		Transport: transport,
		Director: func(request *http.Request) {
			request.URL.Scheme = "https"
			request.URL.Host = b.route.Upstream
			request.Host = b.route.Upstream
			request.Header.Del("Authorization")
			request.Header.Del("X-Api-Key")
			request.Header.Del("X-Goog-Api-Key")
			request.Header.Del("Proxy-Authorization")
			request.Header.Del("Cookie")
			if b.route.Kind == CredentialBrokerDownload {
				return // public bytes: this route holds no credential and adds none
			}
			request.Header.Set(b.route.Header, b.route.HeaderPrefix+b.secret.Credential)
		},
		ModifyResponse: func(response *http.Response) error {
			response.Header.Del("Set-Cookie")
			if response.StatusCode >= 300 && response.StatusCode < 400 {
				return Failure("credential_broker_redirect_refused")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, request *http.Request, err error) {
			if request.Context().Err() != nil {
				return // normal client/generation cancellation is not a network failure
			}
			reason := safeReason(err)
			message := "credential broker unavailable"
			if reason == "credential_broker_redirect_refused" {
				message = "credential broker refused an upstream redirect; configure the server's final URL"
			} else {
				reason = "credential_broker_upstream_unavailable"
			}
			b.events.emit(GuardEvent{Kind: "admission_failed", Name: b.route.Upstream, Reason: reason})
			http.Error(w, message, http.StatusBadGateway)
		},
		FlushInterval: -1,
	}
}

// handler admits one request: to this route's own listener and endpoint, carrying this route's
// secret header with exactly the stand-in issued for it — or, on a download route, carrying no
// credential at all, since that route fetches public bytes and holds nothing to send with them. A request naming any OTHER credential
// header the broker itself would set — or the connection headers a proxy owns — is refused, so a
// box cannot ride its own Authorization upstream beside the credential. A header the broker does
// not set travels as written: the box already reaches this one upstream through this route, and the
// route's own credential is the only authority it gains.
func (b *credentialBroker) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if b.route.Kind == CredentialBrokerDownload {
			b.serveDownload(w, request)
			return
		}
		if !b.route.Admits(request.Method, request.URL) || request.Host != b.address || request.Header.Get("Authorization") != "" && b.route.Header != "authorization" ||
			request.Header.Get("X-Api-Key") != "" && b.route.Header != "x-api-key" ||
			request.Header.Get("X-Goog-Api-Key") != "" && b.route.Header != "x-goog-api-key" ||
			request.Header.Get("Proxy-Authorization") != "" || request.Header.Get("Cookie") != "" || request.Header.Get("Upgrade") != "" {
			http.Error(w, "credential broker request refused", http.StatusForbidden)
			return
		}
		values := request.Header.Values(b.route.Header)
		if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte(b.route.HeaderPrefix+b.secret.Substitute)) != 1 {
			http.Error(w, "credential broker authentication refused", http.StatusUnauthorized)
			return
		}
		select {
		case b.slots <- struct{}{}:
			defer func() { <-b.slots }()
		default:
			http.Error(w, "credential broker busy", http.StatusServiceUnavailable)
			return
		}
		requestCtx, cancel := b.requestContext(request.Context())
		defer cancel()
		request = request.WithContext(requestCtx)
		request.Body = http.MaxBytesReader(w, request.Body, maxCredentialBrokerBody)
		b.proxy.ServeHTTP(w, request)
	})
}

// serveDownload forwards one public request and nothing else. It sends no credential, and refuses a
// request that brings one: a box handing its own Authorization to a route that reaches a host its
// policy does not grant would be smuggling, not fetching.
func (b *credentialBroker) serveDownload(w http.ResponseWriter, request *http.Request) {
	// A refusal here is a real refusal, recorded like any other: this route is the box's only path
	// to a host its policy does not grant, so a request it does not carry must be visible in the
	// run's report rather than disappearing into a local 403.
	refuse := func(reason, message string) {
		b.events.emit(GuardEvent{Kind: "admission_failed", Name: b.route.Upstream, Reason: reason})
		http.Error(w, message, http.StatusForbidden)
	}
	if !b.route.Admits(request.Method, request.URL) || request.Host != b.address || request.Header.Get("Upgrade") != "" {
		refuse("credential_broker_request_refused", "credential broker request refused")
		return
	}
	for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Proxy-Authorization", "Cookie"} {
		if request.Header.Get(name) != "" {
			refuse("credential_broker_credential_refused", "credential broker refuses a credential on a download")
			return
		}
	}
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	default:
		http.Error(w, "credential broker busy", http.StatusServiceUnavailable)
		return
	}
	requestCtx, cancel := b.requestContext(request.Context())
	defer cancel()
	request = request.WithContext(requestCtx)
	// A download SENDS little — a git fetch's want-list, a query — so its request body is bounded
	// far below a model call's, keeping this narrow path from becoming a wide one.
	request.Body = http.MaxBytesReader(w, request.Body, maxBrokerDownloadBody)
	b.proxy.ServeHTTP(w, request)
}

func (b *credentialBroker) Serve(ctx context.Context, ready func()) error {
	address := b.address
	if b.listen != "" {
		address = b.listen
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return Failure("credential_broker_listener_unavailable")
	}
	b.lifecycle = ctx
	server := &http.Server{Handler: b.handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute,
		MaxHeaderBytes: 1 << 20}
	stopped := make(chan struct{})
	go func() {
		<-ctx.Done()
		_ = server.Close()
		close(stopped)
	}()
	if ready != nil {
		ready()
	}
	err = server.Serve(listener)
	b.transport.CloseIdleConnections()
	if ctx.Err() != nil {
		<-stopped
		return ctx.Err()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return Failure("credential_broker_stopped")
	}
	return Failure("credential_broker_unavailable")
}

func (b *credentialBroker) requestContext(request context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(request)
	if b.lifecycle == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(b.lifecycle, cancel)
	return ctx, func() { stop(); cancel() }
}

func (b *credentialBroker) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" || address != net.JoinHostPort(b.route.Upstream, "443") {
		return nil, Failure("credential_broker_request_refused")
	}
	admission, cancel := context.WithTimeout(ctx, GuardAdmissionTimeout)
	defer cancel()
	resolution, err := b.resolver.Resolve(admission, b.route.Upstream)
	if err != nil || len(resolution.Addresses) == 0 {
		return nil, Failure("credential_broker_upstream_unavailable")
	}
	_, peer, until, err := admitLease(admission, b.resolver, &b.peerCursor, resolution, b.route.Port, b.controller.AdmitBroker)
	if err != nil {
		return nil, Failure("credential_broker_upstream_unavailable")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, Failure("credential_broker_unavailable")
	}
	flowID := hex.EncodeToString(random[:])
	header, err := ProxyHeader(netip.AddrPortFrom(peer, uint16(b.route.Port)), flowID)
	if err != nil {
		return nil, Failure("credential_broker_unavailable")
	}
	private, err := (&net.Dialer{}).DialContext(admission, "unix", EnvoyDataSocket)
	if err != nil {
		return nil, Failure("credential_broker_upstream_unavailable")
	}
	b.events.emit(GuardEvent{Kind: "flow_registered", FlowID: flowID, Name: b.route.Upstream, Peer: peer, Port: b.route.Port})
	conn := &brokerFlowConn{Conn: private, closeHook: func() {
		b.events.emit(GuardEvent{Kind: "private_flow_closed", FlowID: flowID})
	}}
	deadline, _ := admission.Deadline()
	now := b.clock.instant()
	if !now.Before(until) {
		_ = conn.Close()
		return nil, Failure("credential_broker_upstream_unavailable")
	}
	if leaseDeadline := time.Now().Add(until.Sub(now)); deadline.IsZero() || leaseDeadline.Before(deadline) {
		deadline = leaseDeadline
	}
	if conn.SetWriteDeadline(deadline) != nil || writeAll(conn, header) != nil || conn.SetWriteDeadline(time.Time{}) != nil {
		_ = conn.Close()
		return nil, Failure("credential_broker_upstream_unavailable")
	}
	return conn, nil
}

// dialDirect is an open helper's dial: the route's one upstream on 443, resolved by the helper's own
// resolver. TLS still verifies the upstream's name, so a lying resolver gets no credential.
func (b *credentialBroker) dialDirect(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" || address != net.JoinHostPort(b.route.Upstream, "443") {
		return nil, Failure("credential_broker_request_refused")
	}
	conn, err := (&net.Dialer{Timeout: credentialBrokerTimeout}).DialContext(ctx, network, address)
	if err != nil {
		return nil, Failure("credential_broker_upstream_unavailable")
	}
	return conn, nil
}

type brokerFlowConn struct {
	net.Conn
	once      sync.Once
	closeHook func()
}

func (c *brokerFlowConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.closeHook)
	return err
}
