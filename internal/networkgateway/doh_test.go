package networkgateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/workertls"
)

func testDoH(t *testing.T, handler http.Handler) *DoH {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	peer, err := netip.ParseAddrPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	doh, err := NewDoH([]netip.AddrPort{peer}, "example.com", roots, nil) // the httptest certificate covers example.com
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(doh.Close)
	return doh
}

func TestDoHPinsPeerAndAuthenticatesServerWithoutEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	wire := bytes.Repeat([]byte{1}, 12)
	doh := testDoH(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Host != "example.com" || r.URL.Path != "/dns-query" || r.URL.RawQuery != "" || r.Method != "POST" ||
			r.Header.Get("Content-Type") != "application/dns-message" || !bytes.Equal(body, wire) {
			t.Error("DoH request changed fixed origin, method, path or message")
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(wire)
	}))
	got, err := doh.Exchange(context.Background(), wire)
	if err != nil || !bytes.Equal(got, wire) {
		t.Fatalf("pinned authenticated DoH: %v", err)
	}
	doh.Close()
	doh.transport.TLSClientConfig.ServerName = "wrong.example.net"
	if _, err := doh.Exchange(context.Background(), wire); err == nil {
		t.Fatal("resolver accepted wrong certificate identity")
	}
}

// On 2026-10-12 one office network answered 1.1.1.1:443 with a certificate that does not verify
// while 1.0.0.1:443 worked, so `coop net setup` failed and the worker refused every filtered
// job. The resolver's other address answers instead. A peer that cannot prove the pinned name
// never receives a query, and with no peer that can, nothing answers.
func TestDoHFallsBackOnlyToAPeerThatProvesTheName(t *testing.T) {
	wire := bytes.Repeat([]byte{1}, 12)
	var intercepted atomic.Int64
	interceptor := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		intercepted.Add(1)
	}))
	interceptor.TLS = &tls.Config{Certificates: []tls.Certificate{workertls.NewCA(t).ServerCertificate(t)}}
	interceptor.StartTLS()
	defer interceptor.Close()
	resolver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(wire)
	}))
	defer resolver.Close()
	roots := x509.NewCertPool()
	roots.AddCert(resolver.Certificate())
	peer := func(server *httptest.Server) netip.AddrPort {
		return netip.MustParseAddrPort(server.Listener.Addr().String())
	}

	doh, err := NewDoH([]netip.AddrPort{peer(interceptor), peer(resolver)}, "example.com", roots, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer doh.Close()
	if got, err := doh.Exchange(context.Background(), wire); err != nil || !bytes.Equal(got, wire) {
		t.Fatalf("resolver behind an intercepted first address: %v", err)
	}

	intercepting, err := NewDoH([]netip.AddrPort{peer(interceptor)}, "example.com", roots, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer intercepting.Close()
	if _, err := intercepting.Exchange(context.Background(), wire); err == nil {
		t.Fatal("resolver answered through a peer that did not prove its name")
	}
	if intercepted.Load() != 0 {
		t.Fatal("a query reached a peer that did not prove the pinned name")
	}
}

// A few hours later the same network stopped answering 1.1.1.1:443 at all: the connection
// hung, and trying the addresses in turn spent the whole three-second query budget on it. The
// resolver still answers through the other address, well inside that budget.
func TestDoHAnswersWhileAnotherPeerSwallowsTheConnection(t *testing.T) {
	wire := bytes.Repeat([]byte{1}, 12)
	silent, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	}()
	resolver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(wire)
	}))
	defer resolver.Close()
	roots := x509.NewCertPool()
	roots.AddCert(resolver.Certificate())
	peers := []netip.AddrPort{
		netip.MustParseAddrPort(silent.Addr().String()),
		netip.MustParseAddrPort(resolver.Listener.Addr().String()),
	}
	doh, err := NewDoH(peers, "example.com", roots, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer doh.Close()
	started := time.Now()
	if got, err := doh.Exchange(context.Background(), wire); err != nil || !bytes.Equal(got, wire) {
		t.Fatalf("resolver behind a silent first address: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= DNSQueryTimeout {
		t.Fatalf("the answer waited out the silent address: %s", elapsed)
	}
}

func TestDoHNeverFollowsRedirectsOrAcceptsUnboundedBodies(t *testing.T) {
	var redirected atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer other.Close()
	for _, behavior := range []string{"redirect", "content-type", "encoding", "large", "status"} {
		t.Run(behavior, func(t *testing.T) {
			doh := testDoH(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/dns-message")
				switch behavior {
				case "redirect":
					http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
				case "content-type":
					w.Header().Set("Content-Type", "text/plain")
					_, _ = w.Write(make([]byte, 12))
				case "encoding":
					w.Header().Set("Content-Encoding", "gzip")
					_, _ = w.Write(make([]byte, 12))
				case "large":
					_, _ = w.Write(make([]byte, MaxDNSMessage+1))
				case "status":
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			if _, err := doh.Exchange(context.Background(), make([]byte, 12)); err == nil {
				t.Fatalf("accepted %s DoH response", behavior)
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("resolver followed redirect to another peer")
	}
}
