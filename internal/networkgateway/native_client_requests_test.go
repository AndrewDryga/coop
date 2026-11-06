package networkgateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/networkview"
)

// nativeAdapterRoutes installs one adapter's declared routes for host on the fixture origin, as the
// box does: an authenticated line checks the public selector and carries the selected credential.
func nativeAdapterRoutes(t *testing.T, provider, selection, host, account string) func(*NativeBrokerOrigin) {
	t.Helper()
	agent, _ := agents.Get(provider)
	routes, err := agent.NativeCredentials().Broker.Routes(selection)
	if err != nil {
		t.Fatal(err)
	}
	return func(origin *NativeBrokerOrigin) {
		origin.Requests, origin.AccountHeaders, origin.ClientAccountHeaders = nil, nil, nil
		for _, route := range routes {
			if route.Host != host {
				continue
			}
			line := NativeBrokerRequestLine{Method: route.Method, Path: route.Path, Query: route.Query, Segment: route.Segment, Suffix: route.Suffix,
				CredentialFree: route.CredentialFree, Header: route.Header, HeaderPrefix: route.HeaderPrefix}
			for _, query := range route.Queries {
				line.Queries = append(line.Queries, NativeBrokerQuery{Name: query.Name, Values: query.Values, Optional: query.Optional, MaxBytes: query.MaxBytes})
			}
			if !route.CredentialFree {
				line.ClientHeader, line.ClientPrefix, line.ClientMarker = route.Header, route.HeaderPrefix, "public-selector"
			}
			origin.Requests = append(origin.Requests, line)
			if route.AccountHeader != "" {
				origin.AccountHeaders = map[string]string{route.AccountHeader: account}
				origin.ClientAccountHeaders = map[string]string{route.AccountHeader: "public-account"}
			}
		}
	}
}

// Every Codex and Grok run on a sign-in ended with "Coop refused this request to the agent's
// provider", and the record could not say which. Captured offline from the pinned clients: Codex
// 0.159.2 asks for the second page of the plugin catalog with an opaque pageToken (refusing it
// failed the whole catalog), and Grok 1.0.44 checks its API host with a bare GET / and no
// credential. Both are now admitted from the adapters' own routes; their neighbours are still
// refused, and each refusal records which check refused it and the request's class, never a value.
func TestNativeBrokerAdmitsThePagesAndProbeThePinnedClientsSend(t *testing.T) {
	token := "opaque-" + strings.Repeat("Zm9v", 512) // a page token is the provider's own cursor
	for _, tc := range []struct {
		name, provider, selection, host, account string
		admitted                                 []string
		refused                                  map[string]string // request -> recorded class
	}{
		{name: "codex plugin pages", provider: "codex", selection: "chatgpt", host: "chatgpt.com", account: "selected-id",
			admitted: []string{
				"/backend-api/ps/plugins/list?scope=GLOBAL&limit=200",
				"/backend-api/ps/plugins/list?scope=GLOBAL&limit=200&pageToken=" + token,
				"/backend-api/ps/plugins/installed?limit=200",
				"/backend-api/ps/plugins/installed?limit=200&pageToken=" + token,
				"/backend-api/ps/plugins/installed?limit=200&includeDownloadUrls=true",
				"/backend-api/ps/plugins/installed?limit=200&includeDownloadUrls=true&pageToken=" + token,
			},
			refused: map[string]string{
				"/backend-api/ps/plugins/list?scope=GLOBAL&limit=200&pageToken=a&pageToken=b":                "GET /backend-api/ps/plugins/list?limit&pageToken&scope",
				"/backend-api/ps/plugins/list?scope=GLOBAL&limit=200&pageToken=" + strings.Repeat("x", 4097): "GET /backend-api/ps/plugins/list?limit&pageToken&scope",
				"/backend-api/ps/plugins/list?scope=GLOBAL&limit=200&pageToken=%0A":                          "GET /backend-api/ps/plugins/list?limit&pageToken&scope",
				"/backend-api/ps/plugins/list?scope=USER&limit=200&pageToken=" + token:                       "GET /backend-api/ps/plugins/list?limit&pageToken&scope",
				"/backend-api/ps/plugins/list?scope=GLOBAL&limit=100":                                        "GET /backend-api/ps/plugins/list?limit&scope",
				"/backend-api/ps/plugins/list?scope=GLOBAL&pageToken=" + token:                               "GET /backend-api/ps/plugins/list?pageToken&scope",
				"/backend-api/ps/plugins/list?scope=GLOBAL&limit=200&collection=x":                           "GET /backend-api/ps/plugins/list?collection&limit&scope",
				"/backend-api/ps/plugins/installed?limit=200&includeDownloadUrls=false":                      "GET /backend-api/ps/plugins/installed?includeDownloadUrls&limit",
				"/backend-api/ps/plugins/installed?scope=GLOBAL":                                             "GET /backend-api/ps/plugins/installed?scope",
				"/backend-api/ps/plugins/plugin_ab12c3?pageToken=" + token:                                   "GET /backend-api/ps/plugins/*?pageToken",
			}},
		{name: "grok probe", provider: "grok", selection: "https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828", host: "cli-chat-proxy.grok.com",
			admitted: []string{"/"},
			refused:  map[string]string{"/?probe=1": "GET /?probe", "//": "GET //", "/v1": "GET /v1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// An authenticated line gets the selected credential and account; the probe gets neither.
			wantAuth, wantAccount := "Bearer private-access-one", tc.account
			if tc.account == "" {
				wantAuth = ""
			}
			f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != wantAuth || r.Header.Get("ChatGPT-Account-ID") != wantAccount || r.Header.Get("Cookie") != "" {
					t.Errorf("%s reached the provider with the wrong credentials", r.URL.Path)
				}
				_, _ = io.WriteString(w, "provider")
			}), nativeAdapterRoutes(t, tc.provider, tc.selection, tc.host, tc.account))
			events := NewGuardEvents(nil)
			f.broker.events = events
			send := func(target string) (*http.Response, string) {
				request := f.request(t, target)
				request.Header.Set("ChatGPT-Account-ID", "public-account")
				request.Header.Set("Cookie", "guest=1")
				if tc.account == "" {
					request.Header.Del("ChatGPT-Account-ID")
				}
				return nativeResponse(t, f.client, request)
			}
			for _, target := range tc.admitted {
				if response, body := send(target); response.StatusCode != http.StatusOK || body != "provider" {
					t.Errorf("%.80s was not admitted: %d %q", target, response.StatusCode, body)
				}
			}
			before := f.forwarded.Load()
			for target := range tc.refused {
				if response, _ := send(target); response.StatusCode != http.StatusForbidden {
					t.Errorf("%.80s was admitted: %d", target, response.StatusCode)
				}
			}
			if f.forwarded.Load() != before {
				t.Fatal("a refused request reached the provider")
			}
			recorded, _ := events.Drain(MaxGuardEvents)
			var classes []string
			for _, event := range recorded {
				if event.Kind != "admission_failed" || event.Name != "example.com" || event.Reason != "native_broker_request_refused" {
					t.Errorf("refusal recorded as %+v", event)
				}
				if strings.Contains(event.Request, "opaque") || strings.Contains(event.Request, "xxxx") || strings.Contains(event.Request, "plugin_ab12c3") {
					t.Errorf("a request value reached the record: %q", event.Request)
				}
				classes = append(classes, event.Request)
			}
			for target, class := range tc.refused {
				found := false
				for _, recorded := range classes {
					found = found || recorded == class
				}
				if !found {
					t.Errorf("%.80s: want class %q in %q", target, class, classes)
				}
			}
			if len(classes) != len(tc.refused) {
				t.Errorf("recorded %d refusals, want %d: %q", len(classes), len(tc.refused), classes)
			}
		})
	}
}

// Each check that refuses a sign-in request names itself in the record, so a refusal says which
// rule it broke: the route, the account selector, or the host.
func TestNativeBrokerRecordsWhichCheckRefused(t *testing.T) {
	f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "provider") }))
	events := NewGuardEvents(nil)
	f.broker.events = events
	for _, tc := range []struct{ name, target, header, value string }{
		{"native_broker_request_refused", "/elsewhere", "", ""},
		{"native_broker_account_refused", "/http", "ChatGPT-Account-ID", "another-account"},
		{"native_broker_account_refused", "/http", "Authorization", "Bearer another-selector"},
	} {
		request := f.request(t, tc.target)
		if tc.header != "" {
			request.Header.Set(tc.header, tc.value)
		}
		if response, _ := nativeResponse(t, f.client, request); response.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: %d", tc.name, response.StatusCode)
		}
		recorded, _ := events.Drain(MaxGuardEvents)
		if len(recorded) != 1 || recorded[0].Reason != tc.name || recorded[0].Request != "GET "+tc.target {
			t.Errorf("%s %s recorded as %+v", tc.target, tc.header, recorded)
		}
	}
	request := f.request(t, "/http")
	request.Host = "other.example.com"
	if response, _ := nativeResponse(t, f.client, request); response.StatusCode != http.StatusForbidden {
		t.Fatalf("another host was served: %d", response.StatusCode)
	}
	if recorded, _ := events.Drain(MaxGuardEvents); len(recorded) != 1 || recorded[0].Reason != "native_broker_origin_refused" || recorded[0].Name != "example.com" {
		t.Errorf("another host recorded as %+v", recorded)
	}
}

// The class keeps the shape of a request and drops everything it carried.
func TestNativeRequestClassNeverCarriesAValue(t *testing.T) {
	for target, want := range map[string]string{
		"/backend-api/wham/rate-limit-reset-credits":                                "GET /backend-api/wham/rate-limit-reset-credits",
		"/.well-known/oauth-protected-resource/backend-api/ps/mcp":                  "GET /.well-known/oauth-protected-resource/backend-api/ps/mcp",
		"/v1/sessions/0b5c2f6e-6b9e-4a8e-9f57-a3c1d2e4f5a6/data":                    "GET /v1/sessions/*/data",
		"/files/sk-SECRET123/content?sig=SECRET&exp=1":                              "GET /files/*/content?exp&sig",
		"/download?a3f9c2e1d4b5&client_version=1":                                   "GET /download?*&client_version",
		"/" + strings.Repeat("secret", 7) + "?" + strings.Repeat("Name", 11) + "=1": "GET /*?*",
		"/a?" + strings.Repeat("x", 41) + "=1&%zz":                                  "GET /a?…",
		"/a/b/c/d/e/f/g/h/i/j":                                                      "GET /a/b/c/d/e/f/g/h/…",
		"/" + strings.Repeat("plugins/", 30) + "?" + strings.Repeat("q", 9):         "GET /plugins/plugins/plugins/plugins/plugins/plugins/plugins/plugins/…?qqqqqqqqq",
	} {
		request, err := http.NewRequest(http.MethodGet, "https://example.com"+target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := NativeRequestClass(request); got != want || strings.Contains(got, "SECRET") {
			t.Errorf("%.60s: class %q, want %q", target, got, want)
		}
	}
	query := ""
	for name := range "abcdefgh" {
		query += string(rune('a'+name)) + strings.Repeat("n", 30) + "=1&"
	}
	long, _ := http.NewRequest(http.MethodGet, "https://example.com/ab/ab/?"+query, nil)
	if got := NativeRequestClass(long); len(got) > networkview.MaxRequestClass || !strings.HasSuffix(got, "…") {
		t.Errorf("long class is %d bytes: %q", len(got), got)
	}
	connect := &http.Request{Method: http.MethodConnect, Host: "chatgpt.com:443"}
	odd := &http.Request{Method: "M\x1bOVE"}
	if NativeRequestClass(connect) != "CONNECT" || NativeRequestClass(odd) != "OTHER" {
		t.Errorf("CONNECT or an odd method kept its target: %q %q", NativeRequestClass(connect), NativeRequestClass(odd))
	}
}
