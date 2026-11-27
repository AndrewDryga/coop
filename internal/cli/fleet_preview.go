package cli

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/randid"
	"github.com/AndrewDryga/coop/internal/workerconnector"
)

func previewGateway(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		host, port = "127.0.0.1", address
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Is4() || !(ip.IsPrivate() || ip.IsLoopback() || netip.MustParsePrefix("100.64.0.0/10").Contains(ip)) {
		return "", errors.New("preview requires a private controller address")
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != port {
		return "", errors.New("invalid controller port")
	}
	return net.JoinHostPort(ip.String(), port), nil
}

func runFleetPreview(address string, input io.Reader, output io.Writer) error {
	gateway, err := previewGateway(address)
	if err != nil {
		return err
	}
	var grant struct {
		Token   string `json:"token"`
		Expires int64  `json:"expires"`
		Server  string `json:"server_name"`
		CA      string `json:"ca_pem"`
	}
	data, err := io.ReadAll(io.LimitReader(input, (32<<10)+1))
	if err != nil || len(data) > 32<<10 || json.Unmarshal(data, &grant) != nil || len(grant.Token) < 32 || len(grant.Token) > 4096 || strings.ContainsAny(grant.Token, "\r\n") || grant.Server == "" {
		return errors.New("invalid controller preview grant")
	}
	roots := x509.NewCertPool()
	expires := time.Unix(grant.Expires, 0)
	if !roots.AppendCertsFromPEM([]byte(grant.CA)) || !expires.After(time.Now()) || expires.After(time.Now().Add(10*time.Minute+30*time.Second)) {
		return errors.New("preview grant expired or invalid")
	}
	tlsConfig := &tls.Config{RootCAs: roots, ServerName: grant.Server, MinVersion: tls.VersionTLS13}
	cancellation, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithDeadline(cancellation, expires)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	host := net.JoinHostPort(randid.Hex(16)+".localhost", fmt.Sprint(listener.Addr().(*net.TCPAddr).Port))
	transport := &http.Transport{MaxConnsPerHost: 8, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: time.Minute,
		DialContext: func(call context.Context, _, _ string) (net.Conn, error) {
			return workerconnector.DialServiceSocket(call, "wss://"+gateway+"/v1/coop-operators/service-relays", tlsConfig, grant.Token)
		}}
	defer transport.CloseIdleConnections()
	cookiePrefix := "coop_" + randid.Hex(12) + "_"
	proxy := &httputil.ReverseProxy{Transport: transport, Rewrite: func(r *httputil.ProxyRequest) {
		r.Out.URL.Scheme = "http"
		r.Out.URL.Host = "localhost"
		r.Out.Host = r.In.Host
		for _, key := range []string{"Cookie", "Proxy-Authorization", "Forwarded", "Referer", "X-Coop-Model-Grant", "X-Coop-Service-Grant"} {
			r.Out.Header.Del(key)
		}
		for _, cookie := range r.In.Cookies() {
			if name, ok := strings.CutPrefix(cookie.Name, cookiePrefix); ok && name != "" {
				cookie.Name = name
				r.Out.AddCookie(cookie)
			}
		}
	}, ModifyResponse: func(resp *http.Response) error {
		cookies := resp.Cookies()
		resp.Header.Del("Set-Cookie")
		for _, cookie := range cookies {
			cookie.Name = cookiePrefix + cookie.Name
			cookie.Domain = ""
			resp.Header.Add("Set-Cookie", cookie.String())
		}
		resp.Header.Del("Clear-Site-Data")
		resp.Header.Set("Referrer-Policy", "no-referrer")
		resp.Header.Set("Cache-Control", "no-store")
		resp.Header.Add("Content-Security-Policy", "frame-ancestors 'none'")
		return nil
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "Job service is unavailable", http.StatusBadGateway)
	}}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 64 << 10, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !previewRequestAllowed(r, host) {
			http.Error(w, "Preview access refused", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(previewResponseWriter{w}, r)
	})}
	stopServer := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stopServer()
	fmt.Fprintf(output, "http://%s/\n", host)
	if err = server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func previewRequestAllowed(r *http.Request, host string) bool {
	if r.Host != host || r.URL.IsAbs() || r.Method == http.MethodConnect || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Values("Origin")
	return len(origin) == 0 || (len(origin) == 1 && origin[0] == "http://"+host)
}

// ReverseProxy reads upgraded client traffic from the hijacked connection. ServeHTTP
// may already have read the first protocol bytes alongside the Upgrade headers.
type previewResponseWriter struct{ http.ResponseWriter }

func (w previewResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w previewResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil || rw.Reader.Buffered() == 0 {
		return conn, rw, err
	}
	return &previewBufferedConn{Conn: conn, buffered: rw.Reader}, rw, nil
}

type previewBufferedConn struct {
	net.Conn
	buffered *bufio.Reader
}

func (c *previewBufferedConn) Read(p []byte) (int, error) { return c.buffered.Read(p) }

func (c *previewBufferedConn) CloseWrite() error {
	if tcp, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return tcp.CloseWrite()
	}
	return c.Conn.Close()
}
