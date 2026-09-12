package loop

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ladder"
)

func TestQuotaFallbackIsSanitizedAndWidthBounded(t *testing.T) {
	got := captureStderr(t, func() {
		printLoopCaution(strings.Repeat("claude:very-long-model@personal", 4) + "\x1b[31m reached its usage limit")
	})
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.Contains(line, "\x1b") || len([]rune(line)) > 79 {
			t.Fatalf("quota fallback retained controls or exceeded the static width: %q", got)
		}
	}
}

func TestAuthenticationDiagnosticsAreSanitizedAndWidthBounded(t *testing.T) {
	target := agents.Target{
		Provider: "claude",
		Model:    strings.Repeat("very-long-model-", 8),
		Accounts: []string{"legacy\x1b[31m\nprofile"},
	}
	headline := authHeadline(target)
	for i, line := range strings.Split(headline, "\n") {
		visibleWidth := len([]rune(line))
		if i == 0 {
			visibleWidth += 2 // Alert's warning mark and following space.
		}
		if strings.Contains(line, "\x1b") || visibleWidth > 80 {
			t.Fatalf("authentication headline retained controls or exceeded the static width: %q", headline)
		}
	}

	r := ladder.NewRotation([]agents.Target{
		target,
		{Provider: "codex", Model: strings.Repeat("another-long-model-", 8), Accounts: []string{"backup"}},
	})
	if !r.OnAuthFailure() || r.OnAuthFailure() {
		t.Fatal("test rotation did not retire both authentication targets")
	}
	errText := rotationAuthenticationError(r, r.Active()).Error()
	for _, line := range strings.Split(errText, "\n") {
		if strings.Contains(line, "\x1b") || len([]rune(line)) > 74 {
			t.Fatalf("authentication error retained controls or exceeded its indented width: %q", errText)
		}
	}
	if command := loginCommand(target); strings.Contains(command, "\x1b") || !strings.HasPrefix(command, "coop login $'") {
		t.Fatalf("unsafe profile login command was not shell-quoted safely: %q", command)
	}
}

func TestRememberPreflightLimitAdvancesWorkRotation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	reset := now.Add(time.Hour)
	r := rts("personal", "backup", "third")
	out := fmt.Sprintf("Claude AI usage limit reached|%d", reset.Unix())

	sleep, until, limited := rememberPreflightLimit(r, classifyIteration("claude", 1, nil, out, streamNotUsed, now), now)
	if !limited || sleep != 0 || !until.IsZero() || r.Active().String() != "claude@backup" {
		t.Fatalf("preflight limit: limited=%v sleep=%v until=%v active=%q, want true, 0, zero, backup", limited, sleep, until, r.Active())
	}
	if got := r.LimitedUntil("claude@personal"); !got.Equal(reset) {
		t.Errorf("personal limited until %v, want %v", got, reset)
	}

	// Successful prose and output exhaustion are not provider limits and must not rotate.
	for _, tc := range []struct {
		name string
		code int
		out  string
	}{
		{"successful prose", 0, out},
		{"output exhaustion", 1, "maximum output length reached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rts("personal", "backup", "third")
			classification := classifyIteration("claude", tc.code, nil, tc.out, streamNotUsed, now)
			if _, _, limited := rememberPreflightLimit(r, classification, now); limited || r.Active().String() != "claude@personal" {
				t.Errorf("limited=%v active=%q, want false + personal", limited, r.Active())
			}
		})
	}
}

// applyTarget lands the active target's effort in cfg, so EffortFor (and thus the agent command)
// sees a /effort rung. The other half of that glue — that expanding a ladder PRESERVES the effort
// onto each rung, and that the single-run path lands it too — stays with expandLadder and
// applyRunTarget in internal/cli's TestEffortThreadsToConfig.
func TestApplyTargetThreadsEffortToConfig(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	c := &Control{cfg: cfg}
	rot := ladder.NewRotation([]agents.Target{{Provider: "codex", Model: "gpt-5.6-sol", Effort: "high", Accounts: []string{"work"}}})
	if got := rot.Members(); !slices.Contains(got, "codex:gpt-5.6-sol/high@work") {
		t.Fatalf("rotation targets = %v, want one carrying /high", got)
	}
	c.applyTarget(rot)
	if got := cfg.EffortFor("codex"); got != "high" {
		t.Errorf("after applyTarget, EffortFor(codex) = %q, want high", got)
	}
}

// applyTarget points cfg at the target's account and model; a bare target clears the model tier.
func TestApplyTarget(t *testing.T) {
	c := &Control{cfg: &config.Config{ConfigDir: t.TempDir()}}
	r := ladder.NewRotation([]agents.Target{{Provider: "claude", Model: "sonnet", Accounts: []string{"work"}}, {Provider: "claude", Accounts: []string{"other"}}})
	c.applyTarget(r)
	if c.cfg.ActiveProfile("claude") != "work" || c.cfg.ModelFor("claude") != "sonnet" {
		t.Errorf("first target: account=%q model=%q, want work/sonnet", c.cfg.ActiveProfile("claude"), c.cfg.ModelFor("claude"))
	}
	r.OnLimit(time.Now().Add(time.Hour), 1, time.Now())
	c.applyTarget(r)
	if c.cfg.ModelFor("claude") != "" {
		t.Errorf("bare target: model = %q, want cleared", c.cfg.ModelFor("claude"))
	}
}
