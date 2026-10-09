package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/nativeauth"
)

type nativeQuotaTransport func(*http.Request) (*http.Response, error)

func (f nativeQuotaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNativeQuotaUsesCanonicalAuthority(t *testing.T) {
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			ag, _ := Get(name)
			files := nativeauth.Files(t, name, "work")
			if name == "claude" {
				files[".credentials.json"] = []byte(strings.Replace(string(files[".credentials.json"]), "account:read", "user:profile", 1))
			}
			state, err := ag.NativeCredentials().Inspect(files, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			requests := 0
			original := http.DefaultTransport
			http.DefaultTransport = nativeQuotaTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Header.Get("Authorization") != "Bearer "+state.AccessToken {
					t.Fatal("quota did not use the selected canonical grant")
				}
				body := `{}`
				switch name {
				case "claude":
					if r.URL.String() != "https://api.anthropic.com/api/oauth/usage" || r.Method != http.MethodGet {
						t.Fatal("wrong Claude quota route")
					}
				case "codex":
					if r.URL.String() != "https://chatgpt.com/backend-api/wham/usage" || r.Header.Get("Chatgpt-Account-Id") != state.AccountID {
						t.Fatal("wrong Codex quota authority")
					}
				case "gemini":
					if r.URL.Host != "cloudcode-pa.googleapis.com" || r.Method != http.MethodPost {
						t.Fatal("wrong Gemini quota route")
					}
					if requests == 1 && r.URL.Path == "/v1internal:loadCodeAssist" {
						body = `{"cloudaicompanionProject":"quota-project","currentTier":{"name":"standard"}}`
					} else if requests != 2 || r.URL.Path != "/v1internal:retrieveUserQuota" {
						t.Fatal("unexpected Gemini quota call")
					}
				case "grok":
					if r.URL.String() != "https://cli-chat-proxy.grok.com/v1/billing?format=credits" || r.Header.Get("X-Userid") != state.AccountID {
						t.Fatal("wrong Grok quota authority")
					}
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = original })
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			currentCalls := 0
			input := UsageQuotaInput{ProfileDir: t.TempDir(), Current: func(ctx context.Context, deadline time.Time) (NativeCredentialState, map[string][]byte, error) {
				currentCalls++
				if ctx.Err() != nil || !deadline.After(time.Now()) {
					t.Fatal("quota renewal lost its deadline")
				}
				return state, files, nil
			}}
			if _, err := ag.Usage().Quota(ctx, input); err != nil || currentCalls != 1 || requests == 0 {
				t.Fatalf("canonical quota = %v; authority calls %d, requests %d", err, currentCalls, requests)
			}
			before := requests
			input.Current = func(context.Context, time.Time) (NativeCredentialState, map[string][]byte, error) {
				return NativeCredentialState{}, nil, errors.New("revoked")
			}
			if _, err := ag.Usage().Quota(ctx, input); err == nil || requests != before {
				t.Fatal("failed canonical authority fell back or reached quota endpoint")
			}
		})
	}
}
