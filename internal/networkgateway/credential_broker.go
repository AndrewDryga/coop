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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

const (
	maxCredentialBrokerSecret = MaxCredentialBrokerRoutes * (33 << 10) // a 32 KiB credential and its envelope per route
	maxCredentialBrokerBody   = 64 << 20
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
	invalid := Failure("credential_broker_configuration_invalid")
	data, err := io.ReadAll(io.LimitReader(reader, maxCredentialBrokerSecret+1))
	if err != nil || len(data) == 0 || len(data) > maxCredentialBrokerSecret || len(config.Brokers) == 0 {
		return CredentialBrokerSecrets{}, invalid
	}
	var value CredentialBrokerSecrets
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF ||
		value.Version != 2 || value.RunID != config.RunID || value.Epoch != config.Epoch || len(value.Routes) != len(config.Brokers) {
		return CredentialBrokerSecrets{}, invalid
	}
	// A substitute opens exactly one listener: were two routes to share one, a capability issued
	// for one account would unlock the other's key.
	substitutes := make(map[string]bool, len(value.Routes))
	for i, route := range value.Routes {
		if route.Name != config.Brokers[i].Name || len(route.Substitute) < 32 || len(route.Substitute) > 256 ||
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
	address    string // this route's own listener
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
	// A provider streams its headers at once. An MCP server answering a tool call with JSON sends
	// them only when the tool finishes — minutes for a long runbook — and a 502 there makes the
	// agent retry a mutation, so only the run's own lifetime bounds that wait.
	headerTimeout := credentialBrokerTimeout
	if route.Kind == CredentialBrokerMCP {
		headerTimeout = 0
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            b.dial,
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
	return b, nil
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
// secret header with exactly the stand-in issued for it. A request naming any OTHER credential
// header the broker itself would set — or the connection headers a proxy owns — is refused, so a
// box cannot ride its own Authorization upstream beside the credential. A header the broker does
// not set travels as written: the box already reaches this one upstream through this route, and the
// route's own credential is the only authority it gains.
func (b *credentialBroker) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
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

func (b *credentialBroker) Serve(ctx context.Context, ready func()) error {
	listener, err := net.Listen("tcp4", b.address)
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
