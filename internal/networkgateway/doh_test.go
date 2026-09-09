package networkgateway

import (
	"bytes"
	"context"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
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
	doh, err := NewDoH(peer, "example.com", roots) // the httptest certificate covers example.com
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
