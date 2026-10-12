package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestUsageClaudeResetGrants(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		count     *int64
		expiries  int
	}{
		{"absent", ``, nil, 0},
		{"null", `null`, nil, 0},
		{"fieldless", `{}`, nil, 0},
		{"ineligible", `{"eligible":false,"grants":[]}`, nil, 0},
		{"missing eligibility", `{"grants":[]}`, nil, 0},
		{"missing grants", `{"eligible":true}`, nil, 0},
		{"null grants", `{"eligible":true,"grants":null}`, nil, 0},
		{"zero", `{"eligible":true,"grants":[]}`, resetCount(0), 0},
		{"remaining not grant count", `{"eligible":true,"grants":[{"id":"first","resets_left":3,"ends_at":"2026-10-20T06:00:00Z","usable_now":false},{"id":"spent","resets_left":0,"ends_at":"2026-10-19T06:00:00Z"},{"id":"second","resets_left":2,"ends_at":null}]}`, resetCount(5), 1},
		{"missing count", `{"eligible":true,"grants":[{"id":"first"}]}`, nil, 0},
		{"null count", `{"eligible":true,"grants":[{"id":"first","resets_left":null}]}`, nil, 0},
		{"negative count", `{"eligible":true,"grants":[{"id":"first","resets_left":-1}]}`, nil, 0},
		{"fractional count", `{"eligible":true,"grants":[{"id":"first","resets_left":1.5}]}`, nil, 0},
		{"string count", `{"eligible":true,"grants":[{"id":"first","resets_left":"3"}]}`, nil, 0},
		{"sum overflow", `{"eligible":true,"grants":[{"id":"first","resets_left":9223372036854775807},{"id":"second","resets_left":1}]}`, nil, 0},
		{"malformed grant", `{"eligible":true,"grants":[{"id":"first","resets_left":3},null]}`, nil, 0},
		{"invalid grant id", `{"eligible":true,"grants":[{"id":"unexpected/route","resets_left":3}]}`, nil, 0},
		{"missing grant id", `{"eligible":true,"grants":[{"resets_left":3}]}`, nil, 0},
		{"bad expiry preserves count", `{"eligible":true,"grants":[{"id":"first","resets_left":3,"ends_at":"bad"},{"id":"second","resets_left":2,"ends_at":42}]}`, resetCount(5), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeUsageResets(json.RawMessage(tc.raw))
			if tc.count == nil {
				if got != nil {
					t.Fatalf("unknown reset count became known: %+v", got)
				}
				return
			}
			if got == nil || got.Available == nil || *got.Available != *tc.count || len(got.Expirations) != tc.expiries {
				t.Fatalf("reset grants = %+v; want count=%d expiries=%d", got, *tc.count, tc.expiries)
			}
			if tc.expiries > 0 && !got.Expirations[0].Equal(time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC)) {
				t.Fatal("reset expiry was not preserved")
			}
		})
	}
}

func TestUsageClaudeOptionalResetsPreserveQuota(t *testing.T) {
	for _, tc := range []struct {
		name, details string
		status        int
		cancel        bool
		count         *int64
	}{
		{"available", `{"cedar_ember":{"eligible":true,"grants":[{"id":"first","resets_left":3}]}}`, 200, false, resetCount(3)},
		{"zero", `{"cedar_ember":{"eligible":true,"grants":[]}}`, 200, false, resetCount(0)},
		{"absent", `{}`, 200, false, nil},
		{"null", `{"cedar_ember":null}`, 200, false, nil},
		{"malformed block", `{"cedar_ember":{"eligible":true,"grants":[{"id":"first","resets_left":"private"}]}}`, 200, false, nil},
		{"malformed response", `private-provider-body`, 200, false, nil},
		{"forbidden", `private-provider-body`, 403, false, nil},
		{"unavailable", `private-provider-body`, 503, false, nil},
		{"redirect refused", `private-provider-body`, 302, false, nil},
		{"cancellation", ``, 200, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			requests, currentCalls := 0, 0
			http.DefaultTransport = nativeQuotaTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Method != http.MethodGet || r.URL.Host != "api.anthropic.com" || r.URL.Path != "/api/oauth/usage" || r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" {
					t.Fatal("reset inspection changed authority or reached redemption")
				}
				status, body := 200, `{"five_hour":{"utilization":14},"extra_usage":{"is_enabled":true,"utilization":20}}`
				switch requests {
				case 1:
					if r.URL.RawQuery != "" {
						t.Fatal("normal quota request changed")
					}
				case 2:
					if r.URL.RawQuery != "cedar_ember=1&skip_spend=1" {
						t.Fatal("wrong reset metadata query")
					}
					deadline, ok := r.Context().Deadline()
					if !ok || time.Until(deadline) > 5*time.Second {
						t.Fatal("optional reset lookup lost its deadline")
					}
					if tc.cancel {
						cancel()
						return nil, errors.New("private-provider-error")
					}
					body, status = tc.details, tc.status
				default:
					t.Fatal("inspection followed redirect or retried redemption")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://must-not-follow.example"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			quota, err := claudeUsageQuota(ctx, UsageQuotaInput{Current: func(context.Context, time.Time) (NativeCredentialState, map[string][]byte, error) {
				currentCalls++
				return NativeCredentialState{}, map[string][]byte{".credentials.json": []byte(`{"claudeAiOauth":{"accessToken":"fixture-token","scopes":["user:profile"],"subscriptionType":"max"}}`)}, nil
			}})
			if err != nil || requests != 2 || currentCalls != 1 || quota.Plan != "max" || len(quota.Buckets) != 2 || *quota.Buckets[0].Used != 14 || *quota.Buckets[1].Used != 20 {
				t.Fatalf("optional resets discarded quota: %+v %v; requests=%d current=%d", quota, err, requests, currentCalls)
			}
			if tc.count == nil {
				if quota.Resets != nil {
					t.Fatal("missing reset metadata became a known count")
				}
			} else if quota.Resets == nil || quota.Resets.Available == nil || *quota.Resets.Available != *tc.count {
				t.Fatalf("reset count not mapped: %+v", quota.Resets)
			}
		})
	}
}
