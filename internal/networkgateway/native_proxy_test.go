package networkgateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestNativeOrdinaryProxyForwardsHTTPAndOwnsCONNECTCleanup(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/resource" || r.URL.RawQuery != "v=1" || r.Header.Get("X-Request") != "kept" {
				t.Error("HTTP forwarding changed request")
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, "upstream response")
		}))
		defer upstream.Close()
		proxy := newNativeOrdinaryProxy(t.Context(), nil)
		defer proxy.close()
		server := httptest.NewServer(proxy)
		defer server.Close()
		address, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		transport := &http.Transport{Proxy: http.ProxyURL(address)}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: wait.Deadline}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, upstream.URL+"/resource?v=1", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("X-Request", "kept")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusAccepted || string(body) != "upstream response" {
			t.Fatalf("response=%d %q %v", response.StatusCode, body, err)
		}
	})
	t.Run("connect cleanup", func(t *testing.T) {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		peer := make(chan net.Conn, 1)
		go func() {
			conn, err := listener.Accept()
			if err == nil {
				peer <- conn
			}
		}()
		proxy := newNativeOrdinaryProxy(context.Background(), nil)
		defer proxy.close()
		server := httptest.NewServer(proxy)
		defer server.Close()
		client := guardClient(t, strings.TrimPrefix(server.URL, "http://"))
		_, err = fmt.Fprintf(client, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", listener.Addr(), listener.Addr())
		if err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(client)
		response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("CONNECT=%+v %v", response, err)
		}
		var upstream net.Conn
		select {
		case upstream = <-peer:
		case <-time.After(wait.Deadline):
			t.Fatal("upstream not connected")
		}
		defer upstream.Close()
		_ = upstream.SetDeadline(time.Now().Add(wait.Deadline))
		exchangeBothWays(t, client, upstream)
		proxy.close()
		proxy.close()
		for name, conn := range map[string]net.Conn{"client": client, "upstream": upstream} {
			var b [1]byte
			_, err := conn.Read(b[:])
			if err == nil {
				t.Fatalf("%s survived close", name)
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatalf("%s was not actively closed", name)
			}
		}
		refused := httptest.NewRecorder()
		proxy.ServeHTTP(refused, httptest.NewRequest(http.MethodGet, "http://example.com/", nil))
		if refused.Code != 503 {
			t.Fatalf("closed proxy status=%d", refused.Code)
		}
	})
}

func TestNativeOrdinaryFilteredProxyRefusesBeforeResolving(t *testing.T) {
	fixture := startGuardFixture(t)
	var exchanges atomic.Int32
	original := fixture.guard.resolver.exchange
	fixture.guard.resolver.exchange = func(ctx context.Context, wire []byte) ([]byte, error) { exchanges.Add(1); return original(ctx, wire) }
	proxy := newNativeOrdinaryProxy(t.Context(), fixture.guard)
	defer proxy.close()
	for _, test := range []struct {
		method, target string
		authorize      bool
	}{
		{http.MethodGet, "http://api.example.com/", false},
		{http.MethodConnect, "forbidden.example.com:443", false},
		{http.MethodConnect, "api.example.com", false},
		{http.MethodConnect, "api.example.com:443", true},
	} {
		request := httptest.NewRequest(test.method, test.target, nil)
		if test.authorize {
			request.Header.Set("Proxy-Authorization", "Bearer refused")
		}
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, request)
		if response.Code != 403 {
			t.Fatalf("%s %s=%d", test.method, test.target, response.Code)
		}
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	client := guardClient(t, strings.TrimPrefix(server.URL, "http://"))
	if _, err := io.WriteString(client, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodConnect})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("CONNECT=%+v %v", response, err)
	}
	if err := writeAll(client, clientHello(t, "one.services.example.com")); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	_, err = client.Read(b[:])
	if err == nil {
		t.Fatal("mismatched SNI stayed open")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("mismatched SNI did not actively close")
	}
	if exchanges.Load() != 0 {
		t.Fatalf("denied proxy resolved upstream %d times", exchanges.Load())
	}
}
