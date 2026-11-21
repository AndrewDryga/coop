package sessionsvc

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

const (
	// accountUsageInterval is how old the last read may be before a report reads the providers
	// again. A read asks every signed-in account's provider over the network, and can renew its
	// token, so a heartbeat must not cost one; the reset times stay right at this grain.
	accountUsageInterval = 5 * time.Minute
	// accountUsageLookup bounds one account's read, as `coop usage` bounds its own.
	accountUsageLookup = 20 * time.Second
	accountUsageReads  = 4
	accountUsageText   = 256
	accountUsageLimits = 32
)

// AccountUsageReport is GET /v1/usage: what each account's provider last reported about the
// allowance left on it. The envelope can grow local detail; the connector forwards only `usage`.
type AccountUsageReport struct {
	Usage workerproto.AccountUsage `json:"usage"`
}

// accountUsageState is the last read of each account, kept so a failed read can still show the
// answer before it, marked by its own check time.
type accountUsageState struct {
	readMu   sync.Mutex // one pass at a time
	mu       sync.Mutex
	accounts map[string]workerproto.AccountUsageAccount
	readAt   time.Time
}

type accountQuotaReader func(ctx context.Context, cfg *config.Config, provider, account string) (agents.UsageQuota, error)

type usageAccountLister func() []usageAccount

// AccountUsageReport returns every account this worker holds with its last provider answer,
// reading the providers again only when the last pass is older than accountUsageInterval.
func (s *Service) AccountUsageReport(ctx context.Context) AccountUsageReport {
	if report, ok := s.cachedAccountUsage(); ok {
		return report
	}
	s.accountUsage.readMu.Lock()
	defer s.accountUsage.readMu.Unlock()
	if report, ok := s.cachedAccountUsage(); ok {
		return report
	}
	s.readAccountUsage(ctx)
	report, _ := s.cachedAccountUsage()
	return report
}

func (s *Service) cachedAccountUsage() (AccountUsageReport, bool) {
	s.accountUsage.mu.Lock()
	defer s.accountUsage.mu.Unlock()
	report := AccountUsageReport{Usage: workerproto.AccountUsage{
		Version: workerproto.AccountUsageVersion, Accounts: []workerproto.AccountUsageAccount{},
	}}
	for _, account := range s.accountUsage.accounts {
		report.Usage.Accounts = append(report.Usage.Accounts, account)
	}
	slices.SortFunc(report.Usage.Accounts, func(a, b workerproto.AccountUsageAccount) int {
		return strings.Compare(a.Account, b.Account)
	})
	fresh := !s.accountUsage.readAt.IsZero() && time.Since(s.accountUsage.readAt) < accountUsageInterval
	return report, fresh
}

// usageAccount is one account the worker lists, with its sign-in as the credentials listing
// shows it.
type usageAccount struct {
	provider, name string
	signIn         agents.SignIn
}

// usageAccounts lists every account of every provider, the way `coop credentials` does.
func (s *Service) usageAccounts() []usageAccount {
	if s.testUsageAccounts != nil {
		return s.testUsageAccounts()
	}
	var accounts []usageAccount
	for _, provider := range agents.Names() {
		names, err := box.EffectiveProfiles(s.sourceCfg, provider)
		if err != nil {
			continue
		}
		for _, name := range names {
			accounts = append(accounts, usageAccount{provider, name, box.ProfileSignIn(s.sourceCfg, provider, name, time.Now())})
		}
	}
	return accounts
}

// readAccountUsage reads every account the worker lists and replaces the state with them: an
// account removed since the last pass goes, a failed read keeps its last answer. A pass the caller
// abandons keeps the previous one, and the next report tries again.
func (s *Service) readAccountUsage(ctx context.Context) {
	read := s.testAccountQuota
	if read == nil {
		read = box.AccountQuota
	}
	s.accountUsage.mu.Lock()
	previous := s.accountUsage.accounts
	s.accountUsage.mu.Unlock()
	next := usagePass(ctx, previous, s.usageAccounts(), func(ctx context.Context, provider, name string) (agents.UsageQuota, error) {
		return read(ctx, s.sourceCfg, provider, name)
	})
	if ctx.Err() != nil {
		return
	}
	s.accountUsage.mu.Lock()
	s.accountUsage.accounts = next
	s.accountUsage.readAt = time.Now()
	s.accountUsage.mu.Unlock()
}

// usagePass reads each signed-in account a few at a time, each within accountUsageLookup, and
// carries an account that is not signed in over unread.
func usagePass(
	ctx context.Context,
	previous map[string]workerproto.AccountUsageAccount,
	accounts []usageAccount,
	read func(ctx context.Context, provider, name string) (agents.UsageQuota, error),
) map[string]workerproto.AccountUsageAccount {
	if len(accounts) > accountUsageLimits {
		accounts = accounts[:accountUsageLimits]
	}
	next := make(map[string]workerproto.AccountUsageAccount, len(accounts))
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, accountUsageReads)
	for _, account := range accounts {
		ref := account.provider + "@" + account.name
		entry := previous[ref]
		entry.Account = ref
		entry.SignIn = signInWire(account.signIn)
		if entry.Limits == nil {
			entry.Limits = []workerproto.AccountUsageLimit{}
		}
		if account.signIn != agents.SignedIn {
			mu.Lock()
			next[ref] = entry
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			lookup, cancel := context.WithTimeout(ctx, accountUsageLookup)
			defer cancel()
			quota, err := read(lookup, account.provider, account.name)
			answered := usageAnswer(entry, quota, err, time.Now().UTC().Truncate(time.Second))
			mu.Lock()
			next[ref] = answered
			mu.Unlock()
		}()
	}
	wg.Wait()
	return next
}

// usageAnswer records one read: a success replaces the account's figures and checks them now, a
// failure keeps the figures and the time they were checked and says why the read failed.
func usageAnswer(entry workerproto.AccountUsageAccount, quota agents.UsageQuota, err error, at time.Time) workerproto.AccountUsageAccount {
	entry.AttemptedAt = &at
	if err != nil {
		code := workerproto.AccountUsageUnavailable
		var signIn *agents.SignInError
		if errors.As(err, &signIn) {
			code = workerproto.AccountUsageSignInRefused
			entry.SignIn = signInWire(signIn.Need)
		}
		entry.Error = &code
		return entry
	}
	entry.Error = nil
	entry.CheckedAt = &at
	entry.Plan = usageString(quota.Plan)
	entry.Auth = usageString(quota.Auth)
	entry.Note = usageString(quota.Note)
	entry.Limits = []workerproto.AccountUsageLimit{}
	for _, bucket := range quota.Buckets {
		if len(entry.Limits) == accountUsageLimits {
			break
		}
		name := usageString(bucket.Name)
		if name == nil {
			continue
		}
		limit := workerproto.AccountUsageLimit{
			Name: *name, UsedPercent: bucket.Used, Remaining: usageString(bucket.Remaining),
			Available: bucket.Available, Note: usageString(bucket.Note),
		}
		if !bucket.Reset.IsZero() {
			reset := bucket.Reset.UTC()
			limit.ResetsAt = &reset
		}
		entry.Limits = append(entry.Limits, limit)
	}
	entry.ResetCredits = nil
	if resets := quota.Resets; resets != nil {
		credits := workerproto.AccountResetCredits{Available: resets.Available, ExpiresAt: []time.Time{}}
		for _, expiry := range resets.Expirations {
			if !expiry.IsZero() && len(credits.ExpiresAt) < accountUsageLimits {
				credits.ExpiresAt = append(credits.ExpiresAt, expiry.UTC())
			}
		}
		entry.ResetCredits = &credits
	}
	return entry
}

func signInWire(state agents.SignIn) string {
	switch state {
	case agents.SignedIn:
		return workerproto.AccountSignedIn
	case agents.NotSignedIn:
		return workerproto.AccountNotSignedIn
	default:
		return workerproto.AccountSignInAgain
	}
}

// usageString is a provider's text as the report may carry it: nil when empty, without control
// characters, and cut at accountUsageText bytes on a character boundary.
func usageString(text string) *string {
	text = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.TrimSpace(text))
	for len(text) > accountUsageText {
		_, size := utf8.DecodeLastRuneInString(text)
		text = text[:len(text)-size]
	}
	if text == "" {
		return nil
	}
	return &text
}
