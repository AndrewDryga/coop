// Optional API billing reports stay separate from local token estimates and subscription quotas.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/safefile"
)

type UsageReportSource struct {
	Provider           string   `json:"provider"`
	Name               string   `json:"name"`
	KeyEnv             string   `json:"key_env,omitempty"`
	Organization       string   `json:"organization,omitempty"`
	Team               string   `json:"team,omitempty"`
	Project            string   `json:"project,omitempty"`
	BillingTable       string   `json:"billing_table,omitempty"`
	SKUIds             []string `json:"sku_ids,omitempty"`
	Location           string   `json:"location,omitempty"`
	GcloudAccount      string   `json:"gcloud_account,omitempty"`
	MaximumBytesBilled int64    `json:"maximum_bytes_billed,omitempty"`
}

type UsageReport struct {
	Source              UsageReportSource
	USD                 float64
	Start, End, Checked time.Time
	Partial             bool
	Err                 error
}

type usageReportFormat uint8

const (
	reportDailyUSD usageReportFormat = iota + 1
	reportDailyCents
	reportTeamUSD
	reportBillingExport
)

// Adapters declare their reporting protocol and scope; the engine never enumerates agents.
type usageReportSpec struct {
	format          usageReportFormat
	label, endpoint string
	scope           func(UsageReportSource) string
}

func reportSpec(provider string) usageReportSpec {
	ag, ok := Get(provider)
	if !ok {
		return usageReportSpec{}
	}
	return ag.Usage().Reports
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
var reportName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,63}$`)
var googleAccount = regexp.MustCompile(`^[A-Za-z0-9_.+%-]+@[A-Za-z0-9.-]+$`)

// LoadUsageReports never creates a file or reads a reusable credential. Absence means reporting is off.
func LoadUsageReports(home string) ([]UsageReportSource, error) {
	if home == "" {
		return nil, nil
	}
	root, err := safefile.OpenRoot(home)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot safely open usage-reports.json")
	}
	defer root.Close()
	data, err := safefile.ReadRegular(root, "usage-reports.json", 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot safely read usage-reports.json")
	}
	var config struct {
		Reports []UsageReportSource `json:"reports"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&config) != nil || d.Decode(new(any)) != io.EOF || len(config.Reports) > len(Names()) {
		return nil, errors.New("usage-reports.json has an unsupported format")
	}
	seen := map[string]bool{}
	for _, s := range config.Reports {
		if seen[s.Provider] || !validSource(s) {
			return nil, errors.New("usage-reports.json needs one valid source per provider; see coop help usage")
		}
		seen[s.Provider] = true
	}
	return config.Reports, nil
}

func validSource(s UsageReportSource) bool {
	if !reportName.MatchString(s.Name) {
		return false
	}
	googleFields := s.Project != "" || s.BillingTable != "" || len(s.SKUIds) > 0 || s.Location != "" || s.GcloudAccount != "" || s.MaximumBytesBilled != 0
	switch reportSpec(s.Provider).format {
	case reportDailyUSD:
		return envName.MatchString(s.KeyEnv) && identifier.MatchString(s.Organization) && s.Team == "" && !googleFields
	case reportDailyCents:
		return envName.MatchString(s.KeyEnv) && s.Organization == "" && s.Team == "" && !googleFields
	case reportTeamUSD:
		return envName.MatchString(s.KeyEnv) && identifier.MatchString(s.Team) && s.Organization == "" && !googleFields
	case reportBillingExport:
		parts := strings.Split(s.BillingTable, ".")
		if s.KeyEnv != "" || s.Organization != "" || s.Team != "" || len(parts) != 3 || !identifier.MatchString(s.Project) || !identifier.MatchString(s.Location) || !googleAccount.MatchString(s.GcloudAccount) || s.MaximumBytesBilled < 1 || s.MaximumBytesBilled > 1<<30 || len(s.SKUIds) == 0 || len(s.SKUIds) > 100 {
			return false
		}
		for _, part := range parts {
			if !identifier.MatchString(part) {
				return false
			}
		}
		for _, sku := range s.SKUIds {
			if !identifier.MatchString(sku) {
				return false
			}
		}
		return true
	}
	return false
}

// ReadUsageReport uses complete UTC days because daily provider billing buckets cannot prove a rolling
// 720-hour cost. No source can supply an endpoint or executable from project configuration.
func ReadUsageReport(ctx context.Context, s UsageReportSource, now time.Time) UsageReport {
	r := UsageReport{Source: s, End: now.UTC().Truncate(24 * time.Hour)}
	r.Start = r.End.AddDate(0, 0, -30)
	if !validSource(s) {
		r.Err = errors.New("invalid reporting source")
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var key string
	spec := reportSpec(s.Provider)
	if spec.format == reportBillingExport {
		key, r.Err = googleToken(ctx, s.GcloudAccount)
	} else {
		key = os.Getenv(s.KeyEnv)
		if key == "" || len(key) > 16<<10 || strings.ContainsAny(key, "\r\n") {
			r.Err = fmt.Errorf("set %s in the host shell to a valid reporting key", s.KeyEnv)
		}
	}
	if r.Err != nil {
		return r
	}
	switch spec.format {
	case reportDailyUSD, reportDailyCents:
		r.Err = readCosts(ctx, key, &r)
	case reportTeamUSD:
		r.Err = readXAI(ctx, key, &r)
	case reportBillingExport:
		r.Err = readGoogle(ctx, key, &r)
	}
	if r.Err == nil && (math.IsNaN(r.USD) || math.IsInf(r.USD, 0) || math.Abs(r.USD) > 1e12) {
		r.Err = errors.New("reported amount is invalid")
	}
	r.Checked = time.Now()
	return r
}

func request(ctx context.Context, method, endpoint string, headers http.Header, body any, out any) error {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return errors.New("cannot construct report request")
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, input)
	if err != nil {
		return errors.New("cannot construct report request")
	}
	req.Header = headers
	client := http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("report lookup timed out")
		}
		return errors.New("report request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return errors.New("reporting credential lacks access")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("report lookup returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("report response exceeds its read limit or is incomplete")
	}
	if json.Unmarshal(data, out) != nil {
		return errors.New("report response has an unsupported format")
	}
	return nil
}

func readCosts(ctx context.Context, key string, r *UsageReport) error {
	spec := reportSpec(r.Source.Provider)
	endpoint := spec.endpoint
	q := url.Values{"start_time": {strconv.FormatInt(r.Start.Unix(), 10)}, "end_time": {strconv.FormatInt(r.End.Unix(), 10)}, "bucket_width": {"1d"}, "limit": {"31"}}
	headers := http.Header{"Authorization": {"Bearer " + key}, "Openai-Organization": {r.Source.Organization}}
	if spec.format == reportDailyCents {
		q = url.Values{"starting_at": {r.Start.Format(time.RFC3339)}, "ending_at": {r.End.Format(time.RFC3339)}, "bucket_width": {"1d"}, "limit": {"31"}}
		headers = http.Header{"X-Api-Key": {key}, "Anthropic-Version": {"2023-06-01"}}
		r.Partial = true // The organization cost API excludes Priority Tier charges.
	}
	seen := map[string]bool{}
	buckets := map[int64]bool{}
	for page := 0; page < 8; page++ {
		var response struct {
			Data *[]struct {
				Start    int64  `json:"start_time"`
				End      int64  `json:"end_time"`
				Starting string `json:"starting_at"`
				Ending   string `json:"ending_at"`
				Results  *[]struct {
					Amount   json.RawMessage `json:"amount"`
					Currency string          `json:"currency"`
				} `json:"results"`
			} `json:"data"`
			HasMore *bool   `json:"has_more"`
			Next    *string `json:"next_page"`
		}
		if err := request(ctx, "GET", endpoint+"?"+q.Encode(), headers, nil, &response); err != nil {
			return err
		}
		if response.Data == nil || response.HasMore == nil {
			return errors.New("report page is incomplete")
		}
		for _, b := range *response.Data {
			start, end := time.Unix(b.Start, 0), time.Unix(b.End, 0)
			if spec.format == reportDailyCents {
				var err error
				start, err = time.Parse(time.RFC3339, b.Starting)
				if err != nil {
					return errors.New("report bucket time is invalid")
				}
				end, err = time.Parse(time.RFC3339, b.Ending)
				if err != nil {
					return errors.New("report bucket time is invalid")
				}
			}
			if start.Before(r.Start) || end.After(r.End) || end.Sub(start) != 24*time.Hour || !start.Equal(start.UTC().Truncate(24*time.Hour)) || buckets[start.Unix()] || b.Results == nil {
				return errors.New("report bucket is incomplete or overlaps")
			}
			buckets[start.Unix()] = true
			for _, item := range *b.Results {
				var value float64
				if spec.format == reportDailyCents {
					var cents string
					if json.Unmarshal(item.Amount, &cents) != nil || item.Currency != "USD" {
						return errors.New("report amount or currency is unsupported")
					}
					var err error
					value, err = strconv.ParseFloat(cents, 64)
					if err != nil {
						return errors.New("report amount is invalid")
					}
					value /= 100
				} else {
					var amount struct {
						Value    *float64 `json:"value"`
						Currency string   `json:"currency"`
					}
					if json.Unmarshal(item.Amount, &amount) != nil || amount.Value == nil || amount.Currency != "usd" {
						return errors.New("report amount or currency is unsupported")
					}
					value = *amount.Value
				}
				if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > 1e12 {
					return errors.New("report amount is invalid")
				}
				r.USD += value
			}
		}
		if !*response.HasMore {
			if len(buckets) == 0 {
				return errors.New("report has no usage buckets; coverage is unknown")
			}
			r.Partial = r.Partial || len(buckets) != 30
			return nil
		}
		if response.Next == nil || *response.Next == "" || len(*response.Next) > 4096 || seen[*response.Next] {
			return errors.New("report pagination is incomplete")
		}
		seen[*response.Next] = true
		q.Set("page", *response.Next)
	}
	return errors.New("report exceeds its page limit")
}

func readXAI(ctx context.Context, key string, r *UsageReport) error {
	body := map[string]any{"analyticsRequest": map[string]any{
		"timeRange": map[string]string{"startTime": r.Start.Format("2006-01-02 15:04:05"), "endTime": r.End.Format("2006-01-02 15:04:05"), "timezone": "Etc/GMT"},
		"timeUnit":  "TIME_UNIT_NONE", "values": []map[string]string{{"name": "usd", "aggregation": "AGGREGATION_SUM"}}, "groupBy": []string{}, "filters": []string{},
	}}
	var response struct {
		TimeSeries *[]struct {
			Group      []string `json:"group"`
			DataPoints []struct {
				Values []*float64 `json:"values"`
			} `json:"dataPoints"`
		} `json:"timeSeries"`
		LimitReached *bool `json:"limitReached"`
	}
	if err := request(ctx, "POST", reportSpec(r.Source.Provider).endpoint+r.Source.Team+"/usage", http.Header{"Authorization": {"Bearer " + key}, "Content-Type": {"application/json"}}, body, &response); err != nil {
		return err
	}
	if response.TimeSeries == nil || response.LimitReached == nil || *response.LimitReached || len(*response.TimeSeries) != 1 {
		return errors.New("report is incomplete or truncated")
	}
	for _, series := range *response.TimeSeries {
		if len(series.Group) != 0 || len(series.DataPoints) != 1 || len(series.DataPoints[0].Values) != 1 || series.DataPoints[0].Values[0] == nil {
			return errors.New("report has an unsupported aggregation")
		}
		r.USD += *series.DataPoints[0].Values[0]
	}
	return nil
}

type tokenOutput struct{ bytes.Buffer }

func (b *tokenOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 16<<10 {
		return 0, errors.New("token output too large")
	}
	return b.Buffer.Write(p)
}

func googleToken(ctx context.Context, account string) (string, error) {
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "print-access-token", account, "--quiet")
	// A gcloud wrapper may leave helpers holding stdout after its leader exits. Cancel the
	// owned group and bound pipe cleanup, just as runtime clients do.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	var out tokenOutput
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		if cmd.Process != nil {
			_ = cmd.Cancel()
		}
		return "", errors.New("reporting with Google needs gcloud and a signed-in configured account")
	}
	key := strings.TrimSpace(out.String())
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return "", errors.New("the Google reporting token is invalid")
	}
	return key, nil
}

func readGoogle(ctx context.Context, key string, r *UsageReport) error {
	s := r.Source
	query := "SELECT COUNT(*) AS records, IFNULL(SUM(cost + IFNULL((SELECT SUM(c.amount) FROM UNNEST(credits) c), 0)), 0) AS amount, COUNTIF(currency IS NULL OR currency != 'USD') AS non_usd FROM `" + s.BillingTable + "` WHERE project.id = @project AND sku.id IN UNNEST(@skus) AND usage_start_time >= @start AND usage_start_time < @end"
	parameter := func(name, kind string, value any) map[string]any {
		return map[string]any{"name": name, "parameterType": map[string]string{"type": kind}, "parameterValue": map[string]any{"value": value}}
	}
	values := make([]map[string]string, 0, len(s.SKUIds))
	for _, sku := range s.SKUIds {
		values = append(values, map[string]string{"value": sku})
	}
	body := map[string]any{"query": query, "useLegacySql": false, "useQueryCache": true, "maxResults": 1, "timeoutMs": 5000, "jobTimeoutMs": "10000", "maximumBytesBilled": strconv.FormatInt(s.MaximumBytesBilled, 10), "location": s.Location, "parameterMode": "NAMED", "queryParameters": []any{parameter("project", "STRING", s.Project), parameter("start", "TIMESTAMP", r.Start.Format(time.RFC3339)), parameter("end", "TIMESTAMP", r.End.Format(time.RFC3339)), map[string]any{"name": "skus", "parameterType": map[string]any{"type": "ARRAY", "arrayType": map[string]string{"type": "STRING"}}, "parameterValue": map[string]any{"arrayValues": values}}}}
	var response struct {
		Complete bool              `json:"jobComplete"`
		Page     string            `json:"pageToken"`
		Total    string            `json:"totalRows"`
		Errors   []json.RawMessage `json:"errors"`
		Rows     []struct {
			Fields []struct {
				Value *string `json:"v"`
			} `json:"f"`
		} `json:"rows"`
	}
	if err := request(ctx, "POST", reportSpec(s.Provider).endpoint+s.Project+"/queries", http.Header{"Authorization": {"Bearer " + key}, "Content-Type": {"application/json"}}, body, &response); err != nil {
		return err
	}
	if !response.Complete || response.Page != "" || response.Total != "1" || len(response.Errors) > 0 || len(response.Rows) != 1 || len(response.Rows[0].Fields) != 3 {
		return errors.New("billing query is incomplete or unsupported")
	}
	f := response.Rows[0].Fields
	for _, field := range f {
		if field.Value == nil {
			return errors.New("billing query fields are incomplete")
		}
	}
	count, err := strconv.ParseInt(*f[0].Value, 10, 64)
	if err != nil || count <= 0 {
		return errors.New("billing export has no matching usage; coverage is unknown")
	}
	if *f[2].Value != "0" {
		return errors.New("billing currency is not USD")
	}
	r.USD, err = strconv.ParseFloat(*f[1].Value, 64)
	if err != nil {
		return errors.New("billing amount is invalid")
	}
	r.Partial = true // Only explicitly selected Gemini/Vertex SKUs, with export lag/backfill limits.
	return nil
}

func (s UsageReportSource) Label() string {
	return reportSpec(s.Provider).label + " / " + s.Name
}

func (s UsageReportSource) Scope() string {
	if scope := reportSpec(s.Provider).scope; scope != nil {
		return scope(s)
	}
	return "unsupported"
}

func UsageReportSelected(s UsageReportSource, names []string) bool {
	return slices.Contains(names, s.Provider)
}
