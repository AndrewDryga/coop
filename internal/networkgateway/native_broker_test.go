package networkgateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

type nativeTransportFixture struct {
	broker          *NativeBroker
	proxy, upstream *httptest.Server
	roots           *x509.CertPool
	client          *http.Client
	mu              sync.Mutex
	snapshot        NativeAccessSnapshot
	forwarded       atomic.Int64
}

func newNativeTransportFixture(t *testing.T, handler http.Handler) *nativeTransportFixture {
	t.Helper()
	f := &nativeTransportFixture{}
	f.upstream = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.forwarded.Add(1)
		handler.ServeHTTP(w, r)
	}))
	f.upstream.EnableHTTP2 = true
	f.upstream.StartTLS()
	t.Cleanup(f.upstream.Close)
	f.roots = x509.NewCertPool()
	f.roots.AddCert(f.upstream.Certificate())
	binding := NativeBrokerBinding{RunID: "run-one", Provider: "codex", Account: "selected", Epoch: 7}
	f.snapshot = NativeAccessSnapshot{Binding: binding, Revision: 1, Expires: time.Now().Add(time.Hour), Credential: "private-access-one"}
	var routes []NativeBrokerRequestLine
	for _, target := range []struct{ path, query string }{{"/http", ""}, {"/http", "fixed=1"}, {"/sse", ""}, {"/ws", ""}, {"/redirect", ""}} {
		routes = append(routes, NativeBrokerRequestLine{Method: http.MethodGet, Path: target.path, Query: target.query,
			Header: "Authorization", HeaderPrefix: "Bearer ", ClientHeader: "Authorization", ClientPrefix: "Bearer ", ClientMarker: "public-selector"})
	}
	var err error
	f.broker, err = NewNativeBroker(context.Background(), binding, []NativeBrokerOrigin{{
		Host: "example.com", Certificate: f.upstream.TLS.Certificates[0], Requests: routes,
		AccountHeaders: map[string]string{"ChatGPT-Account-ID": "selected-id"},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "example.com:443" {
				return nil, fmt.Errorf("unexpected native address: %s", address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, f.upstream.Listener.Addr().String())
		},
	}}, func(context.Context) (NativeAccessSnapshot, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.snapshot, nil
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ordinary") }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.broker.Close() })
	// Trust only the fixture upstream CA, not an insecure verification mode.
	f.broker.origins["example.com"].transport.TLSClientConfig.RootCAs = f.roots
	f.proxy = httptest.NewServer(f.broker)
	t.Cleanup(f.proxy.Close)
	transport := &http.Transport{ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{RootCAs: f.roots, ServerName: "example.com", MinVersion: tls.VersionTLS12},
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return f.tunnel(ctx, "example.com", []string{"h2", "http/1.1"})
		}}
	t.Cleanup(transport.CloseIdleConnections)
	f.client = &http.Client{Transport: transport, Timeout: wait.Deadline}
	return f
}

func (f *nativeTransportFixture) change(fn func(*NativeAccessSnapshot)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.snapshot)
}

func (f *nativeTransportFixture) tunnel(ctx context.Context, sni string, protocols []string) (*tls.Conn, error) {
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(f.proxy.URL, "http://"))
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*tls.Conn, error) { _ = raw.Close(); return nil, err }
	_ = raw.SetDeadline(time.Now().Add(wait.Deadline))
	if _, err := io.WriteString(raw, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"); err != nil {
		return fail(err)
	}
	reader := bufio.NewReader(raw)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return fail(err)
	}
	if response.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("CONNECT status %d", response.StatusCode))
	}
	pending := make([]byte, reader.Buffered())
	if _, err := io.ReadFull(reader, pending); err != nil {
		return fail(err)
	}
	conn := tls.Client(&prefixedConn{Conn: raw, prefix: pending}, &tls.Config{
		RootCAs: f.roots, ServerName: sni, MinVersion: tls.VersionTLS12, NextProtos: protocols})
	if err := conn.HandshakeContext(ctx); err != nil {
		return fail(err)
	}
	_ = raw.SetDeadline(time.Time{})
	return conn, nil
}

func (f *nativeTransportFixture) request(t *testing.T, target string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "https://example.com"+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer public-selector")
	request.Header.Set("ChatGPT-Account-ID", "selected-id")
	return request
}

func nativeResponse(t *testing.T, client *http.Client, request *http.Request) (*http.Response, string) {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return response, string(body)
}

func TestNativeBrokerRealCONNECTHTTP2AndExactBoundary(t *testing.T) {
	f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "example.com" || r.TLS == nil || r.TLS.ServerName != "example.com" {
			t.Error("native original origin changed")
		}
		for _, name := range []string{"Cookie", "Proxy-Authorization", "X-Api-Key", "X-Goog-Api-Key", "Forwarded", "X-Forwarded-For"} {
			if r.Header.Get(name) != "" {
				t.Errorf("guest credential/forwarded header survived: %s", name)
			}
		}
		w.Header().Set("X-Test-Authorization", r.Header.Get("Authorization"))
		w.Header().Set("X-Test-Account", r.Header.Get("ChatGPT-Account-ID"))
		w.Header().Set("X-Test-Protocol", fmt.Sprint(r.ProtoMajor))
		w.Header().Set("Set-Cookie", "provider-secret=not-for-guest")
		_, _ = io.WriteString(w, "native")
	}))
	request := f.request(t, "/http?fixed=1")
	for _, name := range []string{"Cookie", "Proxy-Authorization", "X-Api-Key", "X-Goog-Api-Key", "Forwarded", "X-Forwarded-For"} {
		request.Header.Set(name, "guest-value")
	}
	response, body := nativeResponse(t, f.client, request)
	if response.StatusCode != http.StatusOK || body != "native" || response.ProtoMajor != 2 || response.Header.Get("X-Test-Protocol") != "2" ||
		response.Header.Get("X-Test-Authorization") != "Bearer private-access-one" || response.Header.Get("X-Test-Account") != "selected-id" || response.Header.Get("Set-Cookie") != "" {
		t.Fatalf("native h2/credential mediation failed: status=%d proto=%d body=%q", response.StatusCode, response.ProtoMajor, body)
	}
	before := f.forwarded.Load()
	for _, test := range []struct {
		name, target string
		alter        func(*http.Request)
	}{
		{"path", "/sibling", nil}, {"query", "/http?fixed=2", nil}, {"extra-query", "/http?fixed=1&extra=1", nil}, {"encoded-path", "/%68ttp", nil},
		{"host", "/http", func(r *http.Request) { r.Host = "sibling.example.com" }},
		{"method", "/http", func(r *http.Request) { r.Method = http.MethodPost }},
		{"wrong-account", "/http", func(r *http.Request) { r.Header.Set("ChatGPT-Account-ID", "sibling-id") }},
		{"duplicate-account", "/http", func(r *http.Request) { r.Header.Add("ChatGPT-Account-ID", "selected-id") }},
		{"wrong-marker", "/http", func(r *http.Request) { r.Header.Set("Authorization", "Bearer sibling-selector") }},
		{"duplicate-marker", "/http", func(r *http.Request) { r.Header.Add("Authorization", "Bearer public-selector") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := f.request(t, test.target)
			if test.alter != nil {
				test.alter(request)
			}
			response, _ := nativeResponse(t, f.client, request)
			if response.StatusCode != http.StatusForbidden {
				t.Fatalf("admitted %s: %d", test.name, response.StatusCode)
			}
		})
	}
	if f.forwarded.Load() != before {
		t.Fatal("denied native request reached upstream")
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
	defer cancel()
	conn, err := f.tunnel(ctx, "wrong.example.com", []string{"http/1.1"})
	if err == nil {
		_ = conn.Close()
		t.Fatal("CONNECT/SNI mismatch admitted")
	}
	if err := f.upstream.Certificate().VerifyHostname("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	conn, err = f.tunnel(ctx, "127.0.0.1", []string{"http/1.1"})
	if err == nil {
		_ = conn.Close()
		t.Fatal("missing SNI admitted")
	}
}

func TestNativeBrokerVerifiedUpstreamTLSAndNoRedirect(t *testing.T) {
	t.Run("untrusted-upstream", func(t *testing.T) {
		f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "unexpected") }))
		f.broker.origins["example.com"].transport.TLSClientConfig.RootCAs = x509.NewCertPool()
		response, _ := nativeResponse(t, f.client, f.request(t, "/http"))
		if response.StatusCode != http.StatusBadGateway || f.forwarded.Load() != 0 {
			t.Fatal("untrusted provider TLS was accepted")
		}
	})
	t.Run("redirect", func(t *testing.T) {
		f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://sibling.example.com/http", http.StatusTemporaryRedirect)
		}))
		response, _ := nativeResponse(t, f.client, f.request(t, "/redirect"))
		if response.StatusCode != http.StatusBadGateway || f.forwarded.Load() != 1 {
			t.Fatal("provider redirect escaped original route")
		}
	})
}
