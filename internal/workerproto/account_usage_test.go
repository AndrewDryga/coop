package workerproto

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// The exact shape a control plane decodes: every key present, unknown as null, never as zero.
const accountUsageWire = `{"version":1,"accounts":[` +
	`{"account":"claude@work","sign_in":"not_signed_in","checked_at":null,"attempted_at":null,` +
	`"error":null,"plan":null,"auth":null,"note":null,"limits":[],"reset_credits":null},` +
	`{"account":"codex@personal","sign_in":"signed_in","checked_at":"2026-11-21T01:00:00Z",` +
	`"attempted_at":"2026-11-21T01:05:00Z","error":"unavailable","plan":"pro","auth":null,"note":null,` +
	`"limits":[{"name":"5-hour","used_percent":82,"remaining":null,"resets_at":"2026-11-21T03:00:00Z",` +
	`"available":false,"note":null},{"name":"Credits","used_percent":null,"remaining":"unlimited",` +
	`"resets_at":null,"available":null,"note":null}],` +
	`"reset_credits":{"available":2,"expires_at":["2026-12-21T03:00:00Z"]}}]}`

func sampleAccountUsage(t *testing.T) AccountUsage {
	t.Helper()
	var usage AccountUsage
	if err := json.Unmarshal([]byte(accountUsageWire), &usage); err != nil {
		t.Fatal(err)
	}
	return usage
}

func TestAccountUsageMarshalsTheExactPublishedShape(t *testing.T) {
	usage := sampleAccountUsage(t)
	if err := usage.Validate(); err != nil {
		t.Fatalf("the published sample is invalid: %v", err)
	}
	encoded, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != accountUsageWire {
		t.Fatalf("published shape changed:\n got %s\nwant %s", encoded, accountUsageWire)
	}
}

// A worker that has not read its providers publishes no key at all, and one that has sends the
// report beside the rest of an unchanged poll; an incoherent report is refused like bad storage.
func TestWorkerHelloCarriesAccountUsageWithoutChangingTheRestOfThePoll(t *testing.T) {
	poll := samplePoll()
	without, err := json.Marshal(poll)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), "account_usage") {
		t.Fatalf("a worker without a report published the key: %s", without)
	}

	usage := sampleAccountUsage(t)
	poll.Worker.AccountUsage = &usage
	encoded, err := json.Marshal(poll)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePoll(encoded)
	if err != nil {
		t.Fatalf("a poll carrying account usage was rejected: %v", err)
	}
	if got := decoded.Worker.AccountUsage; got == nil || len(got.Accounts) != 2 || *got.Accounts[1].Limits[0].UsedPercent != 82 {
		t.Fatalf("decoded account usage = %+v", got)
	}

	poll.Worker.AccountUsage.Accounts[0].SignIn = "maybe"
	broken, err := json.Marshal(poll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePoll(broken); err == nil {
		t.Fatal("a poll carrying incoherent account usage was accepted")
	}
}

func TestAccountUsageValidateRefusesWhatItCannotHaveRead(t *testing.T) {
	at := time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC)
	later := at.Add(time.Minute)
	code := AccountUsageUnavailable
	unknown := "maybe"
	nan := math.NaN()
	long := strings.Repeat("x", 257)
	control := "5-hour\nweekly"
	for name, change := range map[string]func(*AccountUsage){
		"unknown version":    func(u *AccountUsage) { u.Version = 2 },
		"no account list":    func(u *AccountUsage) { u.Accounts = nil },
		"duplicate account":  func(u *AccountUsage) { u.Accounts[0].Account = u.Accounts[1].Account },
		"account with slash": func(u *AccountUsage) { u.Accounts[0].Account = "codex@a/b" },
		"no provider":        func(u *AccountUsage) { u.Accounts[0].Account = "@work" },
		"unknown error":      func(u *AccountUsage) { u.Accounts[1].Error = &unknown },
		"checked unattempted": func(u *AccountUsage) {
			u.Accounts[0].CheckedAt = &at
		},
		"error unattempted": func(u *AccountUsage) { u.Accounts[0].Error = &code },
		"success unchecked": func(u *AccountUsage) {
			u.Accounts[1].Error = nil
			u.Accounts[1].CheckedAt = &at
			u.Accounts[1].AttemptedAt = &later
		},
		"checked after attempt": func(u *AccountUsage) { u.Accounts[1].CheckedAt = &later; u.Accounts[1].AttemptedAt = &at },
		"figures never read": func(u *AccountUsage) {
			u.Accounts[0].Limits = []AccountUsageLimit{{Name: "5-hour"}}
		},
		"no limit list":     func(u *AccountUsage) { u.Accounts[0].Limits = nil },
		"unnamed limit":     func(u *AccountUsage) { u.Accounts[1].Limits[0].Name = "" },
		"not a number":      func(u *AccountUsage) { u.Accounts[1].Limits[0].UsedPercent = &nan },
		"control character": func(u *AccountUsage) { u.Accounts[1].Limits[0].Name = control },
		"overlong plan":     func(u *AccountUsage) { u.Accounts[1].Plan = &long },
		"no expiry list":    func(u *AccountUsage) { u.Accounts[1].ResetCredits.ExpiresAt = nil },
		"too many accounts": func(u *AccountUsage) {
			for len(u.Accounts) <= maxUsageAccounts {
				extra := u.Accounts[0]
				extra.Account = "codex@extra-" + strings.Repeat("a", len(u.Accounts))
				u.Accounts = append(u.Accounts, extra)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			usage := sampleAccountUsage(t)
			change(&usage)
			if err := usage.Validate(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	// A provider's own words for a window, with spaces and a middle dot, are fine; so is a use
	// beyond the whole window, which providers report.
	usage := sampleAccountUsage(t)
	name, over := "Fable 5 · Weekly", 104.5
	usage.Accounts[1].Limits[0].Name, usage.Accounts[1].Limits[0].UsedPercent = name, &over
	if err := usage.Validate(); err != nil {
		t.Fatalf("a provider's window name was refused: %v", err)
	}
}
