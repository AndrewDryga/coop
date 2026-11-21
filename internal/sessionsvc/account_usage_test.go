package sessionsvc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func accountUsageTestService(t *testing.T) *Service {
	t.Helper()
	fixture := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), storageTestRepo(t), nil)
	t.Cleanup(func() { _ = fixture.Stop() })
	return fixture.Service
}

// A controller shows each account's allowance beside the models that spend it, so the worker reads
// its providers with its own sign-ins. A read asks the provider over the network and can renew a
// token, so a heartbeat never costs one: the answer is kept for the interval. A read that fails
// keeps the last answer, marked by when it was checked, and an account that is not signed in is
// never read at all.
func TestAccountUsageIsReadOncePerIntervalAndKeepsItsLastAnswer(t *testing.T) {
	service := accountUsageTestService(t)
	accounts := []usageAccount{
		{provider: "codex", name: "personal", signIn: agents.SignedIn},
		{provider: "claude", name: "work", signIn: agents.NotSignedIn},
	}
	service.testUsageAccounts = func() []usageAccount { return accounts }
	used, weekly := 82.0, 40.0
	reset := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	available, credits := false, int64(2)
	var reads atomic.Int32
	answer := func(context.Context, *config.Config, string, string) (agents.UsageQuota, error) {
		return agents.UsageQuota{
			Plan: "pro",
			Buckets: []agents.UsageBucket{
				{Name: "5-hour", Used: &used, Reset: reset, Available: &available},
				{Name: "Weekly", Used: &weekly, Reset: reset.Add(96 * time.Hour)},
				{Name: "Credits", Remaining: "unlimited"},
			},
			Resets: &agents.UsageResetCredits{Available: &credits, Expirations: []time.Time{reset.Add(720 * time.Hour)}},
		}, nil
	}
	service.testAccountQuota = func(ctx context.Context, cfg *config.Config, provider, account string) (agents.UsageQuota, error) {
		reads.Add(1)
		if provider != "codex" || account != "personal" {
			t.Errorf("read %s@%s, which is not signed in", provider, account)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("a provider read had no deadline")
		}
		return answer(ctx, cfg, provider, account)
	}

	report := service.AccountUsageReport(context.Background())
	if err := report.Usage.Validate(); err != nil {
		t.Fatalf("published usage is invalid: %v\n%+v", err, report.Usage)
	}
	if len(report.Usage.Accounts) != 2 {
		t.Fatalf("accounts = %+v", report.Usage.Accounts)
	}
	work, personal := report.Usage.Accounts[0], report.Usage.Accounts[1]
	if work.Account != "claude@work" || work.SignIn != workerproto.AccountNotSignedIn || work.AttemptedAt != nil || len(work.Limits) != 0 {
		t.Fatalf("an account that is not signed in = %+v", work)
	}
	if personal.Account != "codex@personal" || personal.CheckedAt == nil || personal.Error != nil || *personal.Plan != "pro" {
		t.Fatalf("a read account = %+v", personal)
	}
	if len(personal.Limits) != 3 || *personal.Limits[0].UsedPercent != 82 || !personal.Limits[0].ResetsAt.Equal(reset) ||
		*personal.Limits[0].Available || personal.Limits[2].UsedPercent != nil || *personal.Limits[2].Remaining != "unlimited" {
		t.Fatalf("limits = %+v", personal.Limits)
	}
	if personal.ResetCredits == nil || *personal.ResetCredits.Available != 2 || len(personal.ResetCredits.ExpiresAt) != 1 {
		t.Fatalf("reset credits = %+v", personal.ResetCredits)
	}

	service.AccountUsageReport(context.Background())
	if reads.Load() != 1 {
		t.Fatalf("a heartbeat inside the interval read the providers again: %d reads", reads.Load())
	}

	// The next read fails: the figures and their check time stay, and the report says why.
	checked := *personal.CheckedAt
	expireAccountUsage(service)
	answer = func(context.Context, *config.Config, string, string) (agents.UsageQuota, error) {
		return agents.UsageQuota{}, errors.New("dial tcp: i/o timeout")
	}
	failed := service.AccountUsageReport(context.Background()).Usage
	if err := failed.Validate(); err != nil {
		t.Fatalf("usage after a failed read is invalid: %v", err)
	}
	stale := failed.Accounts[1]
	if stale.Error == nil || *stale.Error != workerproto.AccountUsageUnavailable || !stale.CheckedAt.Equal(checked) ||
		stale.AttemptedAt.Before(checked) || len(stale.Limits) != 3 {
		t.Fatalf("a failed read = %+v", stale)
	}

	// The provider refuses the sign-in: the account says so, and still carries its last figures.
	expireAccountUsage(service)
	answer = func(context.Context, *config.Config, string, string) (agents.UsageQuota, error) {
		return agents.UsageQuota{}, &agents.SignInError{Need: agents.SignInAgain, Target: "codex@personal"}
	}
	refused := service.AccountUsageReport(context.Background()).Usage.Accounts[1]
	if *refused.Error != workerproto.AccountUsageSignInRefused || refused.SignIn != workerproto.AccountSignInAgain || len(refused.Limits) != 3 {
		t.Fatalf("a refused sign-in = %+v", refused)
	}

	// An account removed from the worker leaves the report.
	expireAccountUsage(service)
	accounts = accounts[:1]
	if after := service.AccountUsageReport(context.Background()).Usage.Accounts; len(after) != 1 || after[0].Account != "codex@personal" {
		t.Fatalf("accounts after one was removed = %+v", after)
	}
}

// The connector reads the report on the owner-private socket and forwards only `usage`.
func TestSessionHTTPUsageReportsEachAccount(t *testing.T) {
	service := accountUsageTestService(t)
	service.testUsageAccounts = func() []usageAccount {
		return []usageAccount{{provider: "codex", name: "default", signIn: agents.SignInAgain}}
	}

	response := sessionHTTPTestRequest(t, NewHTTPHandler(service), http.MethodGet, "/v1/usage", "", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /v1/usage = %d: %s", response.Code, response.Body.String())
	}
	var body AccountUsageReport
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if err := body.Usage.Validate(); err != nil {
		t.Fatalf("published usage is invalid: %v\n%s", err, response.Body.String())
	}
	if len(body.Usage.Accounts) != 1 || body.Usage.Accounts[0].SignIn != workerproto.AccountSignInAgain {
		t.Fatalf("usage = %s", response.Body.String())
	}
	if post := sessionHTTPTestRequest(t, NewHTTPHandler(service), http.MethodPost, "/v1/usage", "{}", "", "application/json"); post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/usage = %d", post.Code)
	}
}

func expireAccountUsage(service *Service) {
	service.accountUsage.mu.Lock()
	service.accountUsage.readAt = time.Now().Add(-accountUsageInterval)
	service.accountUsage.mu.Unlock()
}
