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
	maxCredentialBrokerSecret = 64 << 10
	maxCredentialBrokerBody   = 64 << 20
	maxCredentialBrokerFlows  = 32
	credentialBrokerTimeout   = 30 * time.Second
)

// CredentialBrokerSecret is mounted into the capless guard only. It is deliberately separate
// from LaunchConfig, which is also readable by the privileged controller.
type CredentialBrokerSecret struct {
	Version    int    `json:"version"`
	RunID      string `json:"run_id"`
	Epoch      string `json:"gateway_epoch"`
	Provider   string `json:"provider"`
	Substitute string `json:"substitute"`
	Credential string `json:"credential"`
}

func ReadCredentialBrokerSecret(reader io.Reader, config LaunchConfig) (CredentialBrokerSecret, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxCredentialBrokerSecret+1))
	if err != nil || len(data) == 0 || len(data) > maxCredentialBrokerSecret || config.Broker == nil {
		return CredentialBrokerSecret{}, Failure("credential_broker_configuration_invalid")
	}
	var value CredentialBrokerSecret
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF ||
		value.Version != 1 || value.RunID != config.RunID || value.Epoch != config.Epoch ||
		value.Provider != config.Broker.Provider || len(value.Substitute) < 32 || len(value.Substitute) > 256 ||
		len(value.Credential) < 8 || len(value.Credential) > 32<<10 || value.Substitute == value.Credential ||
		strings.ContainsAny(value.Substitute, "\x00\r\n") || strings.ContainsAny(value.Credential, "\x00\r\n") {
		return CredentialBrokerSecret{}, Failure("credential_broker_configuration_invalid")
	}
	return value, nil
}

type credentialBroker struct {
	route      CredentialBrokerRoute
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

func newCredentialBroker(config LaunchConfig, secret CredentialBrokerSecret, clock *BootClock, doh *DoH, events *GuardEvents, controller ControllerClient) (*credentialBroker, error) {
	if config.Broker == nil || !config.Broker.valid() || clock == nil || doh == nil || events == nil ||
		secret.RunID != config.RunID || secret.Epoch != config.Epoch || secret.Provider != config.Broker.Provider {
		return nil, Failure("credential_broker_configuration_invalid")
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, Failure("credential_broker_unavailable")
	}
	rule := egress.Rule{To: egress.Destination{Domain: config.Broker.Upstream}, Protocol: "tls", Ports: []int{config.Broker.Port}}
	policy, err := egress.Compile("broker", egress.Filtered, []egress.Input{{Rules: []egress.Rule{rule}, Origin: egress.Origin{Kind: "broker", Name: config.Broker.Provider}}}, nil, false, key[:])
	if err != nil {
		return nil, Failure("credential_broker_configuration_invalid")
	}
	resolver, err := NewResolver(policy, config.Protected, nil, clock, doh.Exchange)
	if err != nil {
		return nil, Failure("credential_broker_configuration_invalid")
	}
	b := &credentialBroker{route: *config.Broker, secret: secret, clock: clock, resolver: resolver,
		controller: controller, events: events, slots: make(chan struct{}, maxCredentialBrokerFlows)}
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
		ResponseHeaderTimeout:  credentialBrokerTimeout,
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
			request.Header.Del("Proxy-Authorization")
			request.Header.Del("Cookie")
			request.Header.Set(b.route.Header, b.secret.Credential)
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
				message = "credential broker refused an upstream redirect"
			} else {
				reason = "credential_broker_upstream_unavailable"
			}
			b.events.emit(GuardEvent{Kind: "admission_failed", Name: b.route.Upstream, Reason: reason})
			http.Error(w, message, http.StatusBadGateway)
		},
		FlushInterval: -1,
	}
}

func (b *credentialBroker) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != b.route.Method || request.URL.Path != b.route.Path || request.URL.RawQuery != "" || request.URL.IsAbs() ||
			request.Host != CredentialBrokerAddress || request.Header.Get("Authorization") != "" ||
			request.Header.Get("Proxy-Authorization") != "" || request.Header.Get("Cookie") != "" || request.Header.Get("Upgrade") != "" {
			http.Error(w, "credential broker request refused", http.StatusForbidden)
			return
		}
		values := request.Header.Values(b.route.Header)
		if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte(b.secret.Substitute)) != 1 {
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
	listener, err := net.Listen("tcp4", CredentialBrokerAddress)
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
	peer := resolution.Addresses[(b.peerCursor.Add(1)-1)%uint64(len(resolution.Addresses))]
	until, err := b.controller.AdmitBroker(admission, Lease{Name: resolution.Name, Peer: peer, Port: b.route.Port, Expires: resolution.Expires})
	if err == Failure("dns_ttl_expired") {
		resolution, err = b.resolver.refresh(admission, resolution)
		if err == nil && len(resolution.Addresses) != 0 {
			peer = resolution.Addresses[(b.peerCursor.Add(1)-1)%uint64(len(resolution.Addresses))]
			until, err = b.controller.AdmitBroker(admission, Lease{Name: resolution.Name, Peer: peer, Port: b.route.Port, Expires: resolution.Expires})
		}
	}
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
