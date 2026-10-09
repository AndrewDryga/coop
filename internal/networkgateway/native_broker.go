package networkgateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

type NativeBrokerBinding struct {
	RunID, Provider, Account string
	Epoch                    uint64
}

// Credentials and private keys are guard-only, never controller or agent config.
type NativeAccessSnapshot struct {
	Binding    NativeBrokerBinding
	Revision   uint64
	Expires    time.Time
	Revoked    bool
	Credential string
}

type NativeBrokerRequestLine struct {
	CredentialFree       bool
	Method, Path, Query  string
	Header, HeaderPrefix string
	// Public native selector, not a capability or grant.
	ClientHeader, ClientPrefix, ClientMarker string
	Segment, Suffix                          string
	Queries                                  []NativeBrokerQuery
}

type NativeBrokerQuery struct {
	Name     string
	Values   []string
	Optional bool
	MaxBytes int
}

func (line NativeBrokerRequestLine) matches(request *http.Request) bool {
	target := request.URL
	return target != nil && !target.IsAbs() && target.Host == "" && target.User == nil &&
		target.Fragment == "" && !target.ForceQuery && target.RawPath == "" &&
		request.Method == line.Method && line.matchesPath(target.Path) && line.matchesQuery(target.RawQuery)
}

func (line NativeBrokerRequestLine) matchesPath(value string) bool {
	if line.Segment == "" {
		return value == line.Path
	}
	if !strings.HasPrefix(value, line.Path) || !strings.HasSuffix(value, line.Suffix) {
		return false
	}
	segment := strings.TrimSuffix(strings.TrimPrefix(value, line.Path), line.Suffix)
	if segment == "" || len(segment) > 1024 {
		return false
	}
	switch line.Segment {
	case "uuid":
		return len(segment) == 36 && segment[8] == '-' && segment[13] == '-' && segment[18] == '-' && segment[23] == '-' &&
			strings.Trim(strings.ReplaceAll(segment, "-", ""), "0123456789abcdef") == "" && strings.Count(segment, "-") == 4
	case "token":
		return segment != "." && segment != ".." && strings.Trim(segment, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") == ""
	case "segments":
		for _, part := range strings.Split(segment, "/") {
			if part == "" || part == "." || part == ".." || strings.Trim(part, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.~:") != "" {
				return false
			}
		}
		return true
	}
	return false
}

func (line NativeBrokerRequestLine) matchesQuery(raw string) bool {
	if len(line.Queries) == 0 {
		return raw == line.Query
	}
	values, err := url.ParseQuery(raw)
	if err != nil || len(raw) > 8192 {
		return false
	}
	for _, rule := range line.Queries {
		value, present := values[rule.Name]
		if !present {
			if rule.Optional {
				continue
			}
			return false
		}
		if len(value) != 1 || strings.ContainsAny(value[0], "\x00\r\n") ||
			!(slices.Contains(rule.Values, value[0]) || rule.MaxBytes > 0 && len(value[0]) <= rule.MaxBytes) {
			return false
		}
		delete(values, rule.Name)
	}
	return len(values) == 0
}

func (line NativeBrokerRequestLine) valid() bool {
	if line.Segment != "" && line.Segment != "uuid" && line.Segment != "token" && line.Segment != "segments" ||
		strings.ContainsAny(line.Suffix, "?#% \t\x00\r\n") || len(line.Queries) > 8 || len(line.Queries) > 0 && line.Query != "" {
		return false
	}
	names := map[string]bool{}
	for _, rule := range line.Queries {
		if rule.Name == "" || names[rule.Name] || strings.Trim(rule.Name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") != "" ||
			len(rule.Values) == 0 && rule.MaxBytes == 0 || rule.MaxBytes < 0 || rule.MaxBytes > 4096 {
			return false
		}
		names[rule.Name] = true
	}
	switch line.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
	default:
		return false
	}
	clean := path.Clean(line.Path)
	return strings.HasPrefix(line.Path, "/") && (clean == line.Path || clean+"/" == line.Path) &&
		!strings.ContainsAny(line.Path, "?#% \t\x00\r\n") && !strings.ContainsAny(line.Query, "# \t\x00\r\n") &&
		(line.CredentialFree && line.Header == "" && line.HeaderPrefix == "" && line.ClientHeader == "" && line.ClientPrefix == "" && line.ClientMarker == "" ||
			!line.CredentialFree && mcpSecretHeader(strings.ToLower(line.Header))) && len(line.HeaderPrefix) <= MaxMCPHeaderPrefix &&
		len(line.ClientPrefix) <= MaxMCPHeaderPrefix && len(line.ClientMarker) <= 32<<10 &&
		!strings.ContainsAny(line.HeaderPrefix+line.ClientPrefix+line.ClientMarker, "\x00\r\n") &&
		(line.ClientMarker == "" && line.ClientHeader == "" ||
			line.ClientMarker != "" && mcpSecretHeader(strings.ToLower(line.ClientHeader)))
}

type NativeBrokerOrigin struct {
	Host        string
	Certificate tls.Certificate
	Requests    []NativeBrokerRequestLine
	// Immutable selected-account facts, e.g. ChatGPT-Account-ID.
	AccountHeaders map[string]string
	// Public native account selector, distinct from real upstream identity.
	ClientAccountHeaders map[string]string
	// Existing filtered admission or open direct dial, never guest input.
	DialContext func(context.Context, string, string) (net.Conn, error)
}

type nativeBrokerRoute struct {
	line  NativeBrokerRequestLine
	proxy *httputil.ReverseProxy
}

type nativeBrokerOrigin struct {
	certificate          tls.Certificate
	accountHeaders       http.Header
	clientAccountHeaders http.Header
	routes               []nativeBrokerRoute
	transport            *http.Transport
}

type NativeBroker struct {
	binding         NativeBrokerBinding
	load            func(context.Context) (NativeAccessSnapshot, error)
	fallback        http.Handler
	origins         map[string]nativeBrokerOrigin
	ctx             context.Context
	cancel          context.CancelFunc
	server          *http.Server
	input           *nativeBrokerListener
	conns, requests chan struct{}
	mu              sync.Mutex
	last            NativeAccessSnapshot
	stopped         bool
	active          map[*nativeBrokerConn]struct{}
	once            sync.Once
	workers         sync.WaitGroup
}

type nativeAccessKey struct{}

// NewNativeBroker terminates only declared original native origins. It owns no
// TCP listener: the selected run namespace supplies the one outer entrance.
func NewNativeBroker(parent context.Context, binding NativeBrokerBinding, origins []NativeBrokerOrigin,
	load func(context.Context) (NativeAccessSnapshot, error), fallback http.Handler) (*NativeBroker, error) {
	if parent == nil || load == nil || fallback == nil || len(origins) == 0 || binding.RunID == "" ||
		binding.Provider == "" || binding.Account == "" || binding.Epoch == 0 {
		return nil, Failure("native_broker_configuration_invalid")
	}
	ctx, cancel := context.WithCancel(parent)
	b := &NativeBroker{binding: binding, load: load, fallback: fallback, origins: map[string]nativeBrokerOrigin{},
		ctx: ctx, cancel: cancel, input: &nativeBrokerListener{accepted: make(chan net.Conn, maxCredentialBrokerFlows),
			done: make(chan struct{}), ready: make(chan struct{})},
		conns: make(chan struct{}, maxCredentialBrokerFlows), requests: make(chan struct{}, maxCredentialBrokerFlows),
		active: map[*nativeBrokerConn]struct{}{}}
	fail := func() (*NativeBroker, error) {
		b.stopProtected()
		return nil, Failure("native_broker_configuration_invalid")
	}
	for _, declared := range origins {
		host, err := egress.NormalizeDomain(declared.Host, false)
		if err != nil || host != declared.Host || declared.DialContext == nil || len(declared.Requests) == 0 ||
			len(declared.Certificate.Certificate) == 0 || declared.Certificate.PrivateKey == nil {
			return fail()
		}
		if _, duplicate := b.origins[host]; duplicate {
			return fail()
		}
		leaf, err := x509.ParseCertificate(declared.Certificate.Certificate[0])
		if err != nil || leaf.VerifyHostname(host) != nil {
			return fail()
		}
		accountHeaders := make(http.Header)
		for name, value := range declared.AccountHeaders {
			key, lower := http.CanonicalHeaderKey(name), strings.ToLower(name)
			if !mcpSecretHeader(lower) || value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") ||
				lower == "authorization" || lower == "x-api-key" || lower == "x-goog-api-key" || accountHeaders.Get(key) != "" {
				return fail()
			}
			accountHeaders.Set(key, value)
		}
		dial := declared.DialContext
		transport := &http.Transport{Proxy: nil,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				if address != net.JoinHostPort(host, "443") || network != "tcp" && network != "tcp4" && network != "tcp6" {
					return nil, Failure("native_broker_request_refused")
				}
				admission, done := context.WithTimeout(ctx, GuardAdmissionTimeout)
				defer done()
				return dial(admission, network, address)
			},
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host},
			ForceAttemptHTTP2: true, DisableCompression: true, MaxConnsPerHost: maxCredentialBrokerFlows,
			MaxIdleConnsPerHost: maxCredentialBrokerFlows, MaxIdleConns: maxCredentialBrokerFlows,
			IdleConnTimeout: time.Minute, TLSHandshakeTimeout: credentialBrokerTimeout, MaxResponseHeaderBytes: 1 << 20}
		clientAccountHeaders := accountHeaders.Clone()
		for name, value := range declared.ClientAccountHeaders {
			if accountHeaders.Get(name) == "" || value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
				return fail()
			}
			clientAccountHeaders.Set(name, value)
		}
		origin := nativeBrokerOrigin{certificate: declared.Certificate, accountHeaders: accountHeaders, clientAccountHeaders: clientAccountHeaders, transport: transport}
		b.origins[host] = origin // establish cleanup ownership before further validation
		for _, line := range declared.Requests {
			if !line.valid() || accountHeaders.Get(line.Header) != "" || line.ClientHeader != "" && accountHeaders.Get(line.ClientHeader) != "" {
				return fail()
			}
			for _, prior := range origin.routes {
				if prior.line.Method == line.Method && prior.line.Path == line.Path && prior.line.Query == line.Query &&
					prior.line.Segment == line.Segment && prior.line.Suffix == line.Suffix && reflect.DeepEqual(prior.line.Queries, line.Queries) {
					return fail()
				}
			}
			proxy := &httputil.ReverseProxy{Transport: transport,
				Rewrite: func(p *httputil.ProxyRequest) {
					access := p.In.Context().Value(nativeAccessKey{}).(NativeAccessSnapshot)
					p.Out.URL.Scheme, p.Out.URL.Host, p.Out.Host = "https", host, host
					for _, header := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key", "X-Goog-Api-Key", line.Header, line.ClientHeader} {
						if header != "" {
							p.Out.Header.Del(header)
						}
					}
					for name, values := range accountHeaders {
						p.Out.Header.Del(name)
						if !line.CredentialFree {
							p.Out.Header.Set(name, values[0])
						}
					}
					if !line.CredentialFree {
						p.Out.Header.Set(line.Header, line.HeaderPrefix+access.Credential)
					}
				},
				ModifyResponse: func(response *http.Response) error {
					response.Header.Del("Set-Cookie")
					if response.StatusCode >= 300 && response.StatusCode < 400 && response.StatusCode != http.StatusNotModified {
						return Failure("native_broker_redirect_refused")
					}
					return nil
				},
				ErrorHandler: func(w http.ResponseWriter, request *http.Request, _ error) {
					if request.Context().Err() == nil {
						http.Error(w, "native provider unavailable", http.StatusBadGateway)
					}
				}, FlushInterval: -1}
			origin.routes = append(origin.routes, nativeBrokerRoute{line: line, proxy: proxy})
		}
		b.origins[host] = origin
	}
	b.server = &http.Server{Handler: http.HandlerFunc(b.serveNative), BaseContext: func(net.Listener) context.Context { return b.ctx },
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}},
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 1 << 20}
	if _, err := b.current(); err != nil {
		return nil, err
	}
	b.workers.Add(2)
	go func() { defer b.workers.Done(); _ = b.server.Serve(b.input); b.stopProtected() }()
	go func() {
		defer b.workers.Done()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-b.ctx.Done():
				b.stopProtected()
				return
			case <-ticker.C:
				if _, err := b.current(); err != nil {
					return
				}
			}
		}
	}()
	select {
	case <-b.input.ready:
		return b, nil
	case <-b.ctx.Done():
		_ = b.Close()
		return nil, Failure("native_broker_unavailable")
	}
}

func validNativeSnapshot(binding NativeBrokerBinding, previous, next NativeAccessSnapshot, now time.Time) bool {
	return next.Binding == binding && !next.Revoked && next.Revision > 0 && next.Revision >= previous.Revision && next.Expires.After(now) &&
		len(next.Credential) > 0 && len(next.Credential) <= 32<<10 && !strings.ContainsAny(next.Credential, "\x00\r\n") &&
		(next.Revision != previous.Revision || next.Credential == previous.Credential && next.Expires.Equal(previous.Expires))
}

func (b *NativeBroker) current() (NativeAccessSnapshot, error) {
	// Serialize reads and comparisons so completion order cannot look like rollback.
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return NativeAccessSnapshot{}, Failure("native_broker_revoked")
	}
	ctx, done := context.WithTimeout(b.ctx, time.Second)
	next, err := b.load(ctx)
	loadErr := ctx.Err()
	done()
	valid := err == nil && loadErr == nil && b.ctx.Err() == nil && validNativeSnapshot(b.binding, b.last, next, time.Now())
	if valid {
		b.last = next
	}
	b.mu.Unlock()
	if !valid {
		b.stopProtected()
		return NativeAccessSnapshot{}, Failure("native_broker_revoked")
	}
	return next, nil
}

func (b *NativeBroker) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodConnect {
		b.fallback.ServeHTTP(w, request)
		return
	}
	host, port, err := net.SplitHostPort(request.Host)
	origin, protected := b.origins[host]
	if !protected {
		b.fallback.ServeHTTP(w, request)
		return
	}
	if err != nil || port != "443" || request.RequestURI != request.Host || request.URL.Host != request.Host ||
		request.ContentLength > 0 || len(request.TransferEncoding) != 0 || request.Header.Get("Proxy-Authorization") != "" {
		http.Error(w, "native CONNECT refused", http.StatusForbidden)
		return
	}
	if _, err := b.current(); err != nil {
		http.Error(w, "native account unavailable", http.StatusForbidden)
		return
	}
	select {
	case b.conns <- struct{}{}:
	default:
		http.Error(w, "native provider busy", http.StatusServiceUnavailable)
		return
	}
	conn, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		<-b.conns
		http.Error(w, "native CONNECT unavailable", http.StatusInternalServerError)
		return
	}
	pending := make([]byte, buffered.Reader.Buffered())
	if _, err = io.ReadFull(buffered.Reader, pending); err != nil {
		_ = conn.Close()
		<-b.conns
		return
	}
	tracked := &nativeBrokerConn{Conn: conn}
	tracked.closed = func() { b.mu.Lock(); delete(b.active, tracked); b.mu.Unlock(); <-b.conns }
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		_ = conn.Close()
		<-b.conns
		return
	}
	b.active[tracked] = struct{}{}
	b.mu.Unlock()
	fail := func() { _ = tracked.Close() }
	_ = conn.SetWriteDeadline(time.Now().Add(GuardAdmissionTimeout))
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		fail()
		return
	}
	if buffered.Flush() != nil {
		fail()
		return
	}
	_ = conn.SetWriteDeadline(time.Time{})
	secure := tls.Server(&prefixedConn{Conn: tracked, prefix: pending}, &tls.Config{
		MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName != host {
				return nil, Failure("native_broker_tls_name_mismatch")
			}
			return &origin.certificate, nil
		},
	})
	handshake, done := context.WithTimeout(b.ctx, GuardAdmissionTimeout)
	err = secure.HandshakeContext(handshake)
	done()
	if err != nil {
		fail()
		return
	}
	select {
	case b.input.accepted <- secure:
	case <-b.ctx.Done():
		fail()
	}
}

func (b *NativeBroker) serveNative(w http.ResponseWriter, request *http.Request) {
	if request.TLS == nil {
		http.Error(w, "native TLS required", http.StatusForbidden)
		return
	}
	host := request.TLS.ServerName
	origin, ok := b.origins[host]
	if !ok || request.Host != host && request.Host != host+":443" {
		http.Error(w, "native origin refused", http.StatusForbidden)
		return
	}
	for name, expected := range origin.clientAccountHeaders {
		values := request.Header.Values(name)
		if len(values) != 0 && (len(values) != 1 || values[0] != expected[0]) {
			http.Error(w, "native account selector refused", http.StatusForbidden)
			return
		}
	}
	for _, route := range origin.routes {
		if !route.line.matches(request) {
			continue
		}
		if route.line.ClientMarker != "" {
			values := request.Header.Values(route.line.ClientHeader)
			if len(values) != 1 || values[0] != route.line.ClientPrefix+route.line.ClientMarker {
				http.Error(w, "native account selector refused", http.StatusForbidden)
				return
			}
		}
		select {
		case b.requests <- struct{}{}:
			defer func() { <-b.requests }()
		default:
			http.Error(w, "native provider busy", http.StatusServiceUnavailable)
			return
		}
		access, err := b.current()
		if err != nil {
			http.Error(w, "native account unavailable", http.StatusForbidden)
			return
		}
		request = request.WithContext(context.WithValue(request.Context(), nativeAccessKey{}, access))
		request.Body = http.MaxBytesReader(w, request.Body, maxCredentialBrokerBody)
		route.proxy.ServeHTTP(w, request)
		return
	}
	http.Error(w, "native route refused", http.StatusForbidden)
}

func (b *NativeBroker) stopProtected() {
	b.once.Do(func() {
		b.mu.Lock()
		b.stopped = true
		conns := make([]*nativeBrokerConn, 0, len(b.active))
		for conn := range b.active {
			conns = append(conns, conn)
		}
		b.last = NativeAccessSnapshot{}
		b.mu.Unlock()
		b.cancel()
		if b.server != nil {
			_ = b.server.Close()
		}
		_ = b.input.Close()
		for _, conn := range conns {
			_ = conn.Close()
		}
		for _, origin := range b.origins {
			origin.transport.CloseIdleConnections()
		}
	})
}

func (b *NativeBroker) Close() error { b.stopProtected(); b.workers.Wait(); return nil }

type nativeBrokerConn struct {
	net.Conn
	once   sync.Once
	closed func()
}

func (c *nativeBrokerConn) Close() error { err := c.Conn.Close(); c.once.Do(c.closed); return err }

// This listener accepts already-connected TLS sockets; it never binds TCP.
type nativeBrokerListener struct {
	accepted        chan net.Conn
	done, ready     chan struct{}
	once, readyOnce sync.Once
}

func (l *nativeBrokerListener) Accept() (net.Conn, error) {
	l.readyOnce.Do(func() { close(l.ready) })
	select {
	case <-l.done:
		return nil, net.ErrClosed
	default:
	}
	select {
	case conn := <-l.accepted:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *nativeBrokerListener) Close() error { l.once.Do(func() { close(l.done) }); return nil }
func (*nativeBrokerListener) Addr() net.Addr { return nativeBrokerAddr{} }

type nativeBrokerAddr struct{}

func (nativeBrokerAddr) Network() string { return "native" }
func (nativeBrokerAddr) String() string  { return "accepted-native-tls" }
