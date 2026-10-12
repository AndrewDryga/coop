package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestUsageCodexResetCredits(t *testing.T) {
	for _, tc := range []struct {
		name, usage, details string
		status               int
		count                *int64
		expiries, requests   int
	}{
		{"absent", `{}`, `{}`, 200, nil, 0, 2},
		{"null", `{"rate_limit_reset_credits":null}`, `{"available_count":null}`, 200, nil, 0, 2},
		{"zero", `{"rate_limit_reset_credits":{"available_count":0}}`, ``, 200, resetCount(0), 0, 1},
		{"count only", `{"rate_limit_reset_credits":{"available_count":3}}`, `{}`, 200, resetCount(3), 0, 2},
		{"capped details", `{"rate_limit_reset_credits":{"available_count":3}}`, `{"available_count":4,"credits":[{"reset_type":"codex_rate_limits","status":"available","expires_at":"2026-10-18T06:39:00Z"}]}`, 200, resetCount(4), 1, 2},
		{"unknown and spent expiry", `{}`, `{"available_count":2,"credits":[{"reset_type":"codex_rate_limits","status":"available","expires_at":null},{"reset_type":"codex_rate_limits","status":"redeemed","expires_at":"2026-10-18T06:39:00Z"},{"reset_type":"future_reset","status":"available","expires_at":"2026-10-18T06:39:00Z"},{"reset_type":"codex_rate_limits","status":"available","expires_at":"invalid"}]}`, 200, resetCount(2), 0, 2},
		{"negative count", `{"rate_limit_reset_credits":{"available_count":-1}}`, `{"available_count":-1}`, 200, nil, 0, 2},
		{"details zero", `{}`, `{"available_count":0,"credits":[]}`, 200, resetCount(0), 0, 2},
		{"detail failure preserves count", `{"rate_limit_reset_credits":{"available_count":3}}`, `private-provider-body`, 403, resetCount(3), 0, 2},
		{"detail format preserves count", `{"rate_limit_reset_credits":{"available_count":3}}`, `private-provider-body`, 200, resetCount(3), 0, 2},
		{"redirect refused", `{"rate_limit_reset_credits":{"available_count":3}}`, `private-provider-body`, 302, resetCount(3), 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			requests := 0
			http.DefaultTransport = nativeQuotaTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Method != http.MethodGet || r.URL.Host != "chatgpt.com" || r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("Chatgpt-Account-Id") != "fixture-account" || r.Header.Get("x-openai-codex-luna-reserve") != "" {
					t.Fatal("reset inspection changed method, authority or reserve admission")
				}
				body, status := tc.usage, 200
				if requests == 1 {
					if r.URL.Path != "/backend-api/wham/usage" {
						t.Fatal("unexpected quota route")
					}
				} else if requests == 2 && r.URL.Path == "/backend-api/wham/rate-limit-reset-credits" {
					body, status = tc.details, tc.status
				} else {
					t.Fatal("inspection followed redirect or contacted a redemption route")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://must-not-follow.example"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			quota, err := codexUsageQuota(ctx, UsageQuotaInput{Current: func(context.Context, time.Time) (NativeCredentialState, map[string][]byte, error) {
				return NativeCredentialState{AccessToken: "fixture-token", AccountID: "fixture-account"}, nil, nil
			}})
			if err != nil || requests != tc.requests || quota.Resets == nil {
				t.Fatalf("quota=%+v requests=%d err=%v", quota, requests, err)
			}
			resets := quota.Resets
			if (resets.Available == nil) != (tc.count == nil) || tc.count != nil && *resets.Available != *tc.count || len(resets.Expirations) != tc.expiries {
				t.Fatalf("resets=%+v want count=%v expiries=%d", resets, tc.count, tc.expiries)
			}
		})
	}
}

func TestUsageCodexResetDetailsCancellationPreservesQuota(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	requests := 0
	http.DefaultTransport = nativeQuotaTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"rate_limit_reset_credits":{"available_count":2}}`))}, nil
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("details lookup has no bounded deadline")
		}
		cancel()
		return nil, errors.New("private-provider-error")
	})
	bounded, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	quota, err := codexUsageQuota(bounded, UsageQuotaInput{Current: func(context.Context, time.Time) (NativeCredentialState, map[string][]byte, error) {
		return NativeCredentialState{AccessToken: "fixture-token", AccountID: "fixture-account"}, nil, nil
	}})
	if err != nil || requests != 2 || quota.Resets == nil || quota.Resets.Available == nil || *quota.Resets.Available != 2 {
		t.Fatalf("optional cancelled detail discarded quota: %+v %v", quota, err)
	}
}

func resetCount(n int64) *int64 { return &n }
