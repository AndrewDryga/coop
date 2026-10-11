package networkgateway

import (
	"fmt"
	"io"
	"net/http"
	"testing"
)

func TestNativeBrokerManagedSettingsPreservesPolicySemantics(t *testing.T) {
	for _, family := range []struct{ name, header, prefix string }{{"oauth", "Authorization", "Bearer "}, {"apikey", "X-Api-Key", ""}} {
		for _, status := range []int{200, 204, 304, 404, 401, 403} {
			t.Run(fmt.Sprintf("%s/%d", family.name, status), func(t *testing.T) {
				policy := `{"settings":{"permissions":{"deny":["Bash(curl:*)"]}},"checksum":"provider-checksum"}`
				f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get(family.header) != family.prefix+"private-access-one" || r.Header.Get("If-None-Match") != "provider-cache" {
						t.Error("managed settings authentication/conditional request changed")
					}
					w.Header().Set("ETag", "provider-cache")
					w.Header().Set("X-Policy-Signature", "provider-signature")
					w.WriteHeader(status)
					if status != 204 && status != 304 {
						_, _ = io.WriteString(w, policy)
					}
				}), func(origin *NativeBrokerOrigin) {
					origin.Requests = []NativeBrokerRequestLine{{Method: "GET", Path: "/api/claude_code/settings", Header: family.header, HeaderPrefix: family.prefix, ClientHeader: family.header, ClientPrefix: family.prefix, ClientMarker: "public-selector"}}
				})
				request := f.request(t, "/api/claude_code/settings")
				request.Header.Del("Authorization")
				request.Header.Set(family.header, family.prefix+"public-selector")
				request.Header.Set("If-None-Match", "provider-cache")
				response, body := nativeResponse(t, f.client, request)
				want := policy
				if status == 204 || status == 304 {
					want = ""
				}
				if response.StatusCode != status || body != want || response.Header.Get("ETag") != "provider-cache" || response.Header.Get("X-Policy-Signature") != "provider-signature" {
					t.Fatal("genuine upstream policy/status/cache/signature changed")
				}
				before := f.forwarded.Load()
				for _, target := range []string{"/api/claude_code/settings?extra=1", "/api/claude_code/%73ettings"} {
					request := f.request(t, target)
					request.Header.Del("Authorization")
					request.Header.Set(family.header, family.prefix+"public-selector")
					response, _ := nativeResponse(t, f.client, request)
					if response.StatusCode != 403 {
						t.Fatal("nonexact settings route admitted")
					}
				}
				if f.forwarded.Load() != before {
					t.Fatal("refused settings request reached provider")
				}
			})
		}
	}
}
