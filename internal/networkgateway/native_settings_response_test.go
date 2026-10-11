package networkgateway

import (
	"fmt"
	"io"
	"net/http"
	"testing"
)

func TestNativeBrokerManagedSettingsPreservesPolicySemantics(t *testing.T) {
	for _, family := range []struct{ name, header, prefix string }{{"oauth", "Authorization", "Bearer "}, {"apikey", "X-Api-Key", ""}} {
		for _, target := range []struct{ path, encoded string }{
			{"/api/claude_code/settings", "/api/claude_code/%73ettings"},
			{"/api/claude_code/policy_limits", "/api/claude_code/%70olicy_limits"},
		} {
			for _, status := range []int{200, 204, 304, 404, 401, 403} {
				t.Run(fmt.Sprintf("%s%s/%d", family.name, target.path, status), func(t *testing.T) {
					policy := `{"settings":{"permissions":{"deny":["Bash(curl:*)"]}},"checksum":"provider-checksum"}`
					if target.path == "/api/claude_code/policy_limits" {
						policy = `{"restrictions":{"organization-policy":{"allowed":false}}}`
					}
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
						origin.Requests = []NativeBrokerRequestLine{{Method: "GET", Path: target.path, Header: family.header, HeaderPrefix: family.prefix, ClientHeader: family.header, ClientPrefix: family.prefix, ClientMarker: "public-selector"}}
					})
					request := f.request(t, target.path)
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
					wrongFamily := "Authorization"
					if family.header == wrongFamily {
						wrongFamily = "X-Api-Key"
					}
					selector := family.prefix + "public-selector"
					for _, invalid := range []struct {
						path, method, header string
						values               []string
					}{
						{target.path + "?extra=1", "GET", family.header, []string{selector}},
						{target.encoded, "GET", family.header, []string{selector}},
						{target.path, "POST", family.header, []string{selector}},
						{target.path, "GET", wrongFamily, []string{selector}},
						{target.path, "GET", family.header, nil},
						{target.path, "GET", family.header, []string{family.prefix + "wrong-selector"}},
						{target.path, "GET", family.header, []string{selector, selector}},
					} {
						request := f.request(t, invalid.path)
						request.Method = invalid.method
						request.Header.Del("Authorization")
						request.Header.Del("X-Api-Key")
						for _, value := range invalid.values {
							request.Header.Add(invalid.header, value)
						}
						response, _ := nativeResponse(t, f.client, request)
						if response.StatusCode != 403 {
							t.Fatal("nonexact policy route admitted")
						}
					}
					if f.forwarded.Load() != before {
						t.Fatal("refused policy request reached provider")
					}
				})
			}
		}
	}
}
