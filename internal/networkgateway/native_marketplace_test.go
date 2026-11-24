package networkgateway

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

const testMarketplaceSHA = "65adb96f26deb662f6a993f74347d0ccd1a61a52"

func writeConnectRequest(w io.Writer, addr string) error {
	crlf := string([]byte{13, 10})
	_, err := io.WriteString(w, "CONNECT "+addr+" HTTP/1.1"+crlf+"Host: "+addr+crlf+crlf)
	return err
}

type marketplaceOriginFixture struct {
	broker    *NativeBroker
	proxy     *httptest.Server
	upstream  *httptest.Server
	client    *http.Client
	events    *GuardEvents
	forwarded atomic.Int64
	ordinary  atomic.Int64
}

func newMarketplaceOriginFixture(t *testing.T, selection string, handler http.Handler, configure ...func(*NativeBrokerOrigin)) *marketplaceOriginFixture {
	t.Helper()
	f := &marketplaceOriginFixture{events: NewGuardEvents(nil)}
	caCert, caKey, caPEM := newMarketplaceTestCA(t)
	leafCert, _, _ := newMarketplaceLeafCert(t, caCert, caKey, "downloads.claude.ai")

	f.upstream = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.forwarded.Add(1)
		handler.ServeHTTP(w, r)
	}))
	f.upstream.TLS = &tls.Config{Certificates: []tls.Certificate{leafCert}, MinVersion: tls.VersionTLS12}
	f.upstream.EnableHTTP2 = true
	f.upstream.StartTLS()
	t.Cleanup(f.upstream.Close)

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("append CA PEM")
	}

	origin := NativeBrokerOrigin{
		Host:        "downloads.claude.ai",
		Certificate: leafCert,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "downloads.claude.ai:443" {
				return nil, fmt.Errorf("unexpected dial target %s", address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, f.upstream.Listener.Addr().String())
		},
	}
	nativeAdapterRoutes(t, "claude", selection, "downloads.claude.ai", "")(&origin)
	for _, apply := range configure {
		apply(&origin)
	}

	binding := NativeBrokerBinding{RunID: "run-claude", Provider: "claude", Account: "selected", Epoch: 1}
	snapshot := NativeAccessSnapshot{Binding: binding, Revision: 1, Expires: time.Now().Add(time.Hour), Credential: "private-claude-secret"}
	var err error
	f.broker, err = NewNativeBroker(context.Background(), binding, []NativeBrokerOrigin{origin}, func(context.Context) (NativeAccessSnapshot, error) {
		return snapshot, nil
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.ordinary.Add(1)
		f.events.emit(GuardEvent{Kind: "tls_denied", Name: strings.TrimSuffix(r.Host, ":443"), Port: 443, Reason: "unapproved_name"})
		http.Error(w, "ordinary policy denied", http.StatusForbidden)
	}))
	if err != nil {
		t.Fatal(err)
	}
	f.broker.events = f.events
	f.broker.origins["downloads.claude.ai"].transport.TLSClientConfig.RootCAs = roots
	t.Cleanup(func() { _ = f.broker.Close() })

	f.proxy = httptest.NewServer(f.broker)
	t.Cleanup(f.proxy.Close)

	transport := &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig:   &tls.Config{RootCAs: roots, ServerName: "downloads.claude.ai", MinVersion: tls.VersionTLS12},
		DialTLSContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(f.proxy.URL, "http://"))
			if err != nil {
				return nil, err
			}
			_ = raw.SetDeadline(time.Now().Add(wait.Deadline))
			if err := writeConnectRequest(raw, addr); err != nil {
				_ = raw.Close()
				return nil, err
			}
			reader := bufio.NewReader(raw)
			resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
			if err != nil {
				_ = raw.Close()
				return nil, err
			}
			if resp.StatusCode != http.StatusOK {
				_ = raw.Close()
				return nil, fmt.Errorf("CONNECT %s status %d", addr, resp.StatusCode)
			}
			pending := make([]byte, reader.Buffered())
			if _, err := io.ReadFull(reader, pending); err != nil {
				_ = raw.Close()
				return nil, err
			}
			conn := tls.Client(&prefixedConn{Conn: raw, prefix: pending}, &tls.Config{
				RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}})
			if err := conn.HandshakeContext(ctx); err != nil {
				_ = raw.Close()
				return nil, err
			}
			_ = raw.SetDeadline(time.Time{})
			return conn, nil
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	f.client = &http.Client{
		Transport:     transport,
		Timeout:       wait.Deadline,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return f
}

func TestNativeOriginsFitFourProvidersWithClaudeMarketplace(t *testing.T) {
	config := testLaunch(t)
	config.NativeOrigins = []string{
		"api.anthropic.com", "mcp-proxy.anthropic.com", "downloads.claude.ai",
		"api.openai.com", "chatgpt.com", "github.com",
		"cloudcode-pa.googleapis.com", "www.googleapis.com",
		"cli-chat-proxy.grok.com", "code.grok.com",
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("four-provider origin configuration: %v", err)
	}
	config.NativeOrigins = append(config.NativeOrigins, "extra.example")
	if err := config.Validate(); err == nil {
		t.Fatal("origin configuration exceeded its bound")
	}
}

func TestNativeBrokerMarketplaceRejectsHTTP2RequestBody(t *testing.T) {
	fixture := newMarketplaceOriginFixture(t, "claude-oauth", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, testMarketplaceSHA)
	}))
	request, err := http.NewRequest(http.MethodGet, "https://downloads.claude.ai/claude-code-releases/plugins/claude-plugins-official/latest",
		io.NopCloser(strings.NewReader("unexpected request body")))
	if err != nil {
		t.Fatal(err)
	}
	response, err := fixture.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 || response.StatusCode != http.StatusForbidden || fixture.forwarded.Load() != 0 {
		t.Fatalf("HTTP/%d status=%d forwarded=%d, want HTTP/2 refusal without forwarding", response.ProtoMajor, response.StatusCode, fixture.forwarded.Load())
	}
}

func TestNativeBrokerClaudeMarketplacePointerGatesArchiveAndBoundsReads(t *testing.T) {
	const otherSHA = "0123456789abcdef0123456789abcdef01234567"
	const basePath = "/claude-code-releases/plugins/claude-plugins-official/"
	lf := string([]byte{10})

	var mu sync.Mutex
	latestStatus := http.StatusOK
	latestBody := testMarketplaceSHA + lf
	latestRedirect := ""
	archiveRedirect := ""
	archiveBody := []byte("PK-zip-bytes")
	var flushArchive bool
	archiveFirstChunkRead := make(chan struct{})
	var seenHeaders http.Header

	f := newMarketplaceOriginFixture(t, "claude-oauth", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenHeaders = r.Header.Clone()
		lStatus, lBody, lRedir := latestStatus, latestBody, latestRedirect
		aRedir, aBody, flush := archiveRedirect, archiveBody, flushArchive
		mu.Unlock()

		switch r.URL.Path {
		case basePath + "latest":
			if lRedir != "" {
				w.Header().Set("Location", lRedir)
				w.WriteHeader(http.StatusFound)
				return
			}
			w.WriteHeader(lStatus)
			_, _ = io.WriteString(w, lBody)
		case basePath + testMarketplaceSHA + ".zip":
			if aRedir != "" {
				w.Header().Set("Location", aRedir)
				w.WriteHeader(http.StatusTemporaryRedirect)
				return
			}
			if flush {
				w.(http.Flusher).Flush()
				_, _ = w.Write(aBody[:512])
				w.(http.Flusher).Flush()
				select {
				case <-archiveFirstChunkRead:
				case <-r.Context().Done():
					return
				}
				aBody = aBody[512:]
			}
			_, _ = w.Write(aBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}), func(origin *NativeBrokerOrigin) {
		// Tighten archive cap to 1 KiB in this unit test so oversize streaming checks stay tiny.
		for i := range origin.Requests {
			if origin.Requests[i].UsesPointer != "" {
				origin.Requests[i].MaxResponseBytes = 1024
			}
		}
	})

	get := func(path string, hdrs map[string]string) (*http.Response, []byte, error) {
		req, err := http.NewRequest(http.MethodGet, "https://downloads.claude.ai"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
		resp, err := f.client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		body, readErr := io.ReadAll(resp.Body)
		return resp, body, readErr
	}

	// 1. Archive request BEFORE latest pointer validation is denied as unapproved_name without reaching upstream.
	resp, _, err := get(basePath+testMarketplaceSHA+".zip", nil)
	if err != nil || resp.StatusCode != http.StatusForbidden || f.forwarded.Load() != 0 {
		t.Fatalf("pre-pointer archive got status=%v err=%v forwarded=%d", resp, err, f.forwarded.Load())
	}
	events, _ := f.events.Drain(MaxGuardEvents)
	if len(events) != 1 || events[0].Kind != "tls_denied" || events[0].Name != "downloads.claude.ai" || events[0].Port != 443 || events[0].Reason != "unapproved_name" {
		t.Fatalf("pre-pointer archive events = %+v", events)
	}

	// 2. Malformed, uppercase, empty, or oversize latest pointers fail closed and never unlock the archive.
	for _, bad := range []string{"", "not-a-sha", strings.ToUpper(testMarketplaceSHA), "../../etc/passwd", strings.Repeat("a", 257)} {
		mu.Lock()
		latestStatus, latestBody, latestRedirect = http.StatusOK, bad, ""
		mu.Unlock()
		resp, _, err := get(basePath+"latest", nil)
		if err != nil || resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("bad pointer %q got status=%v err=%v", bad, resp, err)
		}
		_, _ = f.events.Drain(MaxGuardEvents)
		before := f.forwarded.Load()
		resp, _, err = get(basePath+testMarketplaceSHA+".zip", nil)
		if err != nil || resp.StatusCode != http.StatusForbidden || f.forwarded.Load() != before {
			t.Fatalf("archive unlocked after bad pointer %q: status=%v err=%v", bad, resp, err)
		}
		_, _ = f.events.Drain(MaxGuardEvents)
	}

	// 3. Redirect on latest is refused without leaking Location or unlocking the archive.
	mu.Lock()
	latestStatus, latestBody, latestRedirect = http.StatusFound, "", "https://evil.example/latest"
	mu.Unlock()
	resp, _, err = get(basePath+"latest", nil)
	if err != nil || resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Location") != "" {
		t.Fatalf("latest redirect got status=%v location=%q err=%v", resp, resp.Header.Get("Location"), err)
	}
	events, _ = f.events.Drain(MaxGuardEvents)
	if len(events) != 1 || events[0].Reason != "credential_broker_redirect_refused" {
		t.Fatalf("latest redirect events = %+v", events)
	}

	// 4. Upstream HTTP failure (503) surfaces truthfully to the client without unlocking the archive.
	mu.Lock()
	latestStatus, latestBody, latestRedirect = http.StatusServiceUnavailable, "upstream unavailable", ""
	mu.Unlock()
	resp, body, err := get(basePath+"latest", nil)
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable || string(body) != "upstream unavailable" {
		t.Fatalf("upstream 503 got status=%v body=%q err=%v", resp, body, err)
	}
	if drained, _ := f.events.Drain(MaxGuardEvents); len(drained) != 0 {
		t.Fatalf("upstream HTTP 503 recorded spurious guard denial: %+v", drained)
	}

	// 5. Valid latest pointer succeeds credential-free, strips custom headers, and unlocks ONLY that exact SHA archive.
	mu.Lock()
	latestStatus, latestBody, latestRedirect = http.StatusOK, testMarketplaceSHA+lf, ""
	mu.Unlock()
	resp, body, err = get(basePath+"latest", map[string]string{"User-Agent": "guest-fixture", "Accept": "application/guest-fixture", "X-Custom-Fixture": "test"})
	if err != nil || resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != testMarketplaceSHA {
		t.Fatalf("valid latest got status=%v body=%q err=%v", resp, body, err)
	}
	mu.Lock()
	hdrs := seenHeaders.Clone()
	mu.Unlock()
	if hdrs.Get("Authorization") != "" || hdrs.Get("X-Api-Key") != "" || hdrs.Get("X-Custom-Fixture") != "" || hdrs.Get("User-Agent") == "guest-fixture" ||
		hdrs.Get("Accept") != "" || hdrs.Get("Accept-Encoding") != "identity" {
		t.Fatalf("unexpected upstream headers on latest: %v", hdrs)
	}

	// Unmatched SHA archive remains refused without reaching upstream.
	before := f.forwarded.Load()
	resp, _, err = get(basePath+otherSHA+".zip", nil)
	if err != nil || resp.StatusCode != http.StatusForbidden || f.forwarded.Load() != before {
		t.Fatalf("unmatched SHA archive got status=%v err=%v forwarded=%d (was %d)", resp, err, f.forwarded.Load(), before)
	}
	events, _ = f.events.Drain(MaxGuardEvents)
	if len(events) != 1 || events[0].Kind != "tls_denied" || events[0].Reason != "unapproved_name" {
		t.Fatalf("unmatched SHA events = %+v", events)
	}

	// Matched SHA archive carrying any credential header is refused with credential_broker_credential_refused.
	for _, header := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Cookie"} {
		before = f.forwarded.Load()
		resp, _, err = get(basePath+testMarketplaceSHA+".zip", map[string]string{header: "secret"})
		if err != nil || resp.StatusCode != http.StatusForbidden || f.forwarded.Load() != before {
			t.Fatalf("credentialed download (%s) got status=%v err=%v", header, resp, err)
		}
		events, _ = f.events.Drain(MaxGuardEvents)
		if len(events) != 1 || events[0].Reason != "credential_broker_credential_refused" {
			t.Fatalf("credentialed download (%s) events = %+v", header, events)
		}
	}

	// Matched SHA archive within bound succeeds.
	resp, body, err = get(basePath+testMarketplaceSHA+".zip", nil)
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != "PK-zip-bytes" {
		t.Fatalf("valid archive got status=%v body=%q err=%v", resp, body, err)
	}

	// Redirect on archive is refused.
	mu.Lock()
	archiveRedirect = "https://evil.example/archive.zip"
	mu.Unlock()
	resp, _, err = get(basePath+testMarketplaceSHA+".zip", nil)
	if err != nil || resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Location") != "" {
		t.Fatalf("archive redirect got status=%v err=%v", resp, err)
	}
	events, _ = f.events.Drain(MaxGuardEvents)
	if len(events) != 1 || events[0].Reason != "credential_broker_redirect_refused" {
		t.Fatalf("archive redirect events = %+v", events)
	}

	// Oversize archive response (> 1024 bytes) fails closed.
	mu.Lock()
	archiveRedirect = ""
	archiveBody = bytes.Repeat([]byte("z"), 2048)
	mu.Unlock()
	resp, _, err = get(basePath+testMarketplaceSHA+".zip", nil)
	if err != nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("oversize archive with a declared length got status=%v err=%v", resp, err)
	}
	_, _ = f.events.Drain(MaxGuardEvents)

	mu.Lock()
	flushArchive = true
	mu.Unlock()
	resp, err = f.client.Get("https://downloads.claude.ai" + basePath + testMarketplaceSHA + ".zip")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first, err := io.CopyN(io.Discard, resp.Body, 512)
	close(archiveFirstChunkRead)
	if err != nil || resp.StatusCode != http.StatusOK || resp.ContentLength != -1 {
		t.Fatalf("streamed archive's first chunk got status=%d length=%d bytes=%d err=%v",
			resp.StatusCode, resp.ContentLength, first, err)
	}
	rest, err := io.Copy(io.Discard, resp.Body)
	if err == nil || first+rest > 1024 {
		t.Fatalf("oversize streamed archive got bytes=%d err=%v", first+rest, err)
	}
}

func TestNativeBrokerPublicDownloadOriginPreservesOrdinaryPolicyDecisions(t *testing.T) {
	var allowed atomic.Bool
	f := newMarketplaceOriginFixture(t, "apikey", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, testMarketplaceSHA)
	}), func(origin *NativeBrokerOrigin) {
		origin.OrdinaryDecision = func(port int) egress.Decision {
			if allowed.Load() && port == 443 {
				return egress.Decision{Allowed: true, Reason: "rule_match"}
			}
			return egress.Decision{Allowed: false, Reason: "unapproved_name"}
		}
	})

	// Unrelated paths on downloads.claude.ai when unapproved emit tls_denied/unapproved_name.
	for _, path := range []string{
		"/claude-code-releases/latest",
		"/model-catalog/v1/catalog.json",
		"/claude-code-releases/plugins/claude-plugins-official/latest?extra=1",
	} {
		req, err := http.NewRequest(http.MethodGet, "https://downloads.claude.ai"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := f.client.Do(req)
		if err != nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("unapproved path %s got resp=%v err=%v", path, resp, err)
		}
		_ = resp.Body.Close()
		events, _ := f.events.Drain(MaxGuardEvents)
		if len(events) != 1 || events[0].Kind != "tls_denied" || events[0].Name != "downloads.claude.ai" || events[0].Port != 443 || events[0].Reason != "unapproved_name" {
			t.Fatalf("unapproved path %s events = %+v", path, events)
		}
	}

	// github.com is not a broker origin and goes straight to the ordinary fallback proxy.
	raw, err := net.DialTimeout("tcp", strings.TrimPrefix(f.proxy.URL, "http://"), wait.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	_ = writeConnectRequest(raw, "github.com:443")
	resp, err := http.ReadResponse(bufio.NewReader(raw), &http.Request{Method: http.MethodConnect})
	_ = raw.Close()
	if err != nil || resp.StatusCode != http.StatusForbidden || f.ordinary.Load() != 1 {
		t.Fatalf("github.com CONNECT got resp=%v err=%v ordinary=%d", resp, err, f.ordinary.Load())
	}
	events, _ := f.events.Drain(MaxGuardEvents)
	if len(events) != 1 || events[0].Kind != "tls_denied" || events[0].Name != "github.com" || events[0].Reason != "unapproved_name" {
		t.Fatalf("github.com events = %+v", events)
	}

	// Once ordinary project policy approves downloads.claude.ai:443, CONNECT delegates straight to ordinary fallback.
	allowed.Store(true)
	raw, err = net.DialTimeout("tcp", strings.TrimPrefix(f.proxy.URL, "http://"), wait.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	_ = writeConnectRequest(raw, "downloads.claude.ai:443")
	_, _ = http.ReadResponse(bufio.NewReader(raw), &http.Request{Method: http.MethodConnect})
	_ = raw.Close()
	if f.ordinary.Load() != 2 {
		t.Fatalf("approved downloads.claude.ai did not delegate to ordinary fallback: ordinary=%d", f.ordinary.Load())
	}
}

func TestNativeBrokerConfigRejectsInvalidPublicDownloadDeclarations(t *testing.T) {
	caCert, caKey, _ := newMarketplaceTestCA(t)
	leafCert, _, _ := newMarketplaceLeafCert(t, caCert, caKey, "downloads.claude.ai")
	binding := NativeBrokerBinding{RunID: "run-claude", Provider: "claude", Account: "selected", Epoch: 1}
	snapshot := NativeAccessSnapshot{Binding: binding, Revision: 1, Expires: time.Now().Add(time.Hour), Credential: "secret"}
	load := func(context.Context) (NativeAccessSnapshot, error) { return snapshot, nil }

	base := func() NativeBrokerOrigin {
		origin := NativeBrokerOrigin{
			Host:        "downloads.claude.ai",
			Certificate: leafCert,
			DialContext: func(context.Context, string, string) (net.Conn, error) { return nil, io.EOF },
		}
		nativeAdapterRoutes(t, "claude", "claude-oauth", "downloads.claude.ai", "")(&origin)
		return origin
	}

	for name, mutate := range map[string]func(*NativeBrokerOrigin){
		"dangling uses pointer": func(o *NativeBrokerOrigin) { o.Requests[1].UsesPointer = "missing" },
		"reject credentials with header": func(o *NativeBrokerOrigin) {
			o.Requests[0].CredentialFree = false
			o.Requests[0].Header = "Authorization"
		},
		"mixed public and credentialed origin": func(o *NativeBrokerOrigin) {
			o.Requests = append(o.Requests, NativeBrokerRequestLine{
				Method: http.MethodGet, Path: "/v1/messages", Header: "Authorization", HeaderPrefix: "Bearer ",
			})
		},
		"unbounded pointer": func(o *NativeBrokerOrigin) { o.Requests[0].MaxResponseBytes = 0 },
		"oversize pointer":  func(o *NativeBrokerOrigin) { o.Requests[0].MaxResponseBytes = 8192 },
		"non-sha1 archive":  func(o *NativeBrokerOrigin) { o.Requests[1].Segment = "token" },
	} {
		origin := base()
		mutate(&origin)
		if broker, err := NewNativeBroker(context.Background(), binding, []NativeBrokerOrigin{origin}, load, http.NotFoundHandler()); err == nil {
			_ = broker.Close()
			t.Fatalf("%s was accepted", name)
		}
	}
}

// Run with COOP_PINNED_CLAUDE_MARKETPLACE_TEST=1 in the pinned Linux client image, with only loopback networking.
func TestPinnedClaudeColdInteractiveStartupRegistersOfficialMarketplace(t *testing.T) {
	if os.Getenv("COOP_PINNED_CLAUDE_MARKETPLACE_TEST") != "1" {
		t.Skip("requires the pinned Claude client image and Python PTY")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	ag, _ := agents.Get("claude")
	clients := ag.LockedClients(agents.ClientPlatform{OS: runtime.GOOS, Architecture: runtime.GOARCH, Libc: "glibc"})
	i := slices.IndexFunc(clients, func(client agents.LockedClient) bool { return client.Client == egress.ClientCLI })
	if i < 0 {
		t.Fatal("the pinned Claude CLI is unavailable for this platform")
	}
	cli := clients[i]
	ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
	defer cancel()
	out, err := exec.CommandContext(ctx, cli.Launcher(), "--version").CombinedOutput()
	version, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if err != nil || version != cli.Version {
		t.Fatalf("Claude CLI version = %q (%v), want %s", out, err, cli.Version)
	}
	for _, selection := range []string{"claude-oauth", "apikey"} {
		t.Run(selection, func(t *testing.T) {
			checkPinnedClaudeMarketplace(t, ag, cli.Launcher(), python, selection)
		})
	}
}

func checkPinnedClaudeMarketplace(t *testing.T, ag agents.Agent, cli, python, selection string) {
	t.Helper()
	seed, err := ag.NativeCredentials().Broker.Seed(selection)
	if err != nil {
		t.Fatal(err)
	}
	caCert, caKey, caPEM := newMarketplaceTestCA(t)
	apiCert, _, _ := newMarketplaceLeafCert(t, caCert, caKey, "api.anthropic.com")
	cdnCert, _, _ := newMarketplaceLeafCert(t, caCert, caKey, "downloads.claude.ai")
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("append CA PEM")
	}

	zipBytes := buildTestMarketplaceZip(t)
	var mu sync.Mutex
	var cdnPaths []string

	apiUpstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/oauth/profile":
			_, _ = io.WriteString(w, `{"account":{"uuid":"00000000-0000-4000-8000-000000000001","email":"broker@coop.local"},"organization":{"uuid":"00000000-0000-4000-8000-000000000002","organization_type":"claude_max"}}`)
		case "/v1/mcp_servers":
			_, _ = io.WriteString(w, `{"data":[]}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	apiUpstream.TLS = &tls.Config{Certificates: []tls.Certificate{apiCert}, MinVersion: tls.VersionTLS12}
	apiUpstream.StartTLS()
	t.Cleanup(apiUpstream.Close)

	cdnUpstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hasNativeDownloadCredential(r.Header) {
			t.Errorf("downloads.claude.ai received credential header: %v", r.Header)
		}
		mu.Lock()
		cdnPaths = append(cdnPaths, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		switch r.URL.Path {
		case "/claude-code-releases/plugins/claude-plugins-official/latest":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, testMarketplaceSHA)
		case "/claude-code-releases/plugins/claude-plugins-official/" + testMarketplaceSHA + ".zip":
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(zipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	cdnUpstream.TLS = &tls.Config{Certificates: []tls.Certificate{cdnCert}, MinVersion: tls.VersionTLS12}
	cdnUpstream.StartTLS()
	t.Cleanup(cdnUpstream.Close)

	dialTo := func(srv *httptest.Server, want string) func(context.Context, string, string) (net.Conn, error) {
		return func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != want {
				return nil, fmt.Errorf("unexpected dial %s", address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		}
	}

	makeOrigin := func(host string, cert tls.Certificate, dial func(context.Context, string, string) (net.Conn, error)) NativeBrokerOrigin {
		o := NativeBrokerOrigin{
			Host:        host,
			Certificate: cert,
			DialContext: dial,
			OrdinaryDecision: func(int) egress.Decision {
				return egress.Decision{Allowed: false, Reason: "unapproved_name"}
			},
		}
		nativeAdapterRoutes(t, "claude", selection, host, "")(&o)
		for i := range o.Requests {
			if !o.Requests[i].CredentialFree {
				o.Requests[i].ClientMarker = seed.Marker
			}
		}
		return o
	}

	events := NewGuardEvents(nil)
	binding := NativeBrokerBinding{RunID: "run-live-qualify", Provider: "claude", Account: "default", Epoch: 1}
	snapshot := NativeAccessSnapshot{Binding: binding, Revision: 1, Expires: time.Now().Add(time.Hour), Credential: "private-oauth-token"}
	origins := []NativeBrokerOrigin{
		makeOrigin("api.anthropic.com", apiCert, dialTo(apiUpstream, "api.anthropic.com:443")),
		makeOrigin("downloads.claude.ai", cdnCert, dialTo(cdnUpstream, "downloads.claude.ai:443")),
	}
	if selection == "claude-oauth" {
		mcpCert, _, _ := newMarketplaceLeafCert(t, caCert, caKey, "mcp-proxy.anthropic.com")
		origins = append(origins, makeOrigin("mcp-proxy.anthropic.com", mcpCert,
			func(context.Context, string, string) (net.Conn, error) { return nil, io.EOF }))
	}
	broker, err := NewNativeBroker(context.Background(), binding, origins, func(context.Context) (NativeAccessSnapshot, error) {
		return snapshot, nil
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		if host == "" {
			host = r.Host
		}
		events.emit(GuardEvent{Kind: "tls_denied", Name: host, Port: 443, Reason: "unapproved_name"})
		http.Error(w, "blocked by ordinary policy", http.StatusForbidden)
	}))
	if err != nil {
		t.Fatal(err)
	}
	broker.events = events
	for _, origin := range broker.origins {
		origin.transport.TLSClientConfig.RootCAs = roots
	}
	t.Cleanup(func() { _ = broker.Close() })

	proxy := httptest.NewServer(broker)
	t.Cleanup(proxy.Close)

	tmp := t.TempDir()
	caFile := filepath.Join(tmp, "coop-native-ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	emptyGitConfig := filepath.Join(tmp, "gitconfig")
	if err := os.WriteFile(emptyGitConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(tmp, "home")
	cfgDir := filepath.Join(home, ".claude")
	ws := filepath.Join(tmp, "ws")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}

	for name, content := range seed.Files {
		if err := os.WriteFile(filepath.Join(cfgDir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seeded := map[string]any{}
	if file := seed.Files[".claude.json"]; len(file) != 0 {
		if err := json.Unmarshal(file, &seeded); err != nil {
			t.Fatal(err)
		}
	}
	if selection == "apikey" {
		seeded["customApiKeyResponses"] = map[string]any{"approved": []string{seed.Marker[len(seed.Marker)-20:]}}
	}
	seeded["hasCompletedOnboarding"] = true
	seeded["bypassPermissionsModeAccepted"] = true
	seeded["projects"] = map[string]any{ws: map[string]any{"hasTrustDialogAccepted": true}}
	mergedClaudeJSON, err := json.Marshal(seeded)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, ".claude.json"), mergedClaudeJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{"theme":"dark","skipDangerousModePermissionPrompt":true,"sandbox":{"enabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cmdEnv := []string{
		"PATH=/opt/coop/bin:/usr/local/bin:/usr/bin:/bin",
		"HOME=" + home,
		"TERM=xterm-256color",
		"GIT_CONFIG_GLOBAL=" + emptyGitConfig,
		"GIT_CONFIG_SYSTEM=" + emptyGitConfig,
		"HTTPS_PROXY=" + proxy.URL,
		"HTTP_PROXY=" + proxy.URL,
		"NO_PROXY=127.0.0.1,localhost",
		"NODE_EXTRA_CA_CERTS=" + caFile,
		"SSL_CERT_FILE=" + caFile,
		"GIT_SSL_CAINFO=" + caFile,
	}
	cmdEnv = append(cmdEnv, ag.BoxEnv(home)...)
	for k, v := range seed.Env {
		cmdEnv = append(cmdEnv, k+"="+v)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c",
		`import os, pty, select, subprocess, sys, time, json
cli, cfg = sys.argv[1:3]
master, slave = pty.openpty()
p = subprocess.Popen([cli, "--dangerously-skip-permissions"], stdin=slave, stdout=slave, stderr=slave, close_fds=True)
os.close(slave)
deadline = time.monotonic() + 30.0
while time.monotonic() < deadline and p.poll() is None:
    r, _, _ = select.select([master], [], [], 0.2)
    if r:
        try:
            os.read(master, 4096)
        except OSError:
            break
    try:
        with open(os.path.join(cfg, ".claude.json")) as f:
            st = json.load(f)
        if st.get("officialMarketplaceAutoInstalled") or st.get("officialMarketplaceAutoInstallFailReason"):
            break
    except Exception:
        pass
p.terminate()
try:
    p.wait(timeout=3)
except subprocess.TimeoutExpired:
    p.kill()
    p.wait()
os.close(master)
`, cli, cfgDir)
	cmd.Dir = ws
	cmd.Env = cmdEnv
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cold interactive claude failed: %v: %s", err, out)
	}

	stateRaw, err := os.ReadFile(filepath.Join(cfgDir, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(stateRaw, &state); err != nil {
		t.Fatal(err)
	}
	if state["officialMarketplaceAutoInstalled"] != true {
		t.Fatalf("officialMarketplaceAutoInstalled = %v (failReason=%v)", state["officialMarketplaceAutoInstalled"], state["officialMarketplaceAutoInstallFailReason"])
	}
	shaBytes, err := os.ReadFile(filepath.Join(cfgDir, "plugins", "marketplaces", "claude-plugins-official", ".gcs-sha"))
	if err != nil || strings.TrimSpace(string(shaBytes)) != testMarketplaceSHA {
		t.Fatalf(".gcs-sha = %q, err = %v", shaBytes, err)
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "plugins", "known_marketplaces.json")); err != nil {
		t.Fatalf("known_marketplaces.json missing: %v", err)
	}

	mu.Lock()
	gotPaths := append([]string(nil), cdnPaths...)
	mu.Unlock()
	wantPaths := []string{
		"GET /claude-code-releases/plugins/claude-plugins-official/latest",
		"GET /claude-code-releases/plugins/claude-plugins-official/" + testMarketplaceSHA + ".zip",
	}
	if !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("downloads.claude.ai requests = %v, want %v", gotPaths, wantPaths)
	}
	if denials, _ := events.Drain(MaxGuardEvents); len(denials) != 0 {
		t.Fatalf("unexpected denials during cold interactive Claude startup: %+v", denials)
	}
}

func newMarketplaceTestCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "coop-marketplace-test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func newMarketplaceLeafCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, host string) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair, certPEM, keyPEM
}

func buildTestMarketplaceZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("marketplaces/claude-plugins-official/.claude-plugin/marketplace.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, `{"$schema":"https://anthropic.com/claude-code/marketplace.schema.json","name":"claude-plugins-official","description":"Official Claude Code plugins","owner":{"name":"Anthropic","email":"support@anthropic.com"},"plugins":[]}`); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
