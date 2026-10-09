package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type geminiRenewRoundTrip func(*http.Request) (*http.Response, error)

func (f geminiRenewRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func geminiTestAuthority() []byte {
	return []byte(`{"access_token":"inert-old-access","refresh_token":"inert-old-refresh","expiry_date":1,"token_type":"Bearer","scope":"inert-original-scope","unknown_native":{"preserved":true}}`)
}

func geminiTestClient(t *testing.T, respond geminiRenewRoundTrip) {
	t.Helper()
	original := geminiRenewClient
	clone := *original
	clone.Transport = respond
	geminiRenewClient = &clone
	t.Cleanup(func() { geminiRenewClient = original })
}

func geminiTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestGeminiAuthorityRenewalPreservesRotationBeforeDeadlineError(t *testing.T) {
	for _, rotated := range []bool{false, true} {
		t.Run(map[bool]string{false: "omitted-refresh", true: "rotated-refresh"}[rotated], func(t *testing.T) {
			requests := 0
			issued := `{"access_token":"inert-new-access","expires_in":60}`
			if rotated {
				issued = `{"access_token":"inert-new-access","refresh_token":"inert-rotated-refresh","expires_in":60}`
			}
			geminiTestClient(t, func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Method != http.MethodPost || r.URL.String() != geminiOAuthTokenURL ||
					r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
					t.Fatal("native OAuth request contract changed")
				}
				raw, _ := io.ReadAll(r.Body)
				fields, err := url.ParseQuery(string(raw))
				if err != nil || fields.Get("client_id") != geminiOAuthClientID || fields.Get("client_secret") != geminiOAuthClientSecret ||
					fields.Get("grant_type") != "refresh_token" || fields.Get("refresh_token") != "inert-old-refresh" {
					t.Fatal("native refresh authority contract changed")
				}
				return geminiTestResponse(200, issued), nil
			})
			received := ""
			result, err := renewGeminiAuthority(context.Background(), geminiTestAuthority(), time.Now().Add(time.Hour), func(data []byte) error {
				received = string(data)
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "does not cover") || received != issued || len(result.Credential) == 0 || requests != 1 {
				t.Fatalf("short rotation lost: calls=%d retained=%v credential=%v err=%v", requests, received == issued, len(result.Credential) > 0, err)
			}
			var doc map[string]json.RawMessage
			if json.Unmarshal(result.Credential, &doc) != nil {
				t.Fatal("invalid returned authority")
			}
			var token, scope, kind string
			_ = json.Unmarshal(doc["refresh_token"], &token)
			_ = json.Unmarshal(doc["scope"], &scope)
			_ = json.Unmarshal(doc["token_type"], &kind)
			expected := "inert-old-refresh"
			if rotated {
				expected = "inert-rotated-refresh"
			}
			if token != expected || scope != "inert-original-scope" || kind != "Bearer" || string(doc["unknown_native"]) != `{"preserved":true}` {
				t.Fatal("lost native rotated or omitted fields")
			}
		})
	}
}

func TestGeminiAuthorityRenewalRetainsUnusableIssuedResponse(t *testing.T) {
	for _, body := range []string{
		`{"refresh_token":"inert-rotated-refresh","expires_in":3600}`,
		`{"access_token":"inert-new-access","refresh_token":"inert-rotated-refresh","expires_in":0}`,
		`{"access_token":"inert-new-access","refresh_token":"inert-rotated-refresh","expires_in":-1}`,
		`{"access_token":"inert-new-access","refresh_token":"inert-rotated-refresh","expires_in":1e100}`,
		`{"access_token":"inert-new-access","refresh_token":"inert-rotated-refresh","expires_in":3600,"token_type":"Other"}`,
		`{"access_token":"inert-new-access","refresh_token":"inert-rotated-refresh","expires_in":3600,"scope":null}`,
		"not-json",
	} {
		t.Run(body, func(t *testing.T) {
			geminiTestClient(t, func(*http.Request) (*http.Response, error) { return geminiTestResponse(200, body), nil })
			retained := ""
			result, err := renewGeminiAuthority(context.Background(), geminiTestAuthority(), time.Now().Add(time.Hour), func(data []byte) error {
				retained = string(data)
				return nil
			})
			if err == nil || retained != body || string(result.Received) != body || len(result.Credential) != 0 {
				t.Fatal("unusable issued response lost or admitted as serving authority")
			}
		})
	}
}

func TestGeminiAuthorityRenewalCancellationAfterIssuanceKeepsGrant(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	geminiTestClient(t, func(*http.Request) (*http.Response, error) {
		return geminiTestResponse(200, `{"access_token":"inert-new-access","refresh_token":"inert-rotated-refresh","expires_in":3600}`), nil
	})
	result, err := renewGeminiAuthority(ctx, geminiTestAuthority(), time.Now().Add(time.Minute), func([]byte) error { cancel(); return nil })
	if err != nil || len(result.Credential) == 0 || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("issued rotation discarded after cancel: %v", err)
	}
}

func TestGeminiAuthorityRenewalRefusesCustodyFailureAndRedirect(t *testing.T) {
	t.Run("custody", func(t *testing.T) {
		geminiTestClient(t, func(*http.Request) (*http.Response, error) {
			return geminiTestResponse(200, `{"access_token":"inert-new-access","expires_in":3600}`), nil
		})
		result, err := renewGeminiAuthority(context.Background(), geminiTestAuthority(), time.Now().Add(time.Minute), func([]byte) error { return errors.New("inert denied write") })
		if err == nil || !strings.Contains(err.Error(), "custody unconfirmed") || len(result.Received) == 0 || len(result.Credential) != 0 {
			t.Fatal("uncertain issued authority was silently retried or admitted")
		}
	})
	t.Run("redirect", func(t *testing.T) {
		calls := 0
		geminiTestClient(t, func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.String() != geminiOAuthTokenURL {
				t.Fatal("grant followed upstream redirect")
			}
			response := geminiTestResponse(302, `{"error":"inert redirect"}`)
			response.Header.Set("Location", "https://other.example.invalid/token")
			return response, nil
		})
		result, err := renewGeminiAuthority(context.Background(), geminiTestAuthority(), time.Now().Add(time.Minute), func([]byte) error { return nil })
		if err == nil || calls != 1 || len(result.Credential) != 0 {
			t.Fatal("redirect admitted")
		}
	})
}

func TestGeminiAuthorityRenewalCurrentGrantDoesNotRefresh(t *testing.T) {
	var doc map[string]any
	_ = json.Unmarshal(geminiTestAuthority(), &doc)
	doc["expiry_date"] = time.Now().Add(time.Hour).UnixMilli()
	raw, _ := json.Marshal(doc)
	geminiTestClient(t, func(*http.Request) (*http.Response, error) { t.Fatal("unnecessary native refresh"); return nil, nil })
	result, err := renewGeminiAuthority(context.Background(), raw, time.Now().Add(time.Minute), nil)
	if err != nil || string(result.Credential) != string(raw) || len(result.Received) != 0 {
		t.Fatal("current authority changed")
	}
}

type geminiBrokenBody struct{ data []byte }

func (b *geminiBrokenBody) Read(p []byte) (int, error) {
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, io.ErrUnexpectedEOF
}

func TestGeminiAuthorityRenewalRetainsReceivedBytesOnReadFailure(t *testing.T) {
	issued := `{"access_token":"inert-issued-access","refresh_token":"inert-rotated-refresh","expires_in":3600}`
	geminiTestClient(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(&geminiBrokenBody{data: []byte(issued)})}, nil
	})
	retained := ""
	result, err := renewGeminiAuthority(context.Background(), geminiTestAuthority(), time.Now().Add(time.Minute), func(data []byte) error {
		retained = string(data)
		return nil
	})
	if err == nil || retained != issued || string(result.Received) != issued || len(result.Credential) != 0 {
		t.Fatal("uncertain received rotation was lost or became serving authority")
	}
}

func TestGeminiAuthorityRenewalBoundsUncertainResponseCustody(t *testing.T) {
	for _, size := range []int{0, (1 << 20) + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			geminiTestClient(t, func(*http.Request) (*http.Response, error) {
				return geminiTestResponse(http.StatusOK, strings.Repeat("x", size)), nil
			})
			var retained []byte
			calls := 0
			result, err := renewGeminiAuthority(context.Background(), geminiTestAuthority(), time.Now().Add(time.Minute), func(data []byte) error {
				calls++
				retained = append([]byte(nil), data...)
				return nil
			})
			want := min(size, 1<<20)
			if err == nil || len(result.Credential) != 0 || len(result.Received) != want || len(retained) != want {
				t.Fatal("invalid response exceeded recovery bound or became serving authority")
			}
			if (size == 0 && calls != 0) || (size != 0 && calls != 1) {
				t.Fatalf("unexpected custody calls: %d", calls)
			}
		})
	}
}
