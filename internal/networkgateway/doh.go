package networkgateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

// DoH is gateway maintenance, not an agent-accessible proxy. Its peer is pinned
// by trusted bootstrap configuration and the namespace controller; neither an
// HTTP redirect, an environment proxy nor another DNS lookup can change it.
type DoH struct {
	client    *http.Client
	transport *http.Transport
	url       string
	sockets   maintenanceSockets
	stop      context.CancelFunc
}

func NewDoH(peer netip.AddrPort, serverName string, roots *x509.CertPool) (*DoH, error) {
	name, err := egress.NormalizeDomain(serverName, false)
	if err != nil || !peer.IsValid() || !peer.Addr().Is4() || peer.Port() == 0 {
		return nil, Failure("dns_upstream_invalid")
	}
	doh := &DoH{}
	lifetime, stop := context.WithCancel(context.Background())
	doh.stop = stop
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if !doh.sockets.beginDial() {
				return nil, net.ErrClosed
			}
			defer doh.sockets.endDial()
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			end := context.AfterFunc(lifetime, cancel)
			defer end()
			conn, err := (&net.Dialer{Timeout: DNSQueryTimeout}).DialContext(ctx, "tcp4", peer.String())
			if err != nil {
				return nil, err
			}
			return doh.sockets.track(conn), nil
		},
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12, ServerName: name, RootCAs: roots},
		ForceAttemptHTTP2: true, DisableCompression: true,
		MaxConnsPerHost: MaxDNSInFlight, MaxIdleConns: 2, MaxIdleConnsPerHost: 2,
		IdleConnTimeout: time.Minute, TLSHandshakeTimeout: DNSQueryTimeout,
		ResponseHeaderTimeout: DNSQueryTimeout, MaxResponseHeaderBytes: MaxDNSMessage,
	}
	doh.transport, doh.url = transport, "https://"+name+"/dns-query"
	doh.client = &http.Client{
		Transport: transport, Timeout: DNSQueryTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return doh, nil
}

func (d *DoH) Close() { d.transport.CloseIdleConnections() }

// Shutdown is terminal and joins detached transport dials and raw socket I/O.
// CloseIdleConnections alone is neither an active-I/O nor a byte-meter barrier.
func (d *DoH) Shutdown(ctx context.Context) error {
	d.stop()
	d.transport.CloseIdleConnections()
	return d.sockets.shutdown(ctx)
}

func (d *DoH) Exchange(ctx context.Context, wire []byte) ([]byte, error) {
	if len(wire) < 12 || len(wire) > MaxDNSMessage {
		return nil, Failure("dns_query_invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(wire))
	if err != nil {
		return nil, Failure("dns_unavailable")
	}
	request.Header.Set("Content-Type", "application/dns-message")
	request.Header.Set("Accept", "application/dns-message")
	request.Header.Set("Accept-Encoding", "identity")
	response, err := d.client.Do(request)
	if err != nil {
		return nil, Failure("dns_unavailable")
	}
	defer response.Body.Close()
	mediaType, _, typeErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || typeErr != nil || mediaType != "application/dns-message" || response.Header.Get("Content-Encoding") != "" || response.ContentLength > MaxDNSMessage {
		return nil, Failure("dns_answer_invalid")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxDNSMessage+1))
	if err != nil || len(data) < 12 || len(data) > MaxDNSMessage {
		return nil, Failure("dns_answer_invalid")
	}
	return data, nil
}
