package cli

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"
)

func TestFleetPreviewRequiresItsOwnHostAndOrigin(t *testing.T) {
	const host = "secret.localhost:4567"
	for _, tc := range []struct {
		host, origin, site string
		allowed            bool
	}{
		{host, "", "none", true}, {host, "http://" + host, "same-origin", true},
		{host, "https://other.example", "", false}, {"elsewhere.localhost:4567", "", "", false},
		{host, "", "cross-site", false}, {host, "null", "", false},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = tc.host
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.site != "" {
			r.Header.Set("Sec-Fetch-Site", tc.site)
		}
		if previewRequestAllowed(r, host) != tc.allowed {
			t.Fatalf("unexpected preview admission for %+v", tc)
		}
	}
}

func TestPreviewGatewayAcceptsOnlyLocalAndPrivateControllers(t *testing.T) {
	for _, dest := range []string{"4322", "10.23.0.5:4322", "100.64.1.2:4322"} {
		if _, err := previewGateway(dest); err != nil {
			t.Fatal(dest, err)
		}
	}
	for _, dest := range []string{"0.0.0.0:4322", "8.8.8.8:4322", "10.23.0.5:0", "elsewhere:4322"} {
		if _, err := previewGateway(dest); err == nil {
			t.Fatalf("accepted %s", dest)
		}
	}
}

func TestPreviewForwardsBufferedUpgradePayload(t *testing.T) {
	const handshake = "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: fixture\r\n\r\n"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(conn, handshake+"hello"); err != nil {
			t.Error(err)
			return
		}
		_, _ = io.CopyN(conn, rw.Reader, 4)
	}))
	defer backend.Close()
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(previewResponseWriter{w}, r)
	}))
	defer front.Close()
	conn, err := net.DialTimeout("tcp", front.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: preview\r\nConnection: Upgrade\r\nUpgrade: fixture\r\n\r\nping"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	res, err := http.ReadResponse(reader, nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade failed: %v, %v", res, err)
	}
	buf := make([]byte, 9)
	if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "helloping" {
		t.Fatalf("upgraded stream lost buffered bytes: %q, %v", buf, err)
	}
}
