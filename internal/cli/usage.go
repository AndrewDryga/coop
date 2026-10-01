package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/ui"
)

const usageSyntax = "coop usage [<provider>[@credential]]"

type usageCredential struct {
	provider, account string
	paths             []string
	history           agents.UsageHistory
	value             agents.UsageValue
	quota             agents.UsageQuota
	quotaErr          error
	unpricedTurns     int
	shared            bool
	ambiguous         bool
}

func usageSelector(args []string) (agents.Target, error) {
	if len(args) > 1 {
		return agents.Target{}, ui.UnexpectedArgument(args[1], "coop usage", usageSyntax)
	}
	if len(args) == 0 {
		return agents.Target{}, nil
	}
	if strings.HasPrefix(args[0], "-") {
		return agents.Target{}, unknownOptionErr(args[0], "coop usage", nil)
	}
	target, err := agents.ParseTarget(args[0])
	if err != nil {
		return target, targetUsage(err, "coop usage", usageSyntax)
	}
	if target.Model != "" || target.Effort != "" || len(target.Accounts) > 1 {
		return target, &ui.UsageError{Headline: "Usage takes a provider and at most one credential", Rows: [][2]string{{"Usage:", usageSyntax}}}
	}
	return target, nil
}

func (a *app) cmdUsage(args []string) (int, error) {
	target, err := usageSelector(args)
	if err != nil {
		return 2, err
	}
	names := agents.Names()
	if target.Provider != "" {
		names = []string{target.Provider}
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	state, _ := defaultSessionStateRoot()
	now := time.Now()
	retained, retainedErr := session.ReadUsageSnapshot(ctx, state, now.Add(-30*24*time.Hour), now)
	rows, partial := a.usageCredentials(names, state)
	unattributed := 0
	for _, turn := range retained.Turns {
		t, err := agents.ParseTarget(turn.Target)
		if err != nil || len(t.Accounts) != 1 {
			unattributed++
			continue
		}
		if !slices.Contains(names, t.Provider) || target.Account() != "" && t.Account() != target.Account() {
			continue
		}
		index := slices.IndexFunc(rows, func(row usageCredential) bool {
			return !row.shared && row.provider == t.Provider && row.account == t.Account()
		})
		if index < 0 {
			rows = append(rows, usageCredential{provider: t.Provider, account: t.Account()})
			index = len(rows) - 1
		}
		rows[index].unpricedTurns++
	}
	a.sortUsageCredentials(rows)
	if target.Account() != "" {
		found := false
		for _, row := range rows {
			found = found || !row.shared && row.account == target.Account()
		}
		if !found {
			return 2, noAccountErr(target.Provider, target.Account())
		}
	}
	// Host reads do not initialize a runtime. The one native helper does so only when requested.
	var runtimeMu sync.Mutex
	var wg sync.WaitGroup
	workers := make(chan struct{}, 4)
	for i := range rows {
		row := &rows[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case workers <- struct{}{}:
				defer func() { <-workers }()
			case <-ctx.Done():
				row.quotaErr = errors.New("usage lookup timed out")
				return
			}
			ag, _ := agents.Get(row.provider)
			for _, path := range row.paths {
				history := agents.ReadUsageHistory(ctx, path, ag.Usage())
				row.history.Events = append(row.history.Events, history.Events...)
				row.history.Available = row.history.Available || history.Available
				row.history.Partial = row.history.Partial || history.Partial || partial
			}
			if row.shared || target.Account() != "" && row.account != target.Account() {
				return
			}
			if !box.ProfileAuthed(a.cfg, row.provider, row.account) {
				row.quotaErr = agents.ErrUsageSignIn
				return
			}
			input, err := box.UsageQuotaAuthority(a.cfg, row.provider, row.account)
			if err != nil {
				row.quotaErr = errors.New("selected authentication authority is unavailable")
				return
			}
			input.Native = func(nativeCtx context.Context, command []string) ([]byte, error) {
				runtimeMu.Lock()
				err := a.ensureRuntimeContext(nativeCtx)
				rt := a.rt
				runtimeMu.Unlock()
				if err != nil {
					return nil, errors.New("native quota helper runtime unavailable")
				}
				return box.ReadUsageQuotaNative(nativeCtx, a.cfg, rt, ag, row.account, command)
			}
			row.quota, row.quotaErr = ag.Usage().Quota(ctx, input)
		}()
	}
	wg.Wait()
	rows = deduplicateUsageCredentials(rows)
	for i := range rows {
		ag, _ := agents.Get(rows[i].provider)
		rows[i].value = agents.ValueUsageHistory(rows[i].history, now, ag.Usage().Price)
	}
	if target.Account() != "" {
		rows = slices.DeleteFunc(rows, func(row usageCredential) bool { return row.shared || row.account != target.Account() })
	}
	renderUsage(os.Stdout, ui.For(os.Stdout), ui.TermWidth(os.Stdout), time.Now(), names, rows, a.cfg.DefaultProfileOf)
	if len(retained.Turns) > 0 {
		fmt.Println("Retained Coop turn aggregates are unpriced and are not added to native history.")
		if unattributed > 0 {
			fmt.Printf("%d installation-wide retained turns have no exact historical credential binding.\n", unattributed)
		}
	}
	if retained.Truncated {
		fmt.Println("Retained Coop turn scan is partial (read limit reached).")
	}
	if retainedErr != nil && !errors.Is(retainedErr, os.ErrNotExist) {
		fmt.Println("Retained Coop turn coverage unavailable.")
	}
	// Inspection succeeds when it can show any usable quota or native history. Individual errors
	// stay inline; a total lookup failure is nonzero, not a fabricated empty successful report.
	for _, row := range rows {
		if row.value.Available || row.unpricedTurns > 0 || row.quotaErr == nil && len(row.quota.Buckets) > 0 {
			return 0, nil
		}
	}
	return 1, nil
}

func (a *app) usageCredentials(names []string, state string) ([]usageCredential, bool) {
	var rows []usageCredential
	partial := false
	for _, name := range names {
		for _, account := range box.EffectiveProfiles(a.cfg, name) {
			rows = append(rows, usageCredential{provider: name, account: account, paths: []string{a.cfg.AgentProfileDir(name, account)}})
		}
	}
	// Private session paths freeze the selected account; shared editor paths do not.
	sessions, err := usageDirs(filepath.Join(state, "acp"), 2000)
	partial = err != nil && !errors.Is(err, os.ErrNotExist)
	for _, id := range sessions {
		for _, name := range names {
			base := filepath.Join(state, "acp", id, name, "profiles")
			accounts, err := usageDirs(base, 100)
			partial = partial || err != nil && !errors.Is(err, os.ErrNotExist)
			for _, account := range accounts {
				index := slices.IndexFunc(rows, func(row usageCredential) bool { return row.provider == name && row.account == account })
				if index < 0 {
					rows = append(rows, usageCredential{provider: name, account: account})
					index = len(rows) - 1
				}
				rows[index].paths = append(rows[index].paths, filepath.Join(base, account))
			}
		}
	}
	for _, name := range names {
		rows = append(rows, usageCredential{provider: name, account: "Unattributed ACP", shared: true, paths: []string{filepath.Join(a.cfg.ConfigDir, name, "acp-sessions")}})
	}
	a.sortUsageCredentials(rows)
	return rows, partial
}

func (a *app) sortUsageCredentials(rows []usageCredential) {
	slices.SortStableFunc(rows, func(x, y usageCredential) int {
		if x.provider != y.provider {
			return strings.Compare(x.provider, y.provider)
		}
		if x.shared != y.shared {
			if x.shared {
				return 1
			}
			return -1
		}
		def := a.cfg.DefaultProfileOf(x.provider)
		if (x.account == def) != (y.account == def) {
			if x.account == def {
				return -1
			}
			return 1
		}
		return strings.Compare(x.account, y.account)
	})
}

func usageDirs(path string, limit int) ([]string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("history directory is unsafe")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries, err := file.ReadDir(limit + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > limit {
		return nil, errors.New("history directory exceeds its read limit")
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

func deduplicateUsageCredentials(rows []usageCredential) []usageCredential {
	owners := map[string]int{}
	shared := map[string]int{}
	for i, row := range rows {
		if row.shared {
			shared[row.provider] = i
		}
	}
	for i := range rows {
		if rows[i].shared {
			continue
		}
		for _, event := range rows[i].history.Events {
			key := rows[i].provider + ":" + event.ID
			prior, ok := owners[key]
			if !ok {
				owners[key] = i
			} else if prior != i {
				owners[key] = -1
			}
		}
	}
	for i := range rows {
		kept := rows[i].history.Events[:0]
		for _, event := range rows[i].history.Events {
			owner, known := owners[rows[i].provider+":"+event.ID]
			if rows[i].shared && known && owner >= 0 {
				continue
			}
			if !rows[i].shared && known && owner < 0 {
				if j, ok := shared[rows[i].provider]; ok {
					rows[j].history.Events = append(rows[j].history.Events, event)
					rows[j].history.Available = true
					rows[j].history.Partial = true
				}
				rows[i].history.Partial = true
				rows[i].ambiguous = true
				continue
			}
			kept = append(kept, event)
		}
		rows[i].history.Events = kept
		if rows[i].ambiguous && len(kept) == 0 {
			rows[i].history.Available = false
		}
	}
	return rows
}

func renderUsage(w io.Writer, pal ui.Palette, width int, now time.Time, names []string, rows []usageCredential, defaultOf func(string) string) {
	line := func(prefix, value string) {
		if width <= 0 {
			fmt.Fprintln(w, prefix+value)
			return
		}
		for _, text := range ui.PrefixedLines(prefix, value, width) {
			fmt.Fprintln(w, text)
		}
	}
	for _, name := range names {
		fmt.Fprintln(w, pal.Bold(titleName(name)))
		count := 0
		for _, row := range rows {
			if row.provider != name || row.shared && !row.value.Available {
				continue
			}
			count++
			header := agents.DisplayTarget(row.account)
			if row.quota.Plan != "" {
				header += " · " + agents.DisplayTarget(row.quota.Plan)
			}
			if !row.shared && row.account == defaultOf(name) {
				header += " · default"
			}
			line("  ", header)
			value := "unavailable · no usable history"
			if row.unpricedTurns > 0 {
				value = "unpriced · retained Coop turn aggregates only"
			}
			if row.ambiguous {
				value = "unavailable · historical credential attribution is ambiguous"
			}
			if row.value.Available {
				if row.value.Priced == 0 && row.value.Unpriced > 0 {
					value = "unpriced"
				} else if row.value.Priced == 0 && row.value.Partial {
					value = "unavailable · no usable usage in the partial 30-day history"
				} else {
					value = fmt.Sprintf("≈$%.2f", row.value.USD)
				}
				if row.value.Partial {
					value += " · partial history"
				}
				if row.value.Approximate {
					value += " · approximate token tariff"
				}
				if row.value.Unpriced > 0 {
					value += fmt.Sprintf(" · %d unpriced events", row.value.Unpriced)
				}
			}
			line("    API value, 30d   ", value)
			if row.unpricedTurns > 0 {
				line("    Coop records    ", fmt.Sprintf("%d retained turns · unpriced aggregates, not added", row.unpricedTurns))
			}
			if row.shared {
				line("    Credential      ", "unknown · excluded from credential totals")
				fmt.Fprintln(w)
				continue
			}
			if row.quotaErr != nil {
				line("    Limits          ", "unavailable · "+agents.DisplayTarget(row.quotaErr.Error()))
				if errors.Is(row.quotaErr, agents.ErrUsageSignIn) {
					line("    Run: ", agents.LoginCommand(name+"@"+row.account))
				}
			} else if len(row.quota.Buckets) == 0 {
				fmt.Fprintln(w, "    Limits          unavailable")
			}
			labelWidth := 14
			for _, b := range row.quota.Buckets {
				labelWidth = max(labelWidth, utf8.RuneCountInString(agents.DisplayTarget(b.Name)))
			}
			for _, bucket := range row.quota.Buckets {
				label := agents.DisplayTarget(bucket.Name)
				fact := "usage unknown"
				bar := ""
				if bucket.Used != nil {
					fact = fmt.Sprintf("%.0f%% used", *bucket.Used)
					filled := int(math.Round(min(100, *bucket.Used) / 10))
					bar = strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)
					if *bucket.Used >= 100 {
						bar = pal.Red(bar)
					} else if *bucket.Used >= 80 {
						bar = pal.Yellow(bar)
					} else {
						bar = pal.Cyan(bar)
					}
				}
				if bucket.Remaining != "" {
					fact += " · " + agents.DisplayTarget(bucket.Remaining) + " remaining"
				}
				if bucket.Available != nil && !*bucket.Available {
					fact += " · blocked"
				}
				if bucket.Note != "" {
					fact += " · " + agents.DisplayTarget(bucket.Note)
				}
				reset := usageResetLabel(bucket.Reset, now)
				if width > 0 && labelWidth+utf8.RuneCountInString(fact)+utf8.RuneCountInString(reset)+25 > width {
					line("    ", label)
					if bar != "" {
						fmt.Fprintf(w, "      %s\n", bar)
					}
					line("      ", fact+" · "+reset)
				} else {
					fmt.Fprintf(w, "    %s  %s  %s · %s\n", padRight(label, labelWidth), bar, fact, reset)
				}
			}
			if row.quota.Note != "" {
				line("    ", agents.DisplayTarget(row.quota.Note))
			}
			fmt.Fprintln(w)
		}
		if count == 0 {
			line("  ", "No credentials. Sign in: coop login "+name)
			fmt.Fprintln(w)
		}
	}
	fmt.Fprintf(w, "Estimated API token value at current standard list prices (%s), not billing.\n", agents.UsagePricingDate)
	fmt.Fprintln(w, "Based on retained CLI/Coop history; excludes web/mobile, other machines and non-token charges.")
	fmt.Fprintln(w, "Reset times are local. Shared ACP history has no historical credential binding.")
}

func usageResetLabel(reset, now time.Time) string {
	if reset.IsZero() {
		return "reset unknown"
	}
	if reset.After(now) && reset.Sub(now) < 24*time.Hour {
		minutes := int(math.Ceil(reset.Sub(now).Minutes()))
		if minutes < 60 {
			return fmt.Sprintf("resets in %dm", minutes)
		}
		return fmt.Sprintf("resets in %dh %dm", minutes/60, minutes%60)
	}
	format := "Mon, Jan 2, 15:04 MST"
	if reset.In(time.Local).Year() != now.In(time.Local).Year() {
		format = "Mon, Jan 2 2006, 15:04 MST"
	}
	return "resets " + reset.In(time.Local).Format(format)
}
