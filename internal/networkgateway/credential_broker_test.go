package networkgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func testCredentialBroker(t *testing.T, upstream http.RoundTripper) (*credentialBroker, *BootInstant) {
	t.Helper()
	now := BootInstant(time.Hour)
	clock := testBootClock()
	clock.read = func() (BootInstant, error) { return now, nil }
	b := &credentialBroker{
		route:   CredentialBrokerRoute{Name: "claude", Kind: CredentialBrokerProvider, Upstream: "api.anthropic.com", Header: "x-api-key", Methods: []string{"POST"}, Path: "/v1/messages", Port: 443},
		address: CredentialBrokerAddress(0),
		secret:  CredentialBrokerSecret{Substitute: strings.Repeat("s", 64), Credential: "real-secret-key"},
		clock:   clock, events: NewGuardEvents(clock), slots: make(chan struct{}, maxCredentialBrokerFlows),
	}
	b.setProxy(upstream)
	return b, &now
}

func brokerRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Host = CredentialBrokerAddress(0)
	request.Header.Set("x-api-key", strings.Repeat("s", 64))
	return request
}

func TestCredentialBrokerReplacesOnlyTheQualifiedCredentialAndStreams(t *testing.T) {
	seen := make(chan struct{}, 1)
	b, _ := testCredentialBroker(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "https" || request.URL.Host != "api.anthropic.com" || request.Host != "api.anthropic.com" ||
			request.Header.Get("x-api-key") != "real-secret-key" || request.Header.Get("Authorization") != "" {
			t.Errorf("upstream request widened or retained substitute: %#v", request)
		}
		seen <- struct{}{}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader("data: first\n\ndata: second\n\n"))}, nil
	}))
	recorder := httptest.NewRecorder()
	b.handler().ServeHTTP(recorder, brokerRequest(`{"stream":true}`))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "data: first\n\ndata: second\n\n" {
		t.Fatalf("stream response = %d %q", recorder.Code, recorder.Body.String())
	}
	select {
	case <-seen:
	default:
		t.Fatal("qualified request never reached the fixed upstream")
	}
}

func TestCredentialBrokerSupportsBearerAndNarrowProviderPathPrefixes(t *testing.T) {
	b, _ := testCredentialBroker(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/responses/compact" || request.Header.Get("Authorization") != "Bearer real-secret-key" ||
			request.Header.Get("X-Api-Key") != "" || request.Header.Get("X-Goog-Api-Key") != "" {
			t.Fatalf("brokered bearer request = %#v", request)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}))
	b.route = CredentialBrokerRoute{Name: "codex", Kind: CredentialBrokerProvider, Upstream: "api.openai.com", Header: "authorization",
		HeaderPrefix: "Bearer ", Methods: []string{"POST"}, Path: "/v1/responses", PathPrefix: true, Port: 443}
	b.setProxy(b.proxy.Transport)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader("{}"))
	request.Host = CredentialBrokerAddress(0)
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 64))
	recorder := httptest.NewRecorder()
	b.handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("bearer response = %d %q", recorder.Code, recorder.Body.String())
	}

	// A sibling endpoint, including one the prefix reaches only once the upstream normalizes a dot
	// segment, a doubled slash or an encoded slash — and a slash-terminated path, which only an
	// exact route's own path may be.
	for _, target := range []string{"/v1/chat/completions", "/v1/responses_other", "/v1/responses/../chat/completions",
		"/v1/responses/%2e%2e/chat/completions", "/v1/responses//compact", "/v1/responses%2Fcompact", "/v1/responses/", "/v1/responses/compact/"} {
		request = httptest.NewRequest(http.MethodPost, target, strings.NewReader("{}"))
		request.Host = CredentialBrokerAddress(0)
		request.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 64))
		recorder = httptest.NewRecorder()
		b.handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%s returned %d", target, recorder.Code)
		}
	}
}

// An MCP route carries the protocol's own traffic — POST a message, GET the stream, DELETE the
// session — to its one exact path with the real token in place of the substitute, and nothing else.
func TestCredentialBrokerCarriesAnMCPSessionToItsExactEndpoint(t *testing.T) {
	var seen []string
	b, _ := testCredentialBroker(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer real-secret-key" || request.Host != "mcp.example.com" {
			t.Fatalf("brokered MCP request = %#v", request)
		}
		seen = append(seen, request.Method+" "+request.URL.Path)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Mcp-Session-Id": {"s1"}}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}))
	b.route = CredentialBrokerRoute{Name: "mcp-1", Kind: CredentialBrokerMCP, Upstream: "mcp.example.com", Header: "authorization",
		HeaderPrefix: "Bearer ", Methods: []string{"POST", "GET", "DELETE"}, Path: "/mcp", Port: 443}
	b.setProxy(b.proxy.Transport)
	send := func(method, target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, target, strings.NewReader(""))
		request.Host = CredentialBrokerAddress(0)
		request.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 64))
		request.Header.Set("Mcp-Session-Id", "s1")
		recorder := httptest.NewRecorder()
		b.handler().ServeHTTP(recorder, request)
		return recorder
	}
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		if recorder := send(method, "/mcp"); recorder.Code != http.StatusOK || recorder.Header().Get("Mcp-Session-Id") != "s1" {
			t.Fatalf("%s /mcp returned %d %#v", method, recorder.Code, recorder.Header())
		}
	}
	for _, refused := range [][2]string{{http.MethodPut, "/mcp"}, {http.MethodPost, "/mcp/other"}, {http.MethodPost, "/mcp?next=x"}, {http.MethodPost, "/"}} {
		if recorder := send(refused[0], refused[1]); recorder.Code != http.StatusForbidden {
			t.Fatalf("%s %s returned %d", refused[0], refused[1], recorder.Code)
		}
	}
	if !slices.Equal(seen, []string{"POST /mcp", "GET /mcp", "DELETE /mcp"}) {
		t.Fatalf("upstream saw %q", seen)
	}
}

// A server whose secret rides its own header gets that header, carrying the real credential after the
// route's prefix, and no other credential. The stand-in must sit in exactly that header, and a request
// that brings any other credential header is refused.
func TestCredentialBrokerCarriesAnMCPSecretHeader(t *testing.T) {
	b, _ := testCredentialBroker(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("X-Api-Key") != "key real-secret-key" || request.Header.Get("Authorization") != "" ||
			request.Header.Get("X-Client") != "coop" {
			t.Fatalf("brokered MCP request headers = %#v", request.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}))
	b.route = CredentialBrokerRoute{Name: "mcp-1", Kind: CredentialBrokerMCP, Upstream: "mcp.example.com", Header: "x-api-key",
		HeaderPrefix: "key ", Methods: []string{"POST", "GET", "DELETE"}, Path: "/mcp", Port: 443}
	b.setProxy(b.proxy.Transport)
	send := func(headers map[string]string) int {
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
		request.Host = CredentialBrokerAddress(0)
		request.Header.Set("X-Client", "coop") // a literal header rides as written
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		recorder := httptest.NewRecorder()
		b.handler().ServeHTTP(recorder, request)
		return recorder.Code
	}
	stand := strings.Repeat("s", 64)
	if code := send(map[string]string{"X-Api-Key": "key " + stand}); code != http.StatusOK {
		t.Fatalf("the stand-in in its header returned %d", code)
	}
	for name, headers := range map[string]map[string]string{
		"no prefix":        {"X-Api-Key": stand},
		"a wrong stand-in": {"X-Api-Key": "key " + strings.Repeat("t", 64)},
		"the wrong header": {"Authorization": "key " + stand},
		"a second secret":  {"X-Api-Key": "key " + stand, "Authorization": "Bearer " + stand},
		"no secret at all": {},
	} {
		if code := send(headers); code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Errorf("%s returned %d", name, code)
		}
	}
}

// A download route fetches public bytes for a client the agent's own policy does not let reach that
// host — Codex's curated plugin store. It carries no credential, adds none, refuses a request that
// brings one, and forwards only the exact request lines the operator's adapter declared.
func TestCredentialBrokerDownloadsWithoutACredential(t *testing.T) {
	var sent http.Header
	b, _ := testCredentialBroker(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		sent = request.Header.Clone()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("pack"))}, nil
	}))
	b.route = CredentialBrokerRoute{Name: "codex-plugin-git", Kind: CredentialBrokerDownload, Upstream: "github.com", Port: 443,
		Allow: []BrokerRequestLine{
			{Method: http.MethodGet, Path: "/openai/plugins.git/info/refs", Query: "service=git-upload-pack"},
			{Method: http.MethodPost, Path: "/openai/plugins.git/git-upload-pack"},
		}}
	b.secret = CredentialBrokerSecret{Name: "codex-plugin-git"}
	b.setProxy(b.proxy.Transport)
	send := func(method, target string, headers map[string]string) int {
		request := httptest.NewRequest(method, target, strings.NewReader(""))
		request.Host = CredentialBrokerAddress(0)
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		recorder := httptest.NewRecorder()
		b.handler().ServeHTTP(recorder, request)
		return recorder.Code
	}
	if code := send(http.MethodGet, "/openai/plugins.git/info/refs?service=git-upload-pack", nil); code != http.StatusOK {
		t.Fatalf("the discovery request returned %d", code)
	}
	if sent.Get("Authorization") != "" || sent.Get("X-Api-Key") != "" || sent.Get("Cookie") != "" {
		t.Fatalf("a download carried a credential upstream: %#v", sent)
	}
	if code := send(http.MethodPost, "/openai/plugins.git/git-upload-pack", nil); code != http.StatusOK {
		t.Fatalf("the fetch returned %d", code)
	}
	for name, refused := range map[string]func() int{
		"a push": func() int { return send(http.MethodPost, "/openai/plugins.git/git-receive-pack", nil) },
		// The discovery path serves a push too, by query alone — the half that precedes one.
		"a push's discovery": func() int {
			return send(http.MethodGet, "/openai/plugins.git/info/refs?service=git-receive-pack", nil)
		},
		"another query": func() int {
			return send(http.MethodGet, "/openai/plugins.git/info/refs?service=git-upload-pack&x=1", nil)
		},
		"no query at all":       func() int { return send(http.MethodGet, "/openai/plugins.git/info/refs", nil) },
		"another repository":    func() int { return send(http.MethodGet, "/openai/codex.git/info/refs?service=git-upload-pack", nil) },
		"a query where none is": func() int { return send(http.MethodPost, "/openai/plugins.git/git-upload-pack?sneak=1", nil) },
		"another method":        func() int { return send(http.MethodDelete, "/openai/plugins.git/info/refs", nil) },
		"a request with a token": func() int {
			return send(http.MethodPost, "/openai/plugins.git/git-upload-pack", map[string]string{"Authorization": "Bearer stolen"})
		},
		"a request with cookies": func() int {
			return send(http.MethodPost, "/openai/plugins.git/git-upload-pack", map[string]string{"Cookie": "session=x"})
		},
	} {
		if code := refused(); code != http.StatusForbidden {
			t.Errorf("%s returned %d, want 403", name, code)
		}
	}
}

// A download route's secrets entry must be empty: a file that hands one a credential is refused, so
// a route that fetches public bytes can never be given something to send with them.
func TestDownloadRoutesCarryNoSecret(t *testing.T) {
	config := testLaunch(t)
	config.Brokers = []CredentialBrokerRoute{{Name: "codex-plugin-git", Kind: CredentialBrokerDownload, Upstream: "github.com",
		Port: 443, Allow: []BrokerRequestLine{{Method: http.MethodGet, Path: "/openai/plugins.git/info/refs", Query: "service=git-upload-pack"}}}}
	// A download route that names a credential, or carries a shape a fetch never takes, is refused
	// before it ever listens — by the same judgment, made where the host builds the plan.
	for name, bad := range map[string]CredentialBrokerRoute{
		"a header":       {Name: "d", Kind: CredentialBrokerDownload, Upstream: "github.com", Port: 443, Header: "authorization", Allow: config.Brokers[0].Allow},
		"a path":         {Name: "d", Kind: CredentialBrokerDownload, Upstream: "github.com", Port: 443, Path: "/x", Allow: config.Brokers[0].Allow},
		"no lines":       {Name: "d", Kind: CredentialBrokerDownload, Upstream: "github.com", Port: 443},
		"a dirty path":   {Name: "d", Kind: CredentialBrokerDownload, Upstream: "github.com", Port: 443, Allow: []BrokerRequestLine{{Method: http.MethodGet, Path: "/a/../b"}}},
		"another method": {Name: "d", Kind: CredentialBrokerDownload, Upstream: "github.com", Port: 443, Allow: []BrokerRequestLine{{Method: http.MethodDelete, Path: "/a"}}},
	} {
		if err := CheckBrokerRoutes([]CredentialBrokerRoute{bad}); err == nil {
			t.Errorf("the gateway accepted a download route with %s", name)
		}
	}
	secrets := CredentialBrokerSecrets{Version: 2, RunID: config.RunID, Epoch: config.Epoch,
		Routes: []CredentialBrokerSecret{{Name: "codex-plugin-git"}}}
	data, _ := json.Marshal(secrets)
	if _, err := ReadCredentialBrokerSecrets(bytes.NewReader(data), config); err != nil {
		t.Fatalf("an empty download secret was refused: %v", err)
	}
	for name, mutate := range map[string]func(*CredentialBrokerSecret){
		"a credential": func(s *CredentialBrokerSecret) { s.Credential = "real-token" },
		"a stand-in":   func(s *CredentialBrokerSecret) { s.Substitute = strings.Repeat("s", 64) },
	} {
		changed := secrets
		changed.Routes = []CredentialBrokerSecret{secrets.Routes[0]}
		mutate(&changed.Routes[0])
		data, _ := json.Marshal(changed)
		if _, err := ReadCredentialBrokerSecrets(bytes.NewReader(data), config); err == nil {
			t.Errorf("a download route was given %s", name)
		}
	}
}

// A provider streams its headers at once; an MCP server answering a tool call with JSON sends them
// only when the tool finishes, so only the run's lifetime bounds that wait.
func TestCredentialBrokerWaitsForAnMCPToolsResponseHeaders(t *testing.T) {
	config := testLaunch(t)
	config.Brokers = []CredentialBrokerRoute{
		{Name: "claude", Kind: CredentialBrokerProvider, Upstream: "api.anthropic.com", Header: "x-api-key", Methods: []string{"POST"}, Path: "/v1/messages", Port: 443},
		{Name: "mcp-1", Kind: CredentialBrokerMCP, Upstream: "mcp.example.com", Header: "authorization", HeaderPrefix: "Bearer ",
			Methods: []string{"POST", "GET", "DELETE"}, Path: "/mcp", Port: 443},
	}
	clock := testBootClock()
	doh, err := NewDoH(netip.MustParseAddrPort("1.1.1.1:443"), "cloudflare-dns.com", nil, clock)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []time.Duration{credentialBrokerTimeout, 0} {
		secret := CredentialBrokerSecret{Name: config.Brokers[i].Name, Substitute: strings.Repeat("s", 64), Credential: "real-secret-key"}
		b, err := newCredentialBroker(config, i, secret, clock, doh, NewGuardEvents(clock), ControllerClient{})
		if err != nil {
			t.Fatal(err)
		}
		if b.transport.ResponseHeaderTimeout != want {
			t.Fatalf("%s response header timeout = %s, want %s", config.Brokers[i].Kind, b.transport.ResponseHeaderTimeout, want)
		}
	}
}

func TestCredentialBrokerAllowsQueryOnlyWhenAdapterDeclaresIt(t *testing.T) {
	b, _ := testCredentialBroker(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.RawQuery != "alt=sse" || request.Header.Get("X-Goog-Api-Key") != "real-secret-key" {
			t.Fatalf("brokered Gemini request = %#v", request)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}))
	b.route = CredentialBrokerRoute{Name: "gemini", Kind: CredentialBrokerProvider, Upstream: "generativelanguage.googleapis.com", Header: "x-goog-api-key",
		Methods: []string{"POST"}, Path: "/v1beta/models/", PathPrefix: true, AllowQuery: true, Port: 443}
	b.setProxy(b.proxy.Transport)

	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini:streamGenerateContent?alt=sse", strings.NewReader("{}"))
	request.Host = CredentialBrokerAddress(0)
	request.Header.Set("X-Goog-Api-Key", strings.Repeat("s", 64))
	recorder := httptest.NewRecorder()
	b.handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("Gemini query returned %d", recorder.Code)
	}
}

func TestCredentialBrokerRefusesUpstreamRedirects(t *testing.T) {
	b, _ := testCredentialBroker(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": {"https://attacker.example/"}}, Body: io.NopCloser(strings.NewReader("redirect"))}, nil
	}))
	recorder := httptest.NewRecorder()
	b.handler().ServeHTTP(recorder, brokerRequest("{}"))
	if recorder.Code != http.StatusBadGateway || recorder.Header().Get("Location") != "" {
		t.Fatalf("redirect escaped broker: %d %#v", recorder.Code, recorder.Header())
	}
	events, _ := b.events.Drain(MaxGuardEvents)
	if len(events) != 1 || events[0].Reason != "credential_broker_redirect_refused" {
		t.Fatalf("redirect diagnostic = %#v", events)
	}
}

func TestCredentialBrokerRejectsEveryWiderRequestBeforeUpstream(t *testing.T) {
	calls := 0
	b, _ := testCredentialBroker(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
	}))
	cases := map[string]func(*http.Request){
		"wrong token":      func(r *http.Request) { r.Header.Set("x-api-key", "wrong") },
		"missing token":    func(r *http.Request) { r.Header.Del("x-api-key") },
		"wrong path":       func(r *http.Request) { r.URL.Path = "/v1/complete" },
		"query":            func(r *http.Request) { r.URL.RawQuery = "target=other" },
		"method":           func(r *http.Request) { r.Method = http.MethodConnect },
		"misleading host":  func(r *http.Request) { r.Host = "api.anthropic.com" },
		"authorization":    func(r *http.Request) { r.Header.Set("Authorization", "secret") },
		"proxy credential": func(r *http.Request) { r.Header.Set("Proxy-Authorization", "secret") },
		"cookie":           func(r *http.Request) { r.Header.Set("Cookie", "secret=value") },
		"upgrade":          func(r *http.Request) { r.Header.Set("Upgrade", "websocket") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := brokerRequest("{}")
			mutate(request)
			recorder := httptest.NewRecorder()
			b.handler().ServeHTTP(recorder, request)
			if recorder.Code < 400 {
				t.Fatalf("wider request returned %d", recorder.Code)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("refused requests reached upstream %d times", calls)
	}
}

func TestCredentialBrokerCancellationReachesUpstream(t *testing.T) {
	canceled := make(chan struct{})
	b, _ := testCredentialBroker(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		close(canceled)
		return nil, request.Context().Err()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	request := brokerRequest("{}").WithContext(ctx)
	done := make(chan struct{})
	go func() {
		b.handler().ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("downstream cancellation did not reach upstream")
	}
	<-done
	if events, _ := b.events.Drain(MaxGuardEvents); len(events) != 0 {
		t.Fatalf("client cancellation became a broker failure: %#v", events)
	}
}

func TestCredentialBrokerGenerationCancellationStopsAnActiveStream(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	b, _ := testCredentialBroker(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(entered)
		<-request.Context().Done()
		close(canceled)
		return nil, request.Context().Err()
	}))
	lifecycle, stop := context.WithCancel(context.Background())
	b.lifecycle = lifecycle
	done := make(chan struct{})
	go func() {
		b.handler().ServeHTTP(httptest.NewRecorder(), brokerRequest("{}"))
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("broker stream never reached upstream")
	}
	stop()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("gateway teardown left an active broker stream running")
	}
	<-done
}

func TestCredentialBrokerGenerationDoesNotExpireAfterTwentyFourHours(t *testing.T) {
	b, now := testCredentialBroker(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
	}))
	*now = now.Add(25 * time.Hour)
	recorder := httptest.NewRecorder()
	b.handler().ServeHTTP(recorder, brokerRequest("{}"))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("live generation expired after 24 hours: %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestCredentialBrokerOccupiedPortFailsBeforeReadiness(t *testing.T) {
	listener, err := net.Listen("tcp4", CredentialBrokerAddress(0))
	if err != nil {
		t.Skipf("reserved broker port is already occupied: %v", err)
	}
	defer listener.Close()
	b, _ := testCredentialBroker(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("occupied listener reached upstream")
		return nil, nil
	}))
	ready := false
	if err := b.Serve(context.Background(), func() { ready = true }); err != Failure("credential_broker_listener_unavailable") || ready {
		t.Fatalf("occupied broker listener = %v, ready=%v", err, ready)
	}
}

func TestCredentialBrokerSecretsAreBoundToOneGatewayGeneration(t *testing.T) {
	config := testLaunch(t)
	config.Brokers = []CredentialBrokerRoute{
		{Name: "claude", Kind: CredentialBrokerProvider, Upstream: "api.anthropic.com", Header: "x-api-key", Methods: []string{"POST"}, Path: "/v1/messages", Port: 443},
		{Name: "claude", Kind: CredentialBrokerProvider, Upstream: "api.anthropic.com", Header: "x-api-key", Methods: []string{"POST"}, Path: "/v1/messages", Port: 443},
	}
	secrets := CredentialBrokerSecrets{Version: 2, RunID: config.RunID, Epoch: config.Epoch, Routes: []CredentialBrokerSecret{
		{Name: "claude", Substitute: strings.Repeat("s", 64), Credential: "real-work-key"},
		{Name: "claude", Substitute: strings.Repeat("t", 64), Credential: "real-personal-key"},
	}}
	data, _ := json.Marshal(secrets)
	if _, err := ReadCredentialBrokerSecrets(bytes.NewReader(data), config); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*CredentialBrokerSecrets){
		"other run":         func(s *CredentialBrokerSecrets) { s.RunID = strings.Repeat("c", 32) },
		"other epoch":       func(s *CredentialBrokerSecrets) { s.Epoch = strings.Repeat("d", 32) },
		"older format":      func(s *CredentialBrokerSecrets) { s.Version = 1 },
		"other provider":    func(s *CredentialBrokerSecrets) { s.Routes[1].Name = "codex" },
		"same key":          func(s *CredentialBrokerSecrets) { s.Routes[0].Credential = s.Routes[0].Substitute },
		"shared capability": func(s *CredentialBrokerSecrets) { s.Routes[1].Substitute = s.Routes[0].Substitute },
		"a route missing":   func(s *CredentialBrokerSecrets) { s.Routes = s.Routes[:1] },
	} {
		t.Run(name, func(t *testing.T) {
			changed := secrets
			changed.Routes = append([]CredentialBrokerSecret(nil), secrets.Routes...)
			mutate(&changed)
			data, _ := json.Marshal(changed)
			if _, err := ReadCredentialBrokerSecrets(bytes.NewReader(data), config); err == nil {
				t.Fatal("invalid secret binding accepted")
			}
		})
	}
}

// Two accounts of one provider are two routes, each with its own listener and capability. A
// capability works only at the listener issued for it — presented to the other account's listener
// it is refused before any upstream request — so one account's key never answers for the other.
func TestCredentialBrokerRoutesAreBoundToTheirOwnListener(t *testing.T) {
	upstream := make(chan string, 2)
	serve := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		upstream <- request.Header.Get("x-api-key")
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	work, _ := testCredentialBroker(t, serve)
	personal, _ := testCredentialBroker(t, serve)
	personal.address = CredentialBrokerAddress(1)
	personal.secret = CredentialBrokerSecret{Substitute: strings.Repeat("t", 64), Credential: "real-personal-key"}
	request := func(host, capability string) *http.Request {
		r := brokerRequest(`{}`)
		r.Host = host
		r.Header.Set("x-api-key", capability)
		return r
	}
	for _, attempt := range []struct {
		name   string
		broker *credentialBroker
		req    *http.Request
		code   int
	}{
		{"the personal capability at the work listener", work, request(CredentialBrokerAddress(0), strings.Repeat("t", 64)), http.StatusUnauthorized},
		{"the work listener's address sent to the personal listener", personal, request(CredentialBrokerAddress(0), strings.Repeat("t", 64)), http.StatusForbidden},
		{"each capability at its own listener (work)", work, request(CredentialBrokerAddress(0), strings.Repeat("s", 64)), http.StatusOK},
		{"each capability at its own listener (personal)", personal, request(CredentialBrokerAddress(1), strings.Repeat("t", 64)), http.StatusOK},
	} {
		recorder := httptest.NewRecorder()
		attempt.broker.handler().ServeHTTP(recorder, attempt.req)
		if recorder.Code != attempt.code {
			t.Errorf("%s = %d, want %d", attempt.name, recorder.Code, attempt.code)
		}
	}
	if got := []string{<-upstream, <-upstream}; got[0] != "real-secret-key" || got[1] != "real-personal-key" {
		t.Fatalf("upstream received %v, want each listener's own key once", got)
	}
	select {
	case extra := <-upstream:
		t.Fatalf("a refused capability reached upstream with %q", extra)
	default:
	}
}

// The controller runs one kernel update at a time and answers the rest gateway_lease_capacity. A
// broker flow that opens beside another — a client starting its MCP servers with its model call —
// waits its turn as a guarded flow does, instead of failing the tool call.
func TestCredentialBrokerWaitsOutAConcurrentLeaseUpdate(t *testing.T) {
	config := testLaunch(t)
	config.Brokers = []CredentialBrokerRoute{{Name: "mcp-0", Kind: CredentialBrokerMCP, Upstream: "mcp.example.com", Header: "authorization", HeaderPrefix: "Bearer ",
		Methods: []string{"POST", "GET", "DELETE"}, Path: "/mcp", Port: 443}}
	clock := testBootClock()
	var holding atomic.Bool
	applying, release := make(chan struct{}), make(chan struct{})
	c, err := NewController(Identity{Clock: clock.Domain(), RunID: config.RunID, Epoch: config.Epoch, PolicyFingerprint: config.Policy.Fingerprint},
		config.Policy, nil, nil, nil, nil, netip.Addr{}, config.Brokers, clock, func(_ context.Context, rules string) error {
			if strings.HasPrefix(rules, "flush set") && holding.CompareAndSwap(false, true) {
				close(applying)
				<-release
			}
			return nil
		})
	if err != nil || c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")) != nil {
		t.Fatal("controller setup", err)
	}
	fixed := testBootNow()
	c.now = func() BootInstant { return fixed } // the held update never outlives its own bound
	var asks atomic.Int32
	client, _, _ := startControlFixture(t, c, func(*net.UnixConn) bool { asks.Add(1); return true })
	letGo := sync.OnceFunc(func() { close(release) })
	t.Cleanup(letGo)
	doh, err := NewDoH(netip.MustParseAddrPort("1.1.1.1:443"), "cloudflare-dns.com", nil, clock)
	if err != nil {
		t.Fatal(err)
	}
	secret := CredentialBrokerSecret{Name: "mcp-0", Substitute: strings.Repeat("s", 64), Credential: "real-token"}
	b, err := newCredentialBroker(config, 0, secret, clock, doh, NewGuardEvents(clock), client)
	if err != nil {
		t.Fatal(err)
	}
	b.resolver.exchange = answerExchange(t, func(name string) []dnsmessage.Resource {
		return []dnsmessage.Resource{aRecord(name, "93.184.216.34", 60)}
	})

	held := make(chan error, 1)
	go func() {
		_, err := client.Admit(context.Background(), Lease{Name: "api.example.com", Port: 443, Peer: netip.MustParseAddr("93.184.216.34"), Expires: fixed.Add(time.Minute)})
		held <- err
	}()
	select {
	case <-applying:
	case <-time.After(wait.Deadline):
		t.Fatal("the first kernel update did not start")
	}
	before := asks.Load()
	dialed := make(chan struct{})
	go func() {
		defer close(dialed)
		if conn, err := b.dial(context.Background(), "tcp", "mcp.example.com:443"); err == nil {
			_ = conn.Close() // no Envoy in a unit test: the lease is what this proves
		}
	}()
	// A second ask means the first was refused while the other update held the kernel.
	wait.For(t, "the broker to ask again after a busy reply", func() bool { return asks.Load() >= before+2 })
	letGo()
	select {
	case <-dialed:
	case <-time.After(wait.Deadline):
		t.Fatal("broker dial leaked")
	}
	if err := <-held; err != nil {
		t.Fatal("the held update failed", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, lease := range c.leases {
		if lease.Name == "mcp.example.com" {
			return
		}
	}
	t.Fatal("the broker gave up on a busy controller instead of waiting its turn")
}

func TestControllerKeepsBrokerLeaseOutOfAgentPolicy(t *testing.T) {
	policy := testPolicy(t)
	clock := testBootClock()
	route := CredentialBrokerRoute{Name: "claude", Kind: CredentialBrokerProvider, Upstream: "api.anthropic.com", Header: "x-api-key", Methods: []string{"POST"}, Path: "/v1/messages", Port: 443}
	codex := CredentialBrokerRoute{Name: "codex", Kind: CredentialBrokerProvider, Upstream: "api.openai.com", Header: "authorization", HeaderPrefix: "Bearer ", Methods: []string{"POST"}, Path: "/v1/responses", Port: 443}
	c, err := NewController(Identity{Clock: clock.Domain(), RunID: strings.Repeat("a", 32), Epoch: strings.Repeat("b", 32), PolicyFingerprint: policy.Fingerprint},
		policy, nil, nil, nil, nil, netip.Addr{}, []CredentialBrokerRoute{route, codex}, clock, func(context.Context, string) error { return nil })
	if err != nil || c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")) != nil {
		t.Fatal("controller setup", err)
	}
	now := testBootNow()
	c.now = func() BootInstant { return now }
	lease := Lease{Name: route.Upstream, Peer: netip.MustParseAddr("93.184.216.34"), Port: 443, Expires: now.Add(time.Minute)}
	if _, err := c.Admit(context.Background(), lease); err != Failure("gateway_lease_refused") {
		t.Fatalf("agent policy admitted helper route: %v", err)
	}
	if _, err := c.AdmitBroker(context.Background(), lease); err != nil {
		t.Fatal("typed broker lease refused", err)
	}
	lease.Name = codex.Upstream // every route's own upstream, and only those
	if _, err := c.AdmitBroker(context.Background(), lease); err != nil {
		t.Fatal("second route's broker lease refused", err)
	}
	lease.Name = "other.example.com"
	if _, err := c.AdmitBroker(context.Background(), lease); err != Failure("gateway_lease_refused") {
		t.Fatalf("broker lease widened upstream: %v", err)
	}
}
