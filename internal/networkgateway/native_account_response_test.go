package networkgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const accountDiscoveryFixture = `{"accounts":[{"id":"sibling","workspace_backend_origin":"NO_CONSTRAINT","account_routing_override":"NO_CONSTRAINT"},{"id":"selected-id","workspace_backend_origin":"https://chatgpt.com","account_routing_override":"us_cr","plan_type":"pro","unknown":{"keep":true}}],"default_account_id":"sibling","account_ordering":["sibling","selected-id"],"unknown":"selected-id"}`

func accountResponseShape() NativeBrokerAccountResponse {
	return NativeBrokerAccountResponse{Header: "ChatGPT-Account-ID", CollectionKey: "accounts", IDKey: "id", LegacyObjectKey: "account", LegacyIDKey: "account_id", OrderingKey: "account_ordering", DefaultIDKey: "default_account_id"}
}

func TestNativeAccountResponseUsesAdapterDeclaredKeys(t *testing.T) {
	shape := NativeBrokerAccountResponse{Header: "X-Selected", CollectionKey: "entries", IDKey: "identifier", LegacyObjectKey: "member", LegacyIDKey: "identifier", OrderingKey: "order", DefaultIDKey: "preferred"}
	for _, body := range []string{
		`{"entries":[{"identifier":"selected-id","route":"restricted"}],"order":["selected-id"],"preferred":"selected-id","accounts":"untouched"}`,
		`{"entries":{"old-key":{"member":{"identifier":"selected-id"},"route":"restricted"}},"order":["old-key"],"preferred":"selected-id","accounts":"untouched"}`,
	} {
		response := accountResponse(body)
		if err := projectNativeAccountResponse(response, shape, "selected-id", "public-account"); err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(response.Body)
		if strings.Contains(string(data), "selected-id") || !strings.Contains(string(data), `"preferred":"public-account"`) || !strings.Contains(string(data), `"route":"restricted"`) || !strings.Contains(string(data), `"accounts":"untouched"`) {
			t.Fatalf("adapter grammar was ignored: %s", data)
		}
	}
}

func TestNativeAccountResponseDescriptorDenials(t *testing.T) {
	for _, mutate := range []func(*NativeBrokerAccountResponse){
		func(s *NativeBrokerAccountResponse) { s.Header = "" },
		func(s *NativeBrokerAccountResponse) { s.CollectionKey = "" },
		func(s *NativeBrokerAccountResponse) { s.IDKey = "id.path" },
		func(s *NativeBrokerAccountResponse) { s.LegacyObjectKey = strings.Repeat("x", 129) },
		func(s *NativeBrokerAccountResponse) { s.LegacyIDKey = "id\n" },
		func(s *NativeBrokerAccountResponse) { s.OrderingKey = s.CollectionKey },
		func(s *NativeBrokerAccountResponse) { s.DefaultIDKey = s.OrderingKey },
	} {
		shape := accountResponseShape()
		mutate(&shape)
		if shape.valid() {
			t.Fatal("unsafe adapter shape accepted")
		}
	}
}

func TestNativeBrokerAccountProjectionConfigurationDenials(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected upstream call") }))
	defer upstream.Close()
	binding := NativeBrokerBinding{RunID: "run-one", Provider: "fixture", Account: "selected", Epoch: 7}
	for _, test := range []struct {
		name   string
		change func(*NativeBrokerOrigin)
	}{
		{"invalid-key", func(o *NativeBrokerOrigin) { o.Requests[0].AccountResponse.IDKey = "id.path" }},
		{"post", func(o *NativeBrokerOrigin) { o.Requests[0].Method = "POST" }},
		{"credential-free", func(o *NativeBrokerOrigin) {
			l := &o.Requests[0]
			l.CredentialFree = true
			l.Header, l.HeaderPrefix, l.ClientHeader, l.ClientPrefix, l.ClientMarker = "", "", "", "", ""
		}},
		{"fixed-query", func(o *NativeBrokerOrigin) { o.Requests[0].Query = "fixed=1" }},
		{"query-rule", func(o *NativeBrokerOrigin) {
			o.Requests[0].Queries = []NativeBrokerQuery{{Name: "fixed", Values: []string{"1"}}}
		}},
		{"segment", func(o *NativeBrokerOrigin) { o.Requests[0].Segment = "token" }},
		{"suffix", func(o *NativeBrokerOrigin) { o.Requests[0].Suffix = "/suffix" }},
		{"no-client-marker", func(o *NativeBrokerOrigin) { o.Requests[0].ClientMarker, o.Requests[0].ClientHeader = "", "" }},
		{"unbound-header", func(o *NativeBrokerOrigin) { o.Requests[0].AccountResponse.Header = "X-Other-Account" }},
		{"same-identity", func(o *NativeBrokerOrigin) { o.ClientAccountHeaders["ChatGPT-Account-ID"] = "selected-id" }},
		{"no-public-identity", func(o *NativeBrokerOrigin) { o.ClientAccountHeaders = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			origin := NativeBrokerOrigin{Host: "example.com", Certificate: upstream.TLS.Certificates[0], AccountHeaders: map[string]string{"ChatGPT-Account-ID": "selected-id"}, DialContext: func(context.Context, string, string) (net.Conn, error) {
				calls++
				return nil, fmt.Errorf("unexpected dial")
			}}
			discoveryRoute(&origin)
			test.change(&origin)
			broker, err := NewNativeBroker(context.Background(), binding, []NativeBrokerOrigin{origin}, func(context.Context) (NativeAccessSnapshot, error) {
				calls++
				return NativeAccessSnapshot{Binding: binding, Revision: 1, Credential: "private-test-only", Expires: time.Now().Add(time.Hour)}, nil
			}, http.NotFoundHandler())
			if broker != nil {
				_ = broker.Close()
			}
			if broker != nil || err != Failure("native_broker_configuration_invalid") || calls != 0 {
				t.Fatalf("unsafe configuration admitted: broker%t error%v calls%d", broker != nil, err, calls)
			}
		})
	}
}

func TestNativeBrokerAccountProjectionDeclarationIsImmutable(t *testing.T) {
	var caller *NativeBrokerAccountResponse
	f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, accountDiscoveryFixture) }), func(o *NativeBrokerOrigin) { discoveryRoute(o); caller = o.Requests[0].AccountResponse })
	caller.CollectionKey, caller.Header = "wrong-key", "X-Wrong"
	request := f.request(t, "/accounts")
	request.Header.Set("ChatGPT-Account-ID", "public-account")
	response, body := nativeResponse(t, f.client, request)
	if response.StatusCode != 200 || !strings.Contains(body, `"id":"public-account"`) {
		t.Fatal("caller mutated active projection")
	}
}

func accountResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestNativeAccountResponsePreservesRoutingAndSiblings(t *testing.T) {
	response := accountResponse(accountDiscoveryFixture)
	for _, name := range []string{"ETag", "Last-Modified", "Digest", "Content-MD5"} {
		response.Header.Set(name, "obsolete")
	}
	if err := projectNativeAccountResponse(response, accountResponseShape(), "selected-id", "public-account"); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		t.Fatal("invalid projected JSON")
	}
	entries := doc["accounts"].([]any)
	selected := entries[1].(map[string]any)
	if selected["id"] != "public-account" || selected["workspace_backend_origin"] != "https://chatgpt.com" || selected["account_routing_override"] != "us_cr" || selected["plan_type"] != "pro" || selected["unknown"].(map[string]any)["keep"] != true || entries[0].(map[string]any)["id"] != "sibling" || doc["default_account_id"] != "sibling" || doc["unknown"] != "selected-id" || doc["account_ordering"].([]any)[1] != "public-account" {
		t.Fatalf("projection changed routing, selected the default, or replaced unrelated metadata: %s", body)
	}
	if response.ContentLength != int64(len(body)) || response.Header.Get("Content-Length") != strconv.Itoa(len(body)) || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("incorrect projected framing/cache")
	}
	for _, name := range []string{"ETag", "Last-Modified", "Digest", "Content-MD5"} {
		if response.Header.Get(name) != "" {
			t.Fatal("stale integrity header", name)
		}
	}
	response = accountResponse(strings.ReplaceAll(accountDiscoveryFixture, `"https://chatgpt.com"`, `"NO_CONSTRAINT"`))
	if err := projectNativeAccountResponse(response, accountResponseShape(), "selected-id", "public-account"); err != nil {
		t.Fatal("legitimate upstream routing sentinel refused", err)
	}
}

func TestNativeAccountResponseDenials(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"missing", `{"accounts":[{"id":"sibling"}]}`},
		{"duplicate-selection", `{"accounts":[{"id":"selected-id"},{"id":"selected-id"}]}`},
		{"alias-collision", `{"accounts":[{"id":"selected-id"},{"id":"public-account"}]}`},
		{"duplicate-envelope-key", `{"accounts":[],"accounts":[{"id":"selected-id"}]}`},
		{"duplicate-id-key", `{"accounts":[{"id":"sibling","id":"selected-id"}]}`},
		{"duplicate-unknown-key", `{"accounts":[{"id":"selected-id","x":1,"x":2}]}`},
		{"id-type", `{"accounts":[{"id":10}]}`},
		{"malformed-sibling", `{"accounts":[{"id":"selected-id"},false]}`},
		{"truncated", `{"accounts":[{"id":"selected-id"}]`},
		{"trailing", accountDiscoveryFixture + `{}`},
		{"oversized", accountDiscoveryFixture + strings.Repeat(" ", 1<<20)},
		{"scalar", `{"accounts":true}`},
		{"legacy-key-collision", `{"accounts":{"real":{"account":{"account_id":"selected-id"}},"public-account":{"account":{"account_id":"sibling"}}},"account_ordering":["real"]}`},
		{"legacy-unlisted", `{"accounts":{"real":{"account":{"account_id":"selected-id"}}},"account_ordering":[]}`},
		{"deep", `{"accounts":[{"id":"selected-id"}],"x":` + strings.Repeat("[", 65) + `0` + strings.Repeat("]", 65) + `}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if projectNativeAccountResponse(accountResponse(test.body), accountResponseShape(), "selected-id", "public-account") == nil {
				t.Fatal("unsafe discovery admitted")
			}
		})
	}
	for _, status := range []int{204, 304} {
		response := accountResponse(accountDiscoveryFixture)
		response.StatusCode = status
		if projectNativeAccountResponse(response, accountResponseShape(), "selected-id", "public-account") == nil {
			t.Fatal("missing discovery body accepted", status)
		}
	}
	response := accountResponse(accountDiscoveryFixture)
	response.Header.Set("Content-Encoding", "gzip")
	if projectNativeAccountResponse(response, accountResponseShape(), "selected-id", "public-account") == nil {
		t.Fatal("unsupported encoding accepted")
	}
}

func TestNativeAccountResponseLegacyIdentityOnly(t *testing.T) {
	response := accountResponse(`{"accounts":{"different-map-key":{"account":{"account_id":"selected-id","plan_type":"pro"},"workspace_backend_origin":"NO_CONSTRAINT"}},"account_ordering":["different-map-key"],"default_account_id":"selected-id"}`)
	if err := projectNativeAccountResponse(response, accountResponseShape(), "selected-id", "public-account"); err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		t.Fatal("invalid legacy document")
	}
	entry := doc["accounts"].(map[string]any)["public-account"].(map[string]any)
	account := entry["account"].(map[string]any)
	if account["account_id"] != "public-account" || account["workspace_backend_origin"] != nil || account["account_routing_override"] != nil || doc["account_ordering"].([]any)[0] != "public-account" || doc["default_account_id"] != "public-account" {
		t.Fatalf("legacy projection fabricated routing: %s", body)
	}
}

func TestNativeAccountResponseLegacySkipsAbsentOrderingReferences(t *testing.T) {
	for _, ordering := range []string{`["stale","real"]`, `["real","public-account","stale","real"]`} {
		response := accountResponse(`{"accounts":{"real":{"account":{"account_id":"selected-id"}}},"account_ordering":` + ordering + `}`)
		if err := projectNativeAccountResponse(response, accountResponseShape(), "selected-id", "public-account"); err != nil {
			t.Fatal("native-compatible stale ordering refused", err)
		}
		body, _ := io.ReadAll(response.Body)
		var doc struct{ Accounts map[string]json.RawMessage }
		if json.Unmarshal(body, &doc) != nil || len(doc.Accounts) != 1 || doc.Accounts["public-account"] == nil {
			t.Fatal("legacy selection was not projected exactly once")
		}
	}
}

func discoveryRoute(origin *NativeBrokerOrigin) {
	shape := accountResponseShape()
	origin.Requests = []NativeBrokerRequestLine{{Method: "GET", Path: "/accounts", Header: "Authorization", HeaderPrefix: "Bearer ", ClientHeader: "Authorization", ClientPrefix: "Bearer ", ClientMarker: "public-selector", AccountResponse: &shape}}
	origin.ClientAccountHeaders = map[string]string{"ChatGPT-Account-ID": "public-account"}
}

func TestNativeBrokerAccountDiscoveryThroughCONNECT(t *testing.T) {
	f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("ChatGPT-Account-ID") != "selected-id" || r.Header.Get("Authorization") != "Bearer private-access-one" || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("If-None-Match") != "" {
			t.Error("discovery lost immutable selection or encoding/cache boundary")
		}
		_, _ = io.WriteString(w, accountDiscoveryFixture)
	}), discoveryRoute)
	request := f.request(t, "/accounts")
	request.Header.Set("ChatGPT-Account-ID", "public-account")
	request.Header.Set("Accept-Encoding", "gzip")
	request.Header.Set("If-None-Match", "old")
	response, body := nativeResponse(t, f.client, request)
	if response.StatusCode != 200 || !strings.Contains(body, `"id":"public-account"`) || !strings.Contains(body, `"account_routing_override":"us_cr"`) {
		t.Fatalf("discovery not projected: %d %s", response.StatusCode, body)
	}
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "real upstream denial")
			}), discoveryRoute)
			request := f.request(t, "/accounts")
			request.Header.Set("ChatGPT-Account-ID", "public-account")
			response, body := nativeResponse(t, f.client, request)
			if response.StatusCode != status || body != "real upstream denial" {
				t.Fatal("provider error semantics changed")
			}
		})
	}
	t.Run("malformed-body-fails-before-forwarding", func(t *testing.T) {
		f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"accounts":[{"id":"secret-real-id"}]}`)
		}), discoveryRoute)
		request := f.request(t, "/accounts")
		request.Header.Set("ChatGPT-Account-ID", "public-account")
		response, body := nativeResponse(t, f.client, request)
		if response.StatusCode != 502 || strings.Contains(body, "secret-real-id") {
			t.Fatal("failed projection emitted upstream body")
		}
	})
}
