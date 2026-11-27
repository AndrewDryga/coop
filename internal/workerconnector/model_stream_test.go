//lint:file-ignore SA1019 Public per-connection PING is provided by x/net/http2.

package workerconnector

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

type modelIdleConn struct{ net.Conn }

func (c modelIdleConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	return c.Conn.Read(p)
}

func TestModelStreamPingsOutliveInboundIdleDespiteTricklingData(t *testing.T) {
	for _, ping := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_ping_times_out", true: "ping_keeps_stream_alive"}[ping], func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			go (&http2.Server{}).ServeConn(modelIdleConn{server}, &http2.ServeConnOpts{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for range 10 {
					if _, err := io.WriteString(w, "data: tiny\n\n"); err != nil {
						return
					}
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
						return
					case <-time.After(30 * time.Millisecond):
					}
				}
			})})
			connection, err := new(http2.Transport).NewClientConn(client)
			if err != nil {
				t.Fatal(err)
			}
			body := &modelStreamBody{client: connection, done: make(chan struct{}), stopRequest: func() bool { return true }}
			defer body.Close()
			if ping {
				go body.keepAlive(t.Context(), 20*time.Millisecond, time.Second)
			}
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://controller.example/stream", nil)
			response, err := connection.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			raw, err := io.ReadAll(response.Body)
			if ping && (err != nil || strings.Count(string(raw), "tiny") != 10) {
				t.Fatalf("healthy trickling stream ended: %q %v", raw, err)
			}
			if !ping && err == nil {
				t.Fatal("fixture did not reproduce the inbound idle cutoff")
			}
		})
	}
}

func TestModelStreamCancellationClosesIdleUpstream(t *testing.T) {
	ended := make(chan struct{})
	web := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(ended)
	}))
	web.EnableHTTP2 = true
	web.StartTLS()
	defer web.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = web.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	transport.ResponseHeaderTimeout = time.Second
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, web.URL, nil)
	response, err := forwardModelStream(request, workerRoundTripper{transport})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	cancel()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("cancelled model stream left the upstream request running")
	}
}

func TestModelStreamMissingPongClosesConnection(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ended := make(chan struct{})
	pingSeen := make(chan struct{}, 1)
	go func() {
		defer close(ended)
		preface := make([]byte, len(http2.ClientPreface))
		if _, err := io.ReadFull(server, preface); err != nil {
			return
		}
		frames := http2.NewFramer(server, server)
		// net.Pipe has no kernel buffers: read the client's preface frames
		// while our SETTINGS write waits for its reader to start.
		go func() { _ = frames.WriteSettings() }()
		for {
			frame, err := frames.ReadFrame()
			if err != nil {
				return
			}
			if _, ok := frame.(*http2.PingFrame); ok {
				select {
				case pingSeen <- struct{}{}:
				default:
				}
			}
			// Deliberately withhold PING acknowledgements.
		}
	}()
	connection, err := new(http2.Transport).NewClientConn(client)
	if err != nil {
		t.Fatal(err)
	}
	body := &modelStreamBody{client: connection, done: make(chan struct{}), stopRequest: func() bool { return true }}
	defer body.Close()
	stopped := make(chan struct{})
	go func() {
		body.keepAlive(t.Context(), 10*time.Millisecond, 50*time.Millisecond)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("lost PONG did not stop the heartbeat and close its connection")
	}
	select {
	case <-pingSeen:
	default:
		t.Fatal("fixture did not observe a PING")
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("missing PONG left the wire connection open")
	}
}

func TestModelStreamWebSocketUsesHTTP1AfterHTTP2WasInitialized(t *testing.T) {
	web := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prime" {
			if r.ProtoMajor != 2 {
				t.Error("fixture did not initialize the original HTTP/2 transport")
			}
			return
		}
		if r.ProtoMajor != 1 || r.Header.Get("Upgrade") != "websocket" {
			t.Error("WebSocket was not sent as an HTTP/1 upgrade")
			http.Error(w, "upgrade required", http.StatusBadRequest)
			return
		}
		connection, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = buffered.Flush()
		_, _ = io.Copy(connection, buffered)
	}))
	web.EnableHTTP2 = true
	web.StartTLS()
	defer web.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = web.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	transport.ForceAttemptHTTP2 = true
	defer transport.CloseIdleConnections()
	prime, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, web.URL+"/prime", nil)
	primed, err := transport.RoundTrip(prime)
	if err != nil {
		t.Fatal(err)
	}
	_ = primed.Body.Close()
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, web.URL+"/socket", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	response, err := forwardModelStream(request, workerRoundTripper{transport})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	duplex, ok := response.Body.(io.ReadWriteCloser)
	if response.StatusCode != http.StatusSwitchingProtocols || !ok {
		t.Fatalf("upgrade did not retain its duplex body: %d %T", response.StatusCode, response.Body)
	}
	if _, err := io.WriteString(duplex, "hello\n"); err != nil {
		t.Fatal(err)
	}
	if reply, err := bufio.NewReader(duplex).ReadString('\n'); err != nil || reply != "hello\n" {
		t.Fatalf("upgrade did not carry bidirectional data: %q %v", reply, err)
	}
}

func TestModelStreamRejectsUnsupportedUpgradesBeforeConnecting(t *testing.T) {
	for _, values := range [][]string{{"other"}, {"websocket", "websocket"}, {"websocket, other"}, {""}} {
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://controller.invalid/stream", nil)
		request.Header["Upgrade"] = values
		request.Header.Set("Connection", "Upgrade")
		if _, err := forwardModelStream(request, workerRoundTripper{http.DefaultTransport.(*http.Transport)}); err == nil || err.Error() != "unsupported model connection upgrade" {
			t.Fatalf("unsupported upgrade %q reached the connection: %v", values, err)
		}
	}
}
