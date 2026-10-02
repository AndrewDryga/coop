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
	// Quieter text is laid out as plain text first and dimmed after, so escape codes never count
	// toward the wrapping width.
	quiet := func(prefix, value string) {
		texts := []string{prefix + value}
		if width > 0 {
			texts = ui.PrefixedLines(prefix, value, width)
		}
		for _, text := range texts {
			fmt.Fprintln(w, pal.Dim(text))
		}
	}
	for n, name := range names {
		if n > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, pal.Bold(titleName(name)))
		var shown []usageCredential
		for _, row := range rows {
			// An editor row is nothing but its total, so the summary shows it only with one.
			if row.provider == name && !(row.shared && (!row.value.Available || !details && usageTotal(row) == "")) {
				shown = append(shown, row)
			}
		}
		if len(shown) == 0 {
			line("  ", "No credentials. Sign in: coop login "+name)
			continue
		}
		cols := usageLayout(shown, details, width)
		// A value, balance, reason or total starts at the value column, after an empty bar slot
		// when the provider shows bars.
		gap := "  "
		if cols.bars {
			gap += strings.Repeat(" ", 12)
		}
		prefixWidth := 4 + cols.labelWidth + utf8.RuneCountInString(gap)
		stack := width > 0 && prefixWidth+usageMinValueWidth > width
		// columns prints a label, an optional bar and a value, continuing an overflowing value in
		// the value column; a terminal too narrow for the columns stacks the row instead.
		columns := func(label, bar, coloredBar, value string) {
			if stack {
				if label != "" {
					line("    ", label)
				}
				if bar != "" {
					fmt.Fprintf(w, "      %s\n", coloredBar)
				}
				for _, text := range usageWrap(value, width-6) {
					fmt.Fprintln(w, "      "+text)
				}
				return
			}
			slot := gap
			if bar != "" {
				slot = "  " + coloredBar + "  "
			}
			values := []string{value}
			if width > 0 {
				values = usageWrap(value, width-prefixWidth)
			}
			fmt.Fprintf(w, "    %s%s%s\n", padRight(label, cols.labelWidth), slot, values[0])
			for _, more := range values[1:] {
				fmt.Fprintf(w, "%s%s\n", strings.Repeat(" ", prefixWidth), more)
			}
		}
		fact := func(label, value string) { columns(label, "", "", value) }
		for i, row := range shown {
			// Blank lines separate multi-line accounts; one-line accounts stack together.
			if i > 0 && (!cols.oneLine[i] || !cols.oneLine[i-1]) {
				fmt.Fprintln(w)
			}
			header := usageHeader(row, details)
			if cols.oneLine[i] {
				total := usageTotal(row)
				if total == "" {
					fmt.Fprintln(w, "  "+header)
					continue
				}
				pad := strings.Repeat(" ", cols.valueColumn-2-utf8.RuneCountInString(header))
				fmt.Fprintln(w, "  "+header+pad+pal.Dim(total))
				continue
			}
			buckets := usageShownBuckets(row, details)
			// A note about limits that are shown qualifies the whole account, so it rides the
			// account line; without limits, the note is the account's one reason line.
			note := agents.DisplayTarget(row.quota.Note)
			if note != "" && row.quotaErr == nil && len(buckets) > 0 && (width <= 0 || 2+utf8.RuneCountInString(header+" · "+note) <= width) {
				fmt.Fprintln(w, "  "+header+pal.Dim(" · "+note))
				note = ""
			} else {
				line("  ", header)
			}
			if row.quotaErr != nil {
				fact("Limits", "unavailable · "+agents.DisplayTarget(row.quotaErr.Error()))
				if errors.Is(row.quotaErr, agents.ErrUsageSignIn) {
					fact("", "Run: "+agents.LoginCommand(name+"@"+row.account))
				}
			} else if usageLimitsUnknown(row, buckets) {
				fact("Limits", "unavailable")
			}
			shownResets := make(map[int64]bool)
			for _, bucket := range buckets {
				label := agents.DisplayTarget(bucket.Name)
				text := usageBucketText(bucket, details, now, shownResets)
				bar, colored := "", ""
				if bucket.Used != nil {
					filled := int(math.Round(min(100, *bucket.Used) / 10))
					bar = strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)
					colored = pal.Cyan(bar)
					if *bucket.Used >= 100 {
						colored = pal.Red(bar)
					} else if *bucket.Used >= 80 {
						colored = pal.Yellow(bar)
					}
				}
				columns(label, bar, colored, text)
			}
			if note != "" {
				r, size := utf8.DecodeRuneInString(note)
				line("    ", string(unicode.ToUpper(r))+note[size:])
			}
			if details {
				fact("30-day API estimate", usageValueLabel(row, true))
				if row.shared {
					fact("Credential", "unknown · excluded from credential totals")
				}
				if row.unpricedTurns > 0 {
					fact("Coop records", fmt.Sprintf("%d retained turns · unpriced aggregates, not added", row.unpricedTurns))
				}
				continue
			}
			if total := usageTotal(row); total != "" {
				indent := max(cols.valueColumn, 4)
				if width > 0 && indent+utf8.RuneCountInString(total) > width {
					indent = 4
				}
				quiet(strings.Repeat(" ", indent), total)
			}
		}
	}
}

// usageColumns is one provider's layout. Labels pad to labelWidth, bars follow them, and every
// value (percentage, balance, reason or Σ total) starts at valueColumn, so a provider's columns
// fit its own labels rather than the longest label anywhere in the report.
type usageColumns struct {
	labelWidth, valueColumn int
	bars                    bool
	oneLine                 []bool // accounts with nothing but a total print it on their header line
}

func usageLayout(rows []usageCredential, details bool, width int) usageColumns {
	cols := usageColumns{oneLine: make([]bool, len(rows))}
	blocks := false
	for i, row := range rows {
		buckets := usageShownBuckets(row, details)
		if !details && len(buckets) == 0 && row.quotaErr == nil && row.quota.Note == "" && (row.shared || row.quota.Auth != "") {
			cols.oneLine[i] = true
			continue
		}
		blocks = true
		for _, bucket := range buckets {
			cols.labelWidth = max(cols.labelWidth, utf8.RuneCountInString(agents.DisplayTarget(bucket.Name)))
			cols.bars = cols.bars || bucket.Used != nil
		}
		labels := []string{}
		if row.quotaErr != nil || usageLimitsUnknown(row, buckets) {
			labels = append(labels, "Limits")
		}
		if details {
			labels = append(labels, "30-day API estimate")
			if row.shared {
				labels = append(labels, "Credential")
			}
			if row.unpricedTurns > 0 {
				labels = append(labels, "Coop records")
			}
		}
		for _, label := range labels {
			cols.labelWidth = max(cols.labelWidth, utf8.RuneCountInString(label))
		}
	}
	gutter := 6
	if cols.bars {
		gutter += 12
	}
	if blocks {
		cols.valueColumn = cols.labelWidth + gutter
	}
	// A one-line account sets its total two spaces after its name, up to a modest column; a
	// longer name or a narrow terminal gives it a total line of its own instead.
	limit := max(cols.valueColumn, 40)
	for i, row := range rows {
		if !cols.oneLine[i] {
			continue
		}
		total := usageTotal(row)
		if total == "" {
			continue
		}
		column := 2 + utf8.RuneCountInString(usageHeader(row, details)) + 2
		if column > limit || width > 0 && column+utf8.RuneCountInString(total) > width {
			cols.oneLine[i] = false
			continue
		}
		cols.valueColumn = max(cols.valueColumn, column)
	}
	for i, row := range rows {
		if cols.oneLine[i] && width > 0 && cols.valueColumn+utf8.RuneCountInString(usageTotal(row)) > width {
			cols.oneLine[i] = false
		}
	}
	// Labels widen with the value column, keeping each bar directly before its value.
	cols.labelWidth = max(cols.labelWidth, cols.valueColumn-gutter)
	return cols
}

func usageHeader(row usageCredential, details bool) string {
	header := agents.DisplayTarget(row.account)
	if row.shared {
		header = "Unassigned editor usage"
	}
	if row.quota.Auth != "" {
		header += " (" + agents.DisplayTarget(row.quota.Auth) + ")"
	}
	if details && row.quota.Plan != "" {
		header += " · " + agents.DisplayTarget(row.quota.Plan)
	}
	return header
}

// usageShownBuckets drops extra usage the provider reports as disabled; account details keep it.
func usageShownBuckets(row usageCredential, details bool) []agents.UsageBucket {
	var out []agents.UsageBucket
	for _, bucket := range row.quota.Buckets {
		if details || bucket.Note != "disabled" {
			out = append(out, bucket)
		}
	}
	return out
}

// usageLimitsUnknown is an account whose lookup returned neither limits nor a reason for them.
func usageLimitsUnknown(row usageCredential, buckets []agents.UsageBucket) bool {
	return !row.shared && row.quotaErr == nil && len(buckets) == 0 && row.quota.Note == "" && row.quota.Auth == ""
}

func usageBucketText(bucket agents.UsageBucket, details bool, now time.Time, shownResets map[int64]bool) string {
	text := ""
	if bucket.Used != nil {
		// Right-aligned to the width of "100%", so "used" and the resets line up in every row.
		text = fmt.Sprintf("%3.0f%% used", *bucket.Used)
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
	// The provider's own "not allowed" flag only restates a full bar in the summary; account
	// details keep it for limits, never for a credit balance.
	if details && bucket.Used != nil && bucket.Available != nil && !*bucket.Available {
		text += " · limit reached"
	}
	if bucket.Note != "" {
		text += " · " + agents.DisplayTarget(bucket.Note)
	}
	if details || !bucket.Reset.IsZero() && !(bucket.Used != nil && *bucket.Used == 0 && shownResets[bucket.Reset.Unix()]) {
		text += " · " + usageResetLabel(bucket.Reset, now)
		shownResets[bucket.Reset.Unix()] = true
	}
	return text
}

// usageMinValueWidth is the narrowest value column worth keeping beside labels and bars.
const usageMinValueWidth = 12

// usageWrap fits a value into room columns, breaking at its " · " separators so each fact stays
// whole where it can. A fact that is still too long breaks before its parenthetical, as in
// "resets in 2d 2h" / "(Oct 4, 08:35 UTC)", and only then by words.
func usageWrap(value string, room int) []string {
	if utf8.RuneCountInString(value) <= room {
		return []string{value}
	}
	type piece struct{ text, join string }
	var pieces []piece
	for i, part := range strings.Split(value, " · ") {
		join := " · "
		if i == 0 {
			join = ""
		}
		if head, tail, ok := strings.Cut(part, " ("); ok && utf8.RuneCountInString(part) > room {
			pieces = append(pieces, piece{head, join}, piece{"(" + tail, " "})
			continue
		}
		pieces = append(pieces, piece{part, join})
	}
	var lines []string
	current := ""
	for _, p := range pieces {
		if current != "" && utf8.RuneCountInString(current+p.join+p.text) <= room {
			current += p.join + p.text
			continue
		}
		if current != "" {
			lines = append(lines, current)
		}
		current = p.text
		if utf8.RuneCountInString(p.text) > room {
			wrapped := ui.WrapLines(p.text, room)
			lines = append(lines, wrapped[:len(wrapped)-1]...)
			current = wrapped[len(wrapped)-1]
		}
	}
	return append(lines, current)
}

// usageTotal is the summary's Σ total. Without a dollar estimate there is no total to show:
// account details say why.
func usageTotal(row usageCredential) string {
	if value := usageValueLabel(row, false); strings.HasPrefix(value, "≈$") {
		return "Σ" + value
	}
	return ""
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
	format := "Jan 2, 15:04"
	if reset.UTC().Year() != now.UTC().Year() {
		format = "Jan 2 2006, 15:04"
	}
	when := reset.UTC().Format(format) + " UTC"
	if !reset.After(now) {
		return "reset passed (" + when + ")"
	}
	minutes := int(math.Ceil(reset.Sub(now).Minutes()))
	left := fmt.Sprintf("%dm", minutes)
	if days := minutes / (24 * 60); days > 0 {
		left = fmt.Sprintf("%dd %dh", days, minutes%(24*60)/60)
	} else if minutes >= 60 {
		left = fmt.Sprintf("%dh %dm", minutes/60, minutes%60)
	}
	return "resets in " + left + " (" + when + ")"
}
