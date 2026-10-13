package cli

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/ui"
)

func readUsageReports(ctx context.Context, home string, names []string, now time.Time) ([]agents.UsageReport, error) {
	sources, err := agents.LoadUsageReports(home)
	if err != nil {
		return nil, err
	}
	var reports []agents.UsageReport
	for _, source := range sources {
		if agents.UsageReportSelected(source, names) {
			reports = append(reports, agents.UsageReport{Source: source})
		}
	}
	var wg sync.WaitGroup
	for i := range reports {
		wg.Add(1)
		go func() { defer wg.Done(); reports[i] = agents.ReadUsageReport(ctx, reports[i].Source, now) }()
	}
	wg.Wait()
	return reports, nil
}

func renderUsageReports(w io.Writer, pal ui.Palette, width int, reports []agents.UsageReport, err error) {
	if len(reports) == 0 && err == nil {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, pal.Bold("Provider reports"))
	line := func(prefix, value string, quiet bool) {
		texts := []string{prefix + value}
		if width > 0 {
			texts = ui.PrefixedLines(prefix, value, width)
		}
		for _, text := range texts {
			if quiet {
				text = pal.Dim(text)
			}
			fmt.Fprintln(w, text)
		}
	}
	if err != nil {
		line("  ", "Unavailable: "+err.Error(), false)
		return
	}
	var total float64
	known, partial := false, false
	for _, r := range reports {
		value := "unavailable"
		if r.Err != nil {
			value += ": " + r.Err.Error()
			partial = true
		} else {
			value = fmt.Sprintf("$%.2f/30d", r.USD)
			if r.Partial {
				value += " partial"
				partial = true
			}
			total += r.USD
			known = true
		}
		prefix := "  " + r.Source.Label() + "  "
		if width > 0 && width-len(prefix) < 16 {
			line("  ", r.Source.Label(), false)
			line("    ", value, false)
		} else {
			line(prefix, value, false)
		}
		line("    Scope  ", r.Source.Scope(), true)
		if !r.Start.IsZero() {
			line("    Period ", r.Start.Format("Jan 2, 2006")+" to "+r.End.Format("Jan 2, 2006")+" UTC (end excluded)", true)
		}
		if r.Err == nil {
			line("    Checked ", r.Checked.UTC().Format("Jan 2, 15:04 UTC"), true)
		}
	}
	if known {
		fmt.Fprintln(w)
		value := fmt.Sprintf("$%.2f/30d", total)
		if partial {
			value += " partial"
		}
		line("  Reported API spend  ", value, false)
	}
}
