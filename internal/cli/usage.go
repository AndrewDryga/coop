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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
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
	details := target.Account() != ""
	renderUsage(os.Stdout, ui.For(os.Stdout), ui.TermWidth(os.Stdout), time.Now(), names, rows, details)
	var footer []string
	if details && len(retained.Turns) > 0 {
		footer = append(footer, "Retained Coop turn aggregates are unpriced and are not added to native history.")
		if unattributed > 0 {
			footer = append(footer, fmt.Sprintf("%d installation-wide retained turns have no exact historical credential binding.", unattributed))
		}
	}
	if details && retained.Truncated {
		footer = append(footer, "Retained Coop turn scan is partial (read limit reached).")
	}
	if details && retainedErr != nil && !errors.Is(retainedErr, os.ErrNotExist) {
		footer = append(footer, "Retained Coop turn coverage unavailable.")
	}
	if len(footer) > 0 {
		fmt.Println()
		fmt.Println(strings.Join(footer, "\n"))
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

func renderUsage(w io.Writer, pal ui.Palette, width int, now time.Time, names []string, rows []usageCredential, details bool) {
	line := func(prefix, value string) {
		if width <= 0 {
			fmt.Fprintln(w, prefix+value)
			return
		}
		for _, text := range ui.PrefixedLines(prefix, value, width) {
			fmt.Fprintln(w, text)
		}
	}
	// One label gutter for every fact in the view, quota buckets and fixed labels alike, so bars,
	// balances, reasons and labeled estimates all start in the same column.
	labelWidth := 14
	fixed := []string{"Limits"}
	if details {
		fixed = append(fixed, "30-day API estimate", "Credential", "Coop records")
	}
	for _, label := range fixed {
		labelWidth = max(labelWidth, utf8.RuneCountInString(label))
	}
	for _, row := range rows {
		if row.shared || !slices.Contains(names, row.provider) {
			continue
		}
		for _, bucket := range row.quota.Buckets {
			if details || bucket.Note != "disabled" {
				labelWidth = max(labelWidth, utf8.RuneCountInString(agents.DisplayTarget(bucket.Name)))
			}
		}
	}
	fact := func(label, value string) {
		prefix := "    " + padRight(label, labelWidth) + "  "
		// Like a quota row, a fact too wide for the gutter stacks under its label.
		if width > 0 && utf8.RuneCountInString(prefix+value) > width {
			if label != "" {
				line("    ", label)
			}
			line("      ", value)
			return
		}
		line(prefix, value)
	}
	estimate := func(row usageCredential) {
		value := usageValueLabel(row, details)
		if details {
			fact("30-day API estimate", value)
			return
		}
		if strings.HasPrefix(value, "≈$") {
			value = "Σ" + value
		} else {
			value = "Σ " + value
		}
		// Match the percentage's ones column, including its fixed-width padding.
		indent := labelWidth + 20
		if width > 0 && indent+utf8.RuneCountInString(value) > width {
			indent = 4
		}
		// The total is quieter than the limits above it. Lay it out as plain text, then dim each
		// line, so the escape codes never count toward the wrapping width.
		texts := []string{strings.Repeat(" ", indent) + value}
		if width > 0 {
			texts = ui.PrefixedLines(strings.Repeat(" ", indent), value, width)
		}
		for _, text := range texts {
			fmt.Fprintln(w, pal.Dim(text))
		}
	}
	// A blank line separates blocks; nothing trails the last one.
	wrote := false
	for _, name := range names {
		if wrote {
			fmt.Fprintln(w)
		}
		wrote = true
		fmt.Fprintln(w, pal.Bold(titleName(name)))
		count := 0
		for _, row := range rows {
			if row.provider != name || row.shared && !row.value.Available {
				continue
			}
			if count > 0 {
				fmt.Fprintln(w)
			}
			count++
			header := agents.DisplayTarget(row.account)
			if row.shared {
				header = "Unassigned editor usage"
			}
			if details && row.quota.Plan != "" {
				header += " · " + agents.DisplayTarget(row.quota.Plan)
			}
			// A note about limits that are shown qualifies the whole account, so it rides the
			// account line; without limits, the note is the account's one reason line.
			note := agents.DisplayTarget(row.quota.Note)
			annotate := note != "" && row.quotaErr == nil && len(row.quota.Buckets) > 0
			if annotate && (width <= 0 || 2+utf8.RuneCountInString(header+" · "+note) <= width) {
				fmt.Fprintln(w, "  "+header+pal.Dim(" · "+note))
				note = ""
			} else {
				line("  ", header)
			}
			if row.shared {
				estimate(row)
				if details {
					fact("Credential", "unknown · excluded from credential totals")
				}
				continue
			}
			if row.quotaErr != nil {
				fact("Limits", "unavailable · "+agents.DisplayTarget(row.quotaErr.Error()))
				if errors.Is(row.quotaErr, agents.ErrUsageSignIn) {
					fact("", "Run: "+agents.LoginCommand(name+"@"+row.account))
				}
			} else if len(row.quota.Buckets) == 0 && note == "" {
				fact("Limits", "unavailable")
			}
			shownResets := make(map[int64]bool)
			for _, bucket := range row.quota.Buckets {
				if !details && bucket.Note == "disabled" {
					continue
				}
				label := agents.DisplayTarget(bucket.Name)
				text := ""
				bar := ""
				if bucket.Used != nil {
					text = fmt.Sprintf("%3.0f%% used", *bucket.Used)
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
					remaining := bucket.Remaining
					if !details {
						if number, err := strconv.ParseFloat(remaining, 64); err == nil && !math.IsNaN(number) && !math.IsInf(number, 0) {
							remaining = strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", number), "0"), ".")
						}
					}
					if text != "" {
						text += " · "
					}
					text += agents.DisplayTarget(remaining) + " remaining"
				}
				if text == "" {
					text = "usage unknown"
				}
				if bucket.Available != nil && !*bucket.Available {
					text += " · blocked"
				}
				if bucket.Note != "" {
					text += " · " + agents.DisplayTarget(bucket.Note)
				}
				if details || !bucket.Reset.IsZero() && !(bucket.Used != nil && *bucket.Used == 0 && shownResets[bucket.Reset.Unix()]) {
					text += " · " + usageResetLabel(bucket.Reset, now)
					shownResets[bucket.Reset.Unix()] = true
				}
				columns := labelWidth + utf8.RuneCountInString(text) + 6
				if bar != "" {
					columns += 12
				}
				if width > 0 && columns > width {
					line("    ", label)
					if bar != "" {
						fmt.Fprintf(w, "      %s\n", bar)
					}
					line("      ", text)
				} else {
					if bar != "" {
						bar += "  "
					}
					fmt.Fprintf(w, "    %s  %s%s\n", padRight(label, labelWidth), bar, text)
				}
			}
			if note != "" {
				r, size := utf8.DecodeRuneInString(note)
				line("    ", string(unicode.ToUpper(r))+note[size:])
			}
			estimate(row)
			if details && row.unpricedTurns > 0 {
				fact("Coop records", fmt.Sprintf("%d retained turns · unpriced aggregates, not added", row.unpricedTurns))
			}
		}
		if count == 0 {
			line("  ", "No credentials. Sign in: coop login "+name)
		}
	}
}

func usageValueLabel(row usageCredential, details bool) string {
	value := "unavailable"
	if details {
		value += " · no usable history"
	}
	if row.unpricedTurns > 0 {
		value = "unpriced"
		if details {
			value += " · retained Coop turn aggregates only"
		}
	}
	if row.ambiguous {
		value = "unavailable · historical credential attribution is ambiguous"
	}
	if row.value.Available {
		if row.value.Priced == 0 && row.value.Unpriced > 0 {
			value = "unpriced"
		} else if row.value.Priced == 0 && row.value.Partial {
			value = "unavailable"
			if details {
				value += " · no usable usage in the partial 30-day history"
			}
		} else {
			value = fmt.Sprintf("≈$%.2f", row.value.USD)
		}
		if details {
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
	}
	return value
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
	format := "Jan 2, 15:04"
	if reset.In(time.Local).Year() != now.In(time.Local).Year() {
		format = "Jan 2 2006, 15:04"
	}
	return "resets " + reset.In(time.Local).Format(format)
}
