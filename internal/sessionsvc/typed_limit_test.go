package sessionsvc

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
)

// Recorded from Responder admission b8bff9f3-731d-42b6-b011-87c309d9caea
// on 2026-09-09. Every retry spent another attempt on the exhausted account,
// although the policy already named a usable fallback account.
const recordedSubscriptionLimit = "You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Sep 15th, 2026 4:32 PM."

func TestSubscriptionExhaustionRotatesWithoutARetryAction(t *testing.T) {
	fixture := newSessionACPFixture(t, "typed-provider-subscription-limit-once")
	fixture.signIn(t, "codex", "backup")
	t.Setenv("COOP_TEST_SESSION_LIMIT_MARKER", filepath.Join(t.TempDir(), "limited"))
	leased := fixture.submit(t, "investigate")
	ctx := ladderContext(t, contextWithTurnDeadline(t), "codex@work", "codex@backup")
	result, err := fixture.runner.Run(ctx, fixture.session, leased)
	if result.State != session.TurnCompleted || result.AssistantMessage != "rotated answer" {
		t.Fatalf("subscription exhaustion did not complete on the fallback: result=%+v, err=%v", result, err)
	}
	if err != nil {
		t.Fatal(err)
	}
	bound, err := fixture.store.GetSession(context.Background(), fixture.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Target != "codex@backup" {
		t.Fatalf("session target = %q, want the configured backup", bound.Target)
	}
	methods := readSessionACPLog(t, fixture.childLog)
	if countStrings(methods, "session/new") != 2 || countStrings(methods, "session/load") != 0 ||
		countStrings(methods, "session/prompt") != 2 {
		t.Fatalf("credential rotation did not start exactly one fresh native session: %v", methods)
	}
}

func TestTypedLimitClassificationPreservesTheProviderBoundary(t *testing.T) {
	cases := []struct {
		name, category, severity, title string
		actions                         []string
		want                            session.ErrorCode
	}{
		{"subscription without retry", "limit", "error", recordedSubscriptionLimit, nil, sessionACPRateLimited},
		{"explicit retry", "limit", "error", "capacity temporarily unavailable", []string{"retry"}, sessionACPRateLimited},
		{"output limit", "limit", "error", "maximum output tokens reached", nil, sessionACPProtocolError},
		{"output limit with retry", "limit", "error", "maximum output tokens reached", []string{"retry"}, sessionACPProtocolError},
		{"unspecified limit", "limit", "error", "request limit exceeded", nil, sessionACPProtocolError},
		{"access with quota words", "access", "error", recordedSubscriptionLimit, nil, sessionACPProtocolError},
		{"service with quota words", "service", "error", recordedSubscriptionLimit, nil, sessionACPProtocolError},
		{"advisory with quota words", "limit", "warning", recordedSubscriptionLimit, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := sessionACPTerminalFailureError(sessionACPAirVersion, &sessionACPTerminalFailure{
				ID: "turn:error", Revision: 1, Category: tc.category, Severity: tc.severity,
				Title: tc.title, Actions: tc.actions,
			})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("warning became terminal: %v", err)
				}
				return
			}
			var failure *sessionACPFailure
			if !errors.As(err, &failure) || failure.code != tc.want {
				t.Fatalf("typed failure = %v, want %s", err, tc.want)
			}
			if !strings.Contains(failure.detail, tc.title) {
				t.Fatalf("provider diagnosis was lost: %s", failure.detail)
			}
		})
	}
}

func TestTypedQuotaFailureRetainsItsResetTime(t *testing.T) {
	err := sessionACPTerminalFailureError(sessionACPAirVersion, &sessionACPTerminalFailure{
		ID: "turn:error", Revision: 1, Category: "limit", Severity: "error",
		Title: "Claude AI usage limit reached|2000000000",
	})
	var failure *sessionACPFailure
	if !errors.As(err, &failure) || failure.code != sessionACPRateLimited ||
		!failure.resetAt.Equal(time.Unix(2_000_000_000, 0)) {
		t.Fatalf("typed quota failure lost its provider reset: %+v, err=%v", failure, err)
	}
}
