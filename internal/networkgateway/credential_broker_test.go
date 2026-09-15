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
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func testCredentialBroker(t *testing.T, upstream http.RoundTripper) (*credentialBroker, *BootInstant) {
	t.Helper()
	now := BootInstant(time.Hour)
	clock := testBootClock()
	clock.read = func() (BootInstant, error) { return now, nil }
	b := &credentialBroker{
		route:  CredentialBrokerRoute{Provider: "claude", Upstream: "api.anthropic.com", Header: "x-api-key", Method: "POST", Path: "/v1/messages", Port: 443},
		secret: CredentialBrokerSecret{Substitute: strings.Repeat("s", 64), Credential: "real-secret-key"},
		clock:  clock, events: NewGuardEvents(clock), slots: make(chan struct{}, maxCredentialBrokerFlows),
	}
	b.setProxy(upstream)
	return b, &now
}

func brokerRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Host = CredentialBrokerAddress
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
	b.route = CredentialBrokerRoute{Provider: "codex", Upstream: "api.openai.com", Header: "authorization",
		HeaderPrefix: "Bearer ", Method: "POST", Path: "/v1/responses", PathPrefix: true, Port: 443}
	b.setProxy(b.proxy.Transport)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader("{}"))
	request.Host = CredentialBrokerAddress
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 64))
	recorder := httptest.NewRecorder()
	b.handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("bearer response = %d %q", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	request.Host = CredentialBrokerAddress
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 64))
	recorder = httptest.NewRecorder()
	b.handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("unrelated path returned %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/responses_other", strings.NewReader("{}"))
	request.Host = CredentialBrokerAddress
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("s", 64))
	recorder = httptest.NewRecorder()
	b.handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("sibling path returned %d", recorder.Code)
	}
}

func TestCredentialBrokerAllowsQueryOnlyWhenAdapterDeclaresIt(t *testing.T) {
	b, _ := testCredentialBroker(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.RawQuery != "alt=sse" || request.Header.Get("X-Goog-Api-Key") != "real-secret-key" {
			t.Fatalf("brokered Gemini request = %#v", request)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}))
	b.route = CredentialBrokerRoute{Provider: "gemini", Upstream: "generativelanguage.googleapis.com", Header: "x-goog-api-key",
		Method: "POST", Path: "/v1beta/models/", PathPrefix: true, AllowQuery: true, Port: 443}
	b.setProxy(b.proxy.Transport)

	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini:streamGenerateContent?alt=sse", strings.NewReader("{}"))
	request.Host = CredentialBrokerAddress
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
	listener, err := net.Listen("tcp4", CredentialBrokerAddress)
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

func TestCredentialBrokerSecretIsBoundToOneGatewayGeneration(t *testing.T) {
	config := testLaunch(t)
	config.Broker = &CredentialBrokerRoute{Provider: "claude", Upstream: "api.anthropic.com", Header: "x-api-key", Method: "POST", Path: "/v1/messages", Port: 443}
	secret := CredentialBrokerSecret{Version: 1, RunID: config.RunID, Epoch: config.Epoch, Provider: "claude",
		Substitute: strings.Repeat("s", 64), Credential: "real-secret-key"}
	data, _ := json.Marshal(secret)
	if _, err := ReadCredentialBrokerSecret(bytes.NewReader(data), config); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*CredentialBrokerSecret){
		"other run":      func(s *CredentialBrokerSecret) { s.RunID = strings.Repeat("c", 32) },
		"other epoch":    func(s *CredentialBrokerSecret) { s.Epoch = strings.Repeat("d", 32) },
		"other provider": func(s *CredentialBrokerSecret) { s.Provider = "codex" },
		"same key":       func(s *CredentialBrokerSecret) { s.Credential = s.Substitute },
	} {
		t.Run(name, func(t *testing.T) {
			changed := secret
			mutate(&changed)
			data, _ := json.Marshal(changed)
			if _, err := ReadCredentialBrokerSecret(bytes.NewReader(data), config); err == nil {
				t.Fatal("invalid secret binding accepted")
			}
		})
	}
}

func TestControllerKeepsBrokerLeaseOutOfAgentPolicy(t *testing.T) {
	policy := testPolicy(t)
	clock := testBootClock()
	route := &CredentialBrokerRoute{Provider: "claude", Upstream: "api.anthropic.com", Header: "x-api-key", Method: "POST", Path: "/v1/messages", Port: 443}
	c, err := NewController(Identity{Clock: clock.Domain(), RunID: strings.Repeat("a", 32), Epoch: strings.Repeat("b", 32), PolicyFingerprint: policy.Fingerprint},
		policy, nil, nil, nil, nil, netip.Addr{}, route, clock, func(context.Context, string) error { return nil })
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
	lease.Name = "other.example.com"
	if _, err := c.AdmitBroker(context.Background(), lease); err != Failure("gateway_lease_refused") {
		t.Fatalf("broker lease widened upstream: %v", err)
	}
}
