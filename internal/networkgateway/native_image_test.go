package networkgateway

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestNativeCodexImagesReachSelectedProvider(t *testing.T) {
	// The native image tool failed locally with "native route refused" and
	// returned no image. Consume the adapter's real declarations so this fails
	// when a client capability is missing even though generic proxy tests pass.
	for _, family := range []struct{ selection, host, base, account string }{
		{"chatgpt", "chatgpt.com", "/backend-api/codex", "selected-id"},
		{"apikey", "api.openai.com", "/v1", ""},
	} {
		for _, operation := range []string{"generations", "edits"} {
			for _, status := range []int{200, 403, 429} {
				t.Run(fmt.Sprintf("%s/%s/%d", family.selection, operation, status), func(t *testing.T) {
					path := family.base + "/images/" + operation
					body := `{"prompt":"Draw a cat","model":"gpt-image-2"}`
					f := newNativeTransportFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						got, err := io.ReadAll(r.Body)
						if err != nil || string(got) != body || r.Method != "POST" || r.URL.Path != path || r.Header.Get("Authorization") != "Bearer private-access-one" || r.Header.Get("ChatGPT-Account-ID") != family.account {
							t.Error("image request body, endpoint or selected credentials changed")
						}
						w.Header().Set("X-Codex-Imagegen-Request-Id", "image-request")
						w.WriteHeader(status)
						_, _ = io.WriteString(w, "provider-image-result")
					}), nativeAdapterRoutes(t, "codex", family.selection, family.host, family.account))
					request := f.request(t, path)
					request.Method, request.Body = "POST", io.NopCloser(strings.NewReader(body))
					request.Header.Del("ChatGPT-Account-ID")
					if family.account != "" {
						request.Header.Set("ChatGPT-Account-ID", "public-account")
					}
					response, got := nativeResponse(t, f.client, request)
					if response.StatusCode != status || got != "provider-image-result" || response.Header.Get("X-Codex-Imagegen-Request-Id") != "image-request" {
						t.Fatalf("image request did not preserve provider result: status=%d body=%q", response.StatusCode, got)
					}
					if status != 200 {
						return
					}
					before := f.forwarded.Load()
					for _, invalid := range []struct{ method, path, marker, account string }{
						{"GET", path, "public-selector", "public-account"},
						{"POST", path + "?extra=1", "public-selector", "public-account"},
						{"POST", strings.Replace(path, "/images/", "/%69mages/", 1), "public-selector", "public-account"},
						{"POST", path + "/extra", "public-selector", "public-account"},
						{"POST", path, "wrong-selector", "public-account"},
						{"POST", path, "", "public-account"},
						{"POST", path, "public-selector", "wrong-account"},
					} {
						if family.account == "" && invalid.account == "wrong-account" {
							continue
						}
						request := f.request(t, invalid.path)
						request.Method = invalid.method
						request.Header.Set("Authorization", "Bearer "+invalid.marker)
						request.Header.Del("ChatGPT-Account-ID")
						if family.account != "" {
							request.Header.Set("ChatGPT-Account-ID", invalid.account)
						}
						response, _ := nativeResponse(t, f.client, request)
						if response.StatusCode != 403 {
							t.Fatalf("nonexact image request admitted: %+v", invalid)
						}
					}
					if f.forwarded.Load() != before {
						t.Fatal("refused image request reached provider")
					}
				})
			}
		}
	}
}
