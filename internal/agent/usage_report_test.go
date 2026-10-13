package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

type reportTransport func(*http.Request) (*http.Response, error)

func (f reportTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func useTransport(t *testing.T, f reportTransport) {
	t.Helper()
	prior := http.DefaultTransport
	http.DefaultTransport = f
	t.Cleanup(func() { http.DefaultTransport = prior })
}
func source(provider string) UsageReportSource {
	s := UsageReportSource{Provider: provider, Name: "work", KeyEnv: "COOP_TEST_REPORT_KEY"}
	switch provider {
	case "codex":
		s.Organization = "org-work"
	case "grok":
		s.Team = "team-work"
	case "gemini":
		s.KeyEnv = ""
		s.Project = "billing-project"
		s.BillingTable = "billing-project.billing.export"
		s.SKUIds = []string{"sku-gemini"}
		s.GcloudAccount = "billing@example.com"
		s.Location = "US"
		s.MaximumBytesBilled = 10485760
	}
	return s
}

func TestReportConfigurationIsOptInStrictAndBounded(t *testing.T) {
	home := t.TempDir()
	if got, err := LoadUsageReports(home); err != nil || got != nil {
		t.Fatalf("absent %v %v", got, err)
	}
	path := filepath.Join(home, "usage-reports.json")
	for _, data := range []string{
		`{"reports":[{"provider":"codex","name":"work","key_env":"KEY","organization":"org-work"}]}`,
		`{"reports":[]}`,
	} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadUsageReports(home); err != nil {
			t.Fatal(err)
		}
	}
	for _, data := range []string{
		`{"reports":[{"provider":"codex","name":"work","key_env":"KEY","organization":"org-work","endpoint":"https://evil.example"}]}`,
		`{"reports":[{"provider":"claude","name":"work","key_env":"KEY"},{"provider":"claude","name":"other","key_env":"KEY2"}]}`,
		`{"reports":[{"provider":"grok","name":"work","key_env":"KEY","team":"../evil"}]}`,
		`{"reports":[{"provider":"claude","name":"work\u001b[31m","key_env":"KEY"}]}`,
		`{"reports":[]} {}`,
		strings.Repeat(" ", 65537),
	} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadUsageReports(home); err == nil {
			t.Fatalf("accepted invalid config %.80s", data)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "secret"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUsageReports(home); err == nil {
		t.Fatal("followed symlink")
	}
}

func TestReportSourcesValidateWithoutReadingSecrets(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok", "gemini"} {
		if !validSource(source(provider)) {
			t.Fatal(provider)
		}
	}
	s := source("gemini")
	s.BillingTable = "billing-project.billing.export`; DROP TABLE x"
	if validSource(s) {
		t.Fatal("accepted SQL identifier injection")
	}
	s = source("gemini")
	s.MaximumBytesBilled = 0
	if validSource(s) {
		t.Fatal("unbounded billed query")
	}
	s = source("gemini")
	s.SKUIds = nil
	if validSource(s) {
		t.Fatal("unfiltered cloud spending")
	}
	s = source("gemini")
	s.GcloudAccount = "--impersonate-service-account=evil"
	if validSource(s) {
		t.Fatal("gcloud argument injection")
	}
	s = source("claude")
	s.KeyEnv = "HOME\n"
	if validSource(s) {
		t.Fatal("invalid key reference")
	}
}

func TestReportCostPagesAndUnits(t *testing.T) {
	now := time.Date(2026, 10, 13, 14, 35, 0, 0, time.UTC)
	end := now.Truncate(24 * time.Hour)
	start := end.AddDate(0, 0, -30)
	t.Setenv("COOP_TEST_REPORT_KEY", "private-report-key")
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			calls := 0
			useTransport(t, func(req *http.Request) (*http.Response, error) {
				if req.Method != "GET" {
					t.Fatal(req.Method)
				}
				q := req.URL.Query()
				if q.Get("limit") != "31" || q.Get("bucket_width") != "1d" {
					t.Fatal(q)
				}
				if provider == "codex" {
					if req.URL.Host != "api.openai.com" || req.Header.Get("Authorization") != "Bearer private-report-key" || req.Header.Get("OpenAI-Organization") != "org-work" {
						t.Fatal("OpenAI authority")
					}
				} else {
					if req.URL.Host != "api.anthropic.com" || req.Header.Get("X-Api-Key") != "private-report-key" || req.Header.Get("Anthropic-Version") != "2023-06-01" {
						t.Fatal("Anthropic authority")
					}
				}
				day := start.AddDate(0, 0, calls)
				amount := `{"amount":{"value":1.234,"currency":"usd"}}`
				bucket := fmt.Sprintf(`{"start_time":%d,"end_time":%d,"results":[%s]}`, day.Unix(), day.AddDate(0, 0, 1).Unix(), amount)
				if provider == "claude" {
					bucket = fmt.Sprintf(`{"starting_at":%q,"ending_at":%q,"results":[{"amount":"123.4","currency":"USD"}]}`, day.Format(time.RFC3339), day.AddDate(0, 0, 1).Format(time.RFC3339))
				}
				calls++
				if calls == 1 {
					return response(`{"data":[` + bucket + `],"has_more":true,"next_page":"cursor"}`), nil
				}
				if q.Get("page") != "cursor" {
					t.Fatal(q)
				}
				return response(`{"data":[` + bucket + `],"has_more":false,"next_page":null}`), nil
			})
			r := ReadUsageReport(context.Background(), source(provider), now)
			if r.Err != nil || r.USD != 2.468 || calls != 2 || !r.Start.Equal(start) || !r.End.Equal(end) || !r.Partial || r.Checked.IsZero() {
				t.Fatalf("%+v calls=%d", r, calls)
			}
		})
	}
}

func TestReportRejectsIncompleteDeniedAndLeakingResponses(t *testing.T) {
	t.Setenv("COOP_TEST_REPORT_KEY", "private-report-key")
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"missing", "{}", 200}, {"null", `{"data":null,"has_more":false}`, 200},
		{"empty", `{"data":[],"has_more":false}`, 200},
		{"cursor", `{"data":[],"has_more":true,"next_page":null}`, 200},
		{"repeated", `{"data":[],"has_more":true,"next_page":"again"}`, 200},
		{"denied", "private-report-key sensitive provider body", 403},
		{"server", "private-report-key sensitive provider body", 500},
		{"oversized", strings.Repeat("x", (1<<20)+1), 200},
		{"redirect", "private-report-key", 302},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			useTransport(t, func(req *http.Request) (*http.Response, error) {
				calls++
				r := response(tc.body)
				r.StatusCode = tc.status
				r.Header.Set("Location", "https://evil.example/secret")
				return r, nil
			})
			r := ReadUsageReport(context.Background(), source("codex"), time.Now())
			if r.Err == nil || strings.Contains(r.Err.Error(), "private-report-key") || strings.Contains(r.Err.Error(), "evil.example") {
				t.Fatalf("%+v", r)
			}
			if tc.name == "redirect" && calls != 1 {
				t.Fatal("followed redirect")
			}
		})
	}
}

func TestReportXAIUsesActualUSDAndRejectsTruncation(t *testing.T) {
	t.Setenv("COOP_TEST_REPORT_KEY", "key")
	for _, body := range []string{
		`{"timeSeries":[{"group":[],"dataPoints":[{"values":[12.34]}]}],"limitReached":false}`,
		`{"timeSeries":[],"limitReached":true}`,
		`{"timeSeries":[],"limitReached":false}`,
		`{"timeSeries":[{"group":[],"dataPoints":[{"values":[null]}]}],"limitReached":false}`,
		`{"timeSeries":[{"group":[],"dataPoints":[{"values":[]}]}],"limitReached":false}`,
	} {
		t.Run(body[:20], func(t *testing.T) {
			useTransport(t, func(req *http.Request) (*http.Response, error) {
				data, _ := io.ReadAll(req.Body)
				if req.Method != "POST" || req.URL.Host != "management-api.x.ai" || !strings.Contains(string(data), `"name":"usd"`) || !strings.Contains(string(data), `"timeUnit":"TIME_UNIT_NONE"`) {
					t.Fatalf("%s %s", req.Method, data)
				}
				return response(body), nil
			})
			r := ReadUsageReport(context.Background(), source("grok"), time.Now())
			if strings.Contains(body, "12.34") {
				if r.Err != nil || r.USD != 12.34 {
					t.Fatalf("%+v", r)
				}
			} else if r.Err == nil {
				t.Fatalf("accepted incomplete %s", body)
			}
		})
	}
}

func TestReportGoogleQueryIsScopedBoundedAndReadOnly(t *testing.T) {
	s := source("gemini")
	r := UsageReport{Source: s, Start: time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -30), End: time.Now().UTC().Truncate(24 * time.Hour)}
	useTransport(t, func(req *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(req.Body)
		text := string(data)
		for _, want := range []string{`"maximumBytesBilled":"10485760"`, `sku.id IN UNNEST(@skus)`, `project.id = @project`, `"useLegacySql":false`, `"value":"sku-gemini"`, `UNNEST(credits)`} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %s: %s", want, text)
			}
		}
		if req.URL.Host != "bigquery.googleapis.com" || req.Method != "POST" || strings.Contains(text, "INSERT ") || strings.Contains(text, "DELETE ") {
			t.Fatal(text)
		}
		return response(`{"jobComplete":true,"totalRows":"1","rows":[{"f":[{"v":"10"},{"v":"4.50"},{"v":"0"}]}]}`), nil
	})
	if err := readGoogle(context.Background(), "key", &r); err != nil || r.USD != 4.5 || !r.Partial {
		t.Fatalf("%+v %v", r, r.Err)
	}
}

func TestReportGoogleNeverInventsZeroOrIgnoresCurrency(t *testing.T) {
	for _, body := range []string{`{"jobComplete":false}`, `{"jobComplete":true,"totalRows":"1","rows":[{"f":[{"v":"0"},{"v":"0"},{"v":"0"}]}]}`, `{"jobComplete":true,"totalRows":"1","rows":[{"f":[{"v":"10"},{"v":"4.50"},{"v":"1"}]}]}`} {
		useTransport(t, func(*http.Request) (*http.Response, error) { return response(body), nil })
		r := UsageReport{Source: source("gemini")}
		if err := readGoogle(context.Background(), "key", &r); err == nil {
			t.Fatal(body)
		}
	}
}

func TestReportMissingKeyAndCancellation(t *testing.T) {
	t.Setenv("COOP_TEST_REPORT_KEY", "")
	r := ReadUsageReport(context.Background(), source("codex"), time.Now())
	if r.Err == nil {
		t.Fatal("missing key")
	}
	t.Setenv("COOP_TEST_REPORT_KEY", "key")
	useTransport(t, func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = ReadUsageReport(ctx, source("codex"), time.Now())
	if r.Err == nil || !strings.Contains(r.Err.Error(), "timed out") {
		t.Fatalf("%+v", r)
	}
}

func TestReportShiftedDailyBucketAndExplicitZero(t *testing.T) {
	t.Setenv("COOP_TEST_REPORT_KEY", "key")
	now := time.Now().UTC()
	start := now.Truncate(24*time.Hour).AddDate(0, 0, -30)
	for _, shift := range []time.Duration{0, time.Hour} {
		t.Run(shift.String(), func(t *testing.T) {
			useTransport(t, func(*http.Request) (*http.Response, error) {
				day := start.Add(shift)
				return response(fmt.Sprintf(`{"data":[{"start_time":%d,"end_time":%d,"results":[{"amount":{"value":0,"currency":"usd"}}]}],"has_more":false}`, day.Unix(), day.AddDate(0, 0, 1).Unix())), nil
			})
			r := ReadUsageReport(context.Background(), source("codex"), now)
			if shift == 0 {
				if r.Err != nil || r.USD != 0 || !r.Partial {
					t.Fatalf("explicit zero %+v", r)
				}
			} else if r.Err == nil {
				t.Fatal("overlapping/shifted bucket accepted")
			}
		})
	}
	useTransport(t, func(*http.Request) (*http.Response, error) {
		return response(`{"timeSeries":[{"group":[],"dataPoints":[{"values":[0]}]}],"limitReached":false}`), nil
	})
	r := ReadUsageReport(context.Background(), source("grok"), now)
	if r.Err != nil || r.USD != 0 {
		t.Fatalf("explicit team zero %+v", r)
	}
}

func TestReportGoogleTokenCancellationReapsHelper(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "helper.pid")
	script := fmt.Sprintf("#!/bin/sh\nsleep 300 &\nprintf '%%s' \"$!\" > %q\nprintf 'fixture-token\\n'\nwait\n", pidFile)
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := googleToken(ctx, "billing@example.com"); done <- err }()
	wait.ForFile(t, pidFile)
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid fixture pid %s", data)
	}
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled token succeeded")
		}
	case <-time.After(wait.Deadline):
		t.Fatal("token command ignored cancellation")
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("token cancellation exceeded bounded cleanup")
	}
	wait.For(t, "owned gcloud helper exited", func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
}
