package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUsageQuotaDecoders(t *testing.T) {
	for _, tc := range []struct {
		name, raw    string
		decode       func([]byte) (UsageQuota, error)
		plan, bucket string
		used         *float64
		remaining    string
		reset        time.Time
	}{
		{"claude", `{"five_hour":null,"seven_day":null,"limits":[{"kind":"weekly_scoped","scope":{"model":{"display_name":"New model"}},"percent":41,"resets_at":"2026-10-05T18:00:00Z"}],"extra_usage":{"is_enabled":false,"utilization":null}}`, func(data []byte) (UsageQuota, error) {
			var raw claudeQuotaResponse
			err := json.Unmarshal(data, &raw)
			return raw.quota(), err
		}, "", "New model", ptrUsage(41), "", time.Date(2026, 10, 5, 18, 0, 0, 0, time.UTC)},
		{"codex", `{"plan_type":"pro","rate_limit":{"allowed":false,"primary_window":{"used_percent":42,"limit_window_seconds":18000,"reset_at":1790816400},"secondary_window":null},"credits":{"has_credits":true,"balance":"12.50"}}`, func(data []byte) (UsageQuota, error) {
			var raw codexQuotaResponse
			err := json.Unmarshal(data, &raw)
			return raw.quota(), err
		}, "pro", "5-hour", ptrUsage(42), "", time.Unix(1790816400, 0)},
		{"gemini", `{"tier":"Pro","buckets":[{"modelId":"native-model","remainingFraction":0,"remainingAmount":"0","resetTime":"2026-10-05T18:00:00Z"}]}`, func(data []byte) (UsageQuota, error) {
			var raw geminiQuotaResponse
			err := json.Unmarshal(data, &raw)
			return raw.quota(), err
		}, "Pro", "native-model", ptrUsage(100), "0", time.Date(2026, 10, 5, 18, 0, 0, 0, time.UTC)},
		{"grok", `{"config":{"creditUsagePercent":37.5,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-10-05T00:00:00Z"}}}`, func(data []byte) (UsageQuota, error) {
			var raw grokQuotaResponse
			err := json.Unmarshal(data, &raw)
			return raw.quota(), err
		}, "", "Weekly credits", ptrUsage(37.5), "", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quota, err := tc.decode([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if quota.Plan != tc.plan || len(quota.Buckets) == 0 {
				t.Fatalf("quota = %+v", quota)
			}
			b := quota.Buckets[0]
			if b.Name != tc.bucket || b.Used == nil || *b.Used != *tc.used || b.Remaining != tc.remaining || !b.Reset.Equal(tc.reset) {
				t.Fatalf("bucket = %+v", b)
			}
		})
	}
	var minimal codexQuotaResponse
	if err := json.Unmarshal([]byte(`{"plan_type":"plus"}`), &minimal); err != nil {
		t.Fatal(err)
	}
	if q := minimal.quota(); len(q.Buckets) != 0 || q.Plan != "plus" {
		t.Fatalf("missing limits became known: %+v", q)
	}
	var unknown geminiQuotaResponse
	if err := json.Unmarshal([]byte(`{"buckets":[{"remainingFraction":null,"resetTime":"bad"}]}`), &unknown); err != nil {
		t.Fatal(err)
	}
	if b := unknown.quota().Buckets[0]; b.Used != nil || !b.Reset.IsZero() {
		t.Fatalf("unknown became zero: %+v", b)
	}
}

func TestUsageGrokLegacyCreditPool(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		used *float64
	}{
		{`{"monthlyLimit":{"val":1000},"used":{"val":350}}`, ptrUsage(35)},
		{`{"monthlyLimit":{"val":1000},"used":{}}`, ptrUsage(0)},
		{`{"monthlyLimit":{"val":1000}}`, nil},
		{`{"monthlyLimit":{},"used":{}}`, nil},
		{`{"creditUsagePercent":42,"monthlyLimit":{"val":1000},"used":{"val":350}}`, ptrUsage(42)},
	} {
		var raw grokQuotaResponse
		if err := json.Unmarshal([]byte(`{"config":`+tc.raw+`}`), &raw); err != nil {
			t.Fatal(err)
		}
		got := raw.quota().Buckets[0].Used
		if (got == nil) != (tc.used == nil) || got != nil && *got != *tc.used {
			t.Fatalf("%s: used = %v", tc.raw, got)
		}
	}
}

func ptrUsage(value float64) *float64 { return &value }

func TestUsageClaudeModernLimitsReplaceCompatibilityWindows(t *testing.T) {
	var raw claudeQuotaResponse
	if err := json.Unmarshal([]byte(`{"five_hour":{"utilization":0},"seven_day":{"utilization":100},"limits":[{"kind":"session","percent":0},{"kind":"weekly_all","percent":100}],"extra_usage":{"is_enabled":false}}`), &raw); err != nil {
		t.Fatal(err)
	}
	quota := raw.quota()
	if len(quota.Buckets) != 3 || quota.Buckets[0].Name != "5-hour" || quota.Buckets[1].Name != "Weekly" || quota.Buckets[2].Note != "disabled" {
		t.Fatalf("duplicate windows/disabled pool: %+v", quota)
	}
}

func TestUsageQuotaHTTPFailureBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		signIn bool
	}{
		{"unauthorized", http.StatusUnauthorized, "private-token", true},
		{"throttled", http.StatusTooManyRequests, "private-token", false},
		{"malformed", http.StatusOK, "private-token", false},
		{"oversized", http.StatusOK, strings.Repeat("x", (1<<20)+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var quota codexQuotaResponse
			err := readUsageQuota(ctx, http.MethodGet, server.URL, http.Header{}, nil, &quota)
			if err == nil || strings.Contains(err.Error(), "private-token") || errors.Is(err, ErrUsageSignIn) != tc.signIn {
				t.Fatalf("error = %v", err)
			}
		})
	}
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Redirect(w, r, "/must-not-follow", http.StatusFound)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var out any
	if err := readUsageQuota(ctx, http.MethodGet, server.URL, http.Header{"Authorization": {"Bearer private-token"}}, nil, &out); err == nil || requests != 1 {
		t.Fatalf("redirect followed: requests=%d error=%v", requests, err)
	}
	cancel()
	if err := readUsageQuota(ctx, http.MethodGet, server.URL, nil, nil, &out); err == nil || requests != 1 {
		t.Fatalf("cancelled quota contacted provider: %v", err)
	}
}

func TestUsageAPIKeyAndNativeHelper(t *testing.T) {
	for _, name := range Names() {
		ag, _ := Get(name)
		t.Run(ag.Name(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			quota, err := ag.Usage().Quota(ctx, UsageQuotaInput{ProfileDir: t.TempDir(), APIKey: true, Native: func(context.Context, []string) ([]byte, error) {
				t.Fatal("API-key quota launched a helper")
				return nil, nil
			}})
			if err != nil || quota.Auth != "API key" || quota.Note != "" || len(quota.Buckets) != 0 {
				t.Fatalf("quota=%+v error=%v", quota, err)
			}
		})
	}
}

func TestUsageGeminiUnavailableReason(t *testing.T) {
	for _, tc := range []struct {
		auth, mode string
		apiKey     bool
	}{
		{"", "API key", true},
		{"gemini-api-key", "API key", false},
		{"vertex-ai", "Vertex", true},
	} {
		t.Run(tc.mode+tc.auth, func(t *testing.T) {
			profile := t.TempDir()
			if tc.auth != "" {
				data := []byte(`{"security":{"auth":{"selectedType":"` + tc.auth + `"}}}`)
				if err := os.WriteFile(filepath.Join(profile, "settings.json"), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			quota, err := geminiUsageQuota(context.Background(), UsageQuotaInput{ProfileDir: profile, APIKey: tc.apiKey,
				Native: func(context.Context, []string) ([]byte, error) {
					t.Fatal("unavailable quota launched a native helper")
					return nil, nil
				}})
			if err != nil || len(quota.Buckets) != 0 || quota.Auth != tc.mode || quota.Note != "" {
				t.Fatalf("quota=%+v error=%v", quota, err)
			}
		})
	}
}

func TestUsageGeminiHelperDrainsCredentialPersistence(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	// Replace only the native exports with a network-free fixture. Execute the actual helper
	// body, including process lifetime: returning quota must not interrupt a pending refresh.
	const native = `import {writeFileSync} from "node:fs";
const events=[];
process.on("exit",()=>writeFileSync(process.env.USAGE_EVENTS,JSON.stringify(events)));
export async function getOauthClient(mode,config) {
  if(mode!=="oauth-personal"||config.getProxy()!==undefined||!config.isBrowserLaunchSuppressed()||config.isInteractive()) throw new Error("unsafe configuration");
  events.push("auth");
  setTimeout(()=>{events.push("persist");if(process.env.USAGE_CASE==="persistence-fails") Promise.reject(new Error("private native detail"));},20);
  return {fixture:true};
}
export class CodeAssistServer {
  constructor(client,project,options) {if(!client.fixture||project!==undefined||Object.keys(options).length) throw new Error("unsafe server configuration");}
  async loadCodeAssist(request) {
    events.push("load");
    if(request.metadata.pluginType!=="GEMINI") throw new Error("wrong request");
    return {currentTier:process.env.USAGE_CASE==="missing-tier"?undefined:{name:"free"},paidTier:{name:"Pro"},cloudaicompanionProject:process.env.USAGE_CASE==="missing-project"?undefined:"existing-project"};
  }
  async retrieveUserQuota(request) {
    events.push("quota");
    if(request.project!=="existing-project"||process.env.USAGE_CASE==="quota-fails") throw new Error("private native detail");
    return {buckets:[{modelId:"native-model",remainingFraction:.25,remainingAmount:"12",resetTime:"2026-10-05T00:00:00Z",privateField:"private native detail"}]};
  }
}`
	first, rest, ok := strings.Cut(geminiQuotaHelper, "\n")
	if !ok || !strings.HasPrefix(first, "import {getOauthClient,CodeAssistServer}") {
		t.Fatal("helper no longer has the reviewed native import")
	}
	helper := "import {getOauthClient,CodeAssistServer} from " + strconv.Quote("data:text/javascript;base64,"+base64.StdEncoding.EncodeToString([]byte(native))) + ";\n" + rest
	for _, scenario := range []string{"success", "missing-tier", "missing-project", "quota-fails", "persistence-fails"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			eventsFile := filepath.Join(t.TempDir(), "events.json")
			cmd := exec.CommandContext(ctx, node, "--input-type=module", "-e", helper)
			cmd.Env = []string{"USAGE_CASE=" + scenario, "USAGE_EVENTS=" + eventsFile}
			var output bytes.Buffer
			cmd.Stdout = &output
			err := cmd.Run()
			if ctx.Err() != nil || (err == nil) != (scenario == "success") {
				t.Fatalf("helper exit: %v, context: %v", err, ctx.Err())
			}
			var quota geminiQuotaResponse
			if json.Unmarshal(output.Bytes(), &quota) != nil || strings.Contains(output.String(), "private native detail") {
				t.Fatalf("unsafe or malformed helper output: %q", output.String())
			}
			if scenario == "success" && (quota.Tier != "Pro" || len(quota.Buckets) != 1) {
				t.Fatalf("quota = %+v", quota)
			}
			if scenario != "success" && scenario != "persistence-fails" && quota.Error != "quota lookup unavailable" {
				t.Fatalf("native failure was not sanitized: %+v", quota)
			}
			data, err := os.ReadFile(eventsFile)
			if err != nil || !strings.Contains(string(data), `"persist"`) {
				t.Fatalf("helper exited before native persistence: %q, %v", data, err)
			}
			if (scenario == "missing-tier" || scenario == "missing-project") && strings.Contains(string(data), `"quota"`) {
				t.Fatal("helper queried quota without existing Code Assist authority")
			}
		})
	}
}
