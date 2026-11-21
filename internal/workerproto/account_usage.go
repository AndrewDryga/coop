package workerproto

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
)

// AccountUsageVersion is the version of the account usage object, separate from the poll's
// protocol version for the same reason as StorageVersion.
const AccountUsageVersion = 1

const (
	AccountSignedIn    = "signed_in"
	AccountNotSignedIn = "not_signed_in"
	AccountSignInAgain = "sign_in_again"

	// AccountUsageSignInRefused says the provider refused the account's sign-in while its usage
	// was read; AccountUsageUnavailable says the read failed for any other reason. Neither carries
	// the provider's own words: they are not this worker's to forward.
	AccountUsageSignInRefused = "sign_in_refused"
	AccountUsageUnavailable   = "unavailable"

	maxUsageAccounts = 32
	maxUsageLimits   = 32
	maxUsageExpiries = 32
	maxUsageText     = 256
)

var (
	accountSignIns    = []string{AccountSignedIn, AccountNotSignedIn, AccountSignInAgain}
	accountUsageCodes = []string{AccountUsageSignInRefused, AccountUsageUnavailable}
	usageProvider     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// AccountUsage is what this worker's providers last reported about the allowance left on each
// account it holds: the windows `coop usage` shows (a 5-hour window, a weekly one, a model's
// own, credits), each with how much of it is used and when it resets. It rides on the hello so a
// control plane can show the allowance beside the models that spend it without holding any
// credential: the worker reads the providers with its own sign-ins, in the background.
//
// Every field is always present, and unknown is null, never zero: a window whose use the
// provider did not report is not an unused one.
type AccountUsage struct {
	Version  int                   `json:"version"`
	Accounts []AccountUsageAccount `json:"accounts"`
}

// AccountUsageAccount is one account, as provider@name.
//
// CheckedAt is when its provider last answered and Limits, Plan, Auth, Note and ResetCredits come
// from that answer; null when no read has succeeded since the worker started. AttemptedAt is the
// latest read and Error its failure, null when it succeeded: a failed read keeps the last answer,
// so a control plane can show figures that are going stale as such. An account that is not
// signed in is never read.
type AccountUsageAccount struct {
	Account      string               `json:"account"`
	SignIn       string               `json:"sign_in"`
	CheckedAt    *time.Time           `json:"checked_at"`
	AttemptedAt  *time.Time           `json:"attempted_at"`
	Error        *string              `json:"error"`
	Plan         *string              `json:"plan"`
	Auth         *string              `json:"auth"`
	Note         *string              `json:"note"`
	Limits       []AccountUsageLimit  `json:"limits"`
	ResetCredits *AccountResetCredits `json:"reset_credits"`
}

// AccountUsageLimit is one window the provider reported. UsedPercent is the share of it USED, 0
// to 100 as the provider said it, which can exceed 100; Remaining is the provider's own amount
// (a credit balance, "unlimited"), never derived; ResetsAt is when the window resets; Available
// is the provider's own word on whether the window can be spent now.
type AccountUsageLimit struct {
	Name        string     `json:"name"`
	UsedPercent *float64   `json:"used_percent"`
	Remaining   *string    `json:"remaining"`
	ResetsAt    *time.Time `json:"resets_at"`
	Available   *bool      `json:"available"`
	Note        *string    `json:"note"`
}

// AccountResetCredits are the resets of a window an account may still spend, and when each of
// them expires.
type AccountResetCredits struct {
	Available *int64      `json:"available"`
	ExpiresAt []time.Time `json:"expires_at"`
}

// Validate confirms one account usage report before a worker publishes it or a controller
// trusts it.
func (u AccountUsage) Validate() error { return u.validate() }

func (u AccountUsage) validate() error {
	if u.Version != AccountUsageVersion {
		return fmt.Errorf("unsupported worker account usage version %d", u.Version)
	}
	if u.Accounts == nil || len(u.Accounts) > maxUsageAccounts {
		return fmt.Errorf("worker account usage needs at most %d accounts", maxUsageAccounts)
	}
	seen := map[string]bool{}
	for _, account := range u.Accounts {
		if seen[account.Account] {
			return errors.New("duplicate account in worker account usage")
		}
		seen[account.Account] = true
		if err := account.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (a AccountUsageAccount) validate() error {
	if !usageAccount(a.Account) {
		return errors.New("worker account usage names an invalid account")
	}
	if !slices.Contains(accountSignIns, a.SignIn) {
		return errors.New("worker account usage has an invalid sign-in state")
	}
	if a.Error != nil && !slices.Contains(accountUsageCodes, *a.Error) {
		return errors.New("worker account usage has an invalid error")
	}
	if a.CheckedAt != nil && a.CheckedAt.IsZero() || a.AttemptedAt != nil && a.AttemptedAt.IsZero() {
		return errors.New("worker account usage has an empty time")
	}
	// A read that succeeded is the answer it checked; one that failed keeps an older answer, or none.
	switch {
	case a.AttemptedAt == nil && (a.CheckedAt != nil || a.Error != nil):
		return errors.New("worker account usage reports a read it never attempted")
	case a.AttemptedAt != nil && a.Error == nil && (a.CheckedAt == nil || !a.CheckedAt.Equal(*a.AttemptedAt)):
		return errors.New("worker account usage reports a successful read it did not check")
	case a.CheckedAt != nil && a.CheckedAt.After(*a.AttemptedAt):
		return errors.New("worker account usage was checked after its latest attempt")
	}
	for _, text := range []*string{a.Plan, a.Auth, a.Note} {
		if !usageText(text) {
			return errors.New("worker account usage text is invalid")
		}
	}
	if a.Limits == nil || len(a.Limits) > maxUsageLimits {
		return fmt.Errorf("worker account usage needs at most %d limits", maxUsageLimits)
	}
	if a.CheckedAt == nil && (len(a.Limits) > 0 || a.Plan != nil || a.Auth != nil || a.Note != nil || a.ResetCredits != nil) {
		return errors.New("worker account usage reports figures it never read")
	}
	for _, limit := range a.Limits {
		if err := limit.validate(); err != nil {
			return err
		}
	}
	if credits := a.ResetCredits; credits != nil {
		if credits.Available != nil && *credits.Available < 0 {
			return errors.New("worker account usage has negative reset credits")
		}
		if credits.ExpiresAt == nil || len(credits.ExpiresAt) > maxUsageExpiries {
			return fmt.Errorf("worker account usage needs at most %d reset credit expiries", maxUsageExpiries)
		}
		for _, expiry := range credits.ExpiresAt {
			if expiry.IsZero() {
				return errors.New("worker account usage has an empty reset credit expiry")
			}
		}
	}
	return nil
}

func (l AccountUsageLimit) validate() error {
	name := l.Name
	if !usageText(&name) || name == "" {
		return errors.New("worker account usage limit needs a name")
	}
	if used := l.UsedPercent; used != nil && (math.IsNaN(*used) || math.IsInf(*used, 0) || *used < 0 || *used > 1000) {
		return errors.New("worker account usage limit has an invalid use")
	}
	if !usageText(l.Remaining) || !usageText(l.Note) {
		return errors.New("worker account usage limit text is invalid")
	}
	if l.ResetsAt != nil && l.ResetsAt.IsZero() {
		return errors.New("worker account usage limit has an empty reset time")
	}
	return nil
}

// usageAccount is provider@name with Coop's own account rule (agent.ParseTarget): one path-safe
// segment, never ".", ".." or a leading "-".
func usageAccount(ref string) bool {
	provider, name, found := strings.Cut(ref, "@")
	if !found || !usageProvider.MatchString(provider) || name == "" || len(name) > 128 {
		return false
	}
	if strings.ContainsAny(name, ":@/\\") || name == "." || name == ".." || strings.HasPrefix(name, "-") {
		return false
	}
	return usageText(&name)
}

// usageText bounds a provider string the way a reference is bounded, but allows the spaces and
// punctuation a window's name ("5-hour", "Fable 5 · Weekly") or a plan carries.
func usageText(text *string) bool {
	if text == nil {
		return true
	}
	if len(*text) > maxUsageText {
		return false
	}
	for _, r := range *text {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
