//lint:file-ignore SA1019 x/net exposes the public per-connection PING operation that net/http does not yet expose.

package workerconnector

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

// A model can be silent, or send only tiny events, for minutes. Regular PINGs
// keep its dedicated connection alive even when incoming DATA would suppress
// net/http's idle-read PING. Closing the response owns all connection cleanup.
func forwardModelStream(request *http.Request, original workerRoundTripper) (*http.Response, error) {
	// Clone waits for the ordinary transport's HTTP/2 initialization before
	// snapshotting TLS configuration shared with concurrent polls and grants.
	transport := original.Transport.Clone()
	upgrades := request.Header.Values("Upgrade")
	if len(upgrades) != 0 {
		connectionUpgrade := false
		for _, value := range request.Header.Values("Connection") {
			for token := range strings.SplitSeq(value, ",") {
				connectionUpgrade = connectionUpgrade || strings.EqualFold(strings.TrimSpace(token), "upgrade")
			}
		}
		if len(upgrades) != 1 || upgrades[0] != "websocket" || request.Method != http.MethodGet || !connectionUpgrade {
			if request.Body != nil {
				_ = request.Body.Close()
			}
			return nil, errors.New("unsupported model connection upgrade")
		}
		// Native WebSocket upgrades use HTTP/1.1; their handler owns ping/pong.
		dedicated := transport
		dedicated.ForceAttemptHTTP2 = false
		dedicated.Protocols = new(http.Protocols)
		dedicated.Protocols.SetHTTP1(true)
		dedicated.TLSNextProto = nil
		if dedicated.TLSClientConfig == nil {
			dedicated.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
		}
		dedicated.TLSClientConfig.NextProtos = []string{"http/1.1"}
		dedicated.DisableKeepAlives = true
		defer dedicated.CloseIdleConnections()
		return (workerRoundTripper{dedicated}).RoundTrip(request)
	}
	handedOff := false
	defer func() {
		if !handedOff && request.Body != nil {
			_ = request.Body.Close()
		}
	}()

	address := request.URL.Host
	if request.URL.Port() == "" {
		address = net.JoinHostPort(request.URL.Hostname(), "443")
	}
	setup := transport.TLSHandshakeTimeout
	if setup <= 0 {
		setup = 10 * time.Second
	}
	admission, cancel := context.WithTimeout(request.Context(), setup)
	defer cancel()
	dial := transport.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	connection, err := dial(admission, "tcp", address)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13}
	if transport.TLSClientConfig != nil {
		tlsConfig = transport.TLSClientConfig.Clone()
	}
	tlsConfig.NextProtos = []string{"h2"}
	if tlsConfig.ServerName == "" {
		tlsConfig.ServerName = request.URL.Hostname()
	}
	secure := tls.Client(connection, tlsConfig)
	stopRequest := context.AfterFunc(request.Context(), func() { _ = secure.Close() })
	fail := func() {
		stopRequest()
		_ = secure.Close()
	}
	if err = secure.HandshakeContext(admission); err != nil {
		fail()
		return nil, err
	}
	if secure.ConnectionState().NegotiatedProtocol != "h2" {
		fail()
		return nil, errors.New("controller model streams require HTTP/2")
	}
	deadline, _ := admission.Deadline()
	_ = secure.SetDeadline(deadline)
	h2 := &http2.Transport{MaxHeaderListSize: 64 << 10, DisableCompression: true}
	client, err := h2.NewClientConn(secure)
	_ = secure.SetDeadline(time.Time{})
	if err != nil {
		fail()
		return nil, err
	}

	body := &modelStreamBody{done: make(chan struct{}), client: client, stopRequest: stopRequest}
	go body.keepAlive(request.Context(), 20*time.Second, 10*time.Second)
	var headerTimer *time.Timer
	if transport.ResponseHeaderTimeout > 0 {
		headerTimer = time.AfterFunc(transport.ResponseHeaderTimeout, func() { _ = client.Close() })
		defer headerTimer.Stop()
	}
	handedOff = true
	response, err := client.RoundTrip(request)
	if err != nil {
		_ = body.Close()
		return nil, err
	}
	response.Header.Del("Location")
	body.ReadCloser = response.Body
	response.Body = body
	return response, nil
}

type modelStreamBody struct {
	io.ReadCloser
	client      *http2.ClientConn
	stopRequest func() bool
	done        chan struct{}
	once        sync.Once
}

func (b *modelStreamBody) Close() error {
	b.once.Do(func() {
		close(b.done)
		b.stopRequest()
		_ = b.client.Close()
	})
	if b.ReadCloser != nil {
		return b.ReadCloser.Close()
	}
	return nil
}

func (b *modelStreamBody) keepAlive(ctx context.Context, interval, timeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-ctx.Done():
			_ = b.client.Close()
			return
		case <-ticker.C:
		}
		ping, cancel := context.WithTimeout(ctx, timeout)
		err := b.client.Ping(ping)
		cancel()
		if err != nil {
			_ = b.client.Close()
			return
		}
	}
}
