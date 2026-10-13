package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestUsageReportsAreSeparateAndScopeFailures(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	reports := []agents.UsageReport{
		{Source: agents.UsageReportSource{Provider: "codex", Name: "work", Organization: "org-work"}, USD: 8.4, Start: now.AddDate(0, 0, -30).Truncate(24 * time.Hour), End: now.Truncate(24 * time.Hour), Checked: now},
		{Source: agents.UsageReportSource{Provider: "claude", Name: "work"}, USD: 3.6, Partial: true, Start: now.AddDate(0, 0, -30).Truncate(24 * time.Hour), End: now.Truncate(24 * time.Hour), Checked: now},
		{Source: agents.UsageReportSource{Provider: "grok", Name: "work", Team: "team-work"}, USD: 999, Err: errors.New("reporting credential lacks access")},
	}
	var out bytes.Buffer
	renderUsageReports(&out, ui.Palette{}, 120, reports, nil)
	for _, want := range []string{"Provider reports", "OpenAI API / work  $8.40/30d", "Claude API / work  $3.60/30d partial", "Reported API spend  $12.00/30d partial", "excludes Priority Tier", "end excluded", "Checked", "unavailable: reporting credential lacks access"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %s: %s", want, out.String())
		}
	}
	for _, bad := range []string{"$999", "$0.00", "Σ", "≈", "Total  "} {
		if strings.Contains(out.String(), bad) {
			t.Fatalf("mixed report/estimate: %s", out.String())
		}
	}
	for _, width := range []int{40, 20} {
		out.Reset()
		renderUsageReports(&out, ui.Palette{}, width, reports, nil)
		for _, line := range strings.Split(out.String(), "\n") {
			if len(line) > width {
				t.Fatalf("width%d: %q", width, line)
			}
		}
		if width == 20 && !strings.Contains(out.String(), "    unavailable:") {
			t.Fatal("narrow report split its status into fragments")
		}
	}
	out.Reset()
	renderUsageReports(&out, ui.Palette{}, 80, nil, nil)
	if out.Len() != 0 {
		t.Fatal("disabled reports printed")
	}
	out.Reset()
	renderUsageReports(&out, ui.Palette{}, 80, nil, errors.New("invalid configuration"))
	if !strings.Contains(out.String(), "Unavailable: invalid configuration") || strings.Contains(out.String(), "$0") {
		t.Fatal(out.String())
	}
}

func TestUsageReportsSelectProviderAndDoNotReadMissingKeys(t *testing.T) {
	home := t.TempDir()
	data := `{"reports":[{"provider":"codex","name":"work","organization":"org-work","key_env":"COOP_USAGE_MISSING_TEST_KEY"},{"provider":"claude","name":"work","key_env":"COOP_USAGE_MISSING_TEST_KEY"}]}`
	if err := os.WriteFile(filepath.Join(home, "usage-reports.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_USAGE_MISSING_TEST_KEY", "")
	reports, err := readUsageReports(context.Background(), home, []string{"codex"}, time.Now())
	if err != nil || len(reports) != 1 || reports[0].Source.Provider != "codex" || reports[0].Err == nil {
		t.Fatalf("%+v %v", reports, err)
	}
}
