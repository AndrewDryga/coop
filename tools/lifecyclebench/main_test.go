package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

// TestMain keeps the package's tests out of the developer's real Coop state.
func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m.Run)) }

func TestRunCommandReportsRealProcessOutcomes(t *testing.T) {
	got, err := runCommand(5*time.Second, "/bin/sh", "-c", "printf stdout; printf stderr >&2; exit 7")
	if err != nil || got.stdout != "stdout" || got.stderr != "stderr" || got.code != 7 {
		t.Fatalf("nonzero process = %+v, %v", got, err)
	}
	if _, err := runCommand(time.Second, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing executable reported success")
	}
	if _, err := runCommand(50*time.Millisecond, "/bin/sh", "-c", "exec sleep 60"); err == nil || !strings.Contains(err.Error(), "did not finish within") {
		t.Fatalf("timed out process = %v", err)
	}
}

func TestPercentile(t *testing.T) {
	// Eight samples is a realistic run, and p95 has to land between the top two rather than snapping
	// to the max — otherwise every p95 is just "the slowest sample" under another name.
	values := []float64{1, 2, 3, 4, 5, 6, 7, 8}
	for fraction, want := range map[float64]float64{0.5: 4.5, 0.95: 7.65} {
		if got := percentile(values, fraction); math.Abs(got-want) > 1e-9 {
			t.Errorf("p%g = %g, want %g", fraction*100, got, want)
		}
	}
	if got := percentile([]float64{2.5}, 0.95); got != 2.5 {
		t.Errorf("one sample: %g", got)
	}
	if got := percentile(nil, 0.5); !math.IsNaN(got) {
		t.Errorf("no samples: %g, want NaN", got)
	}
	if got := percentile([]float64{3, 1, 2}, 0.5); got != 2 {
		t.Errorf("order mattered: %g", got)
	}
}

func TestRedactHidesTheMachineButKeepsTheEvidence(t *testing.T) {
	redacted := redact(home + "/Projects/os/coop failed at /tmp/coop-run-123/state")
	if home != "" && (strings.Contains(redacted, home) || !strings.Contains(redacted, "~")) {
		t.Errorf("the home directory survived: %q", redacted)
	}
	if strings.Contains(redacted, "coop-run-123") {
		t.Errorf("the temp path survived: %q", redacted)
	}
	if strings.Contains(redact("/private/tmp/smoke-repo/x"), "smoke-repo") {
		t.Error("the /private/tmp form survived")
	}
	// A commit or a version is the whole point of retaining the artifact.
	const commit = "commit 8299aa2cdca2f6c111593a0090575b52b458bf93"
	if got := redact(commit); got != commit {
		t.Errorf("redact(%q) = %q", commit, got)
	}
}

func TestReport(t *testing.T) {
	environment := map[string]any{"coop_version": "v1", "runtime": "docker"}
	workspace := map[string]any{"path": "/tmp/workspace", "tracked_files": "3"}
	again := repeat{2, "repeat_start", "/bin/sh"}
	seconds := func(s float64) *float64 { return &s }
	report := func(c *caseResult) string { return renderReport([]*caseResult{c}, environment, workspace, again) }

	t.Run("failed samples are visible, not averaged away", func(t *testing.T) {
		got := report(&caseResult{Name: "repeat_start", Boundary: "start", Samples: []sample{
			{OK: true, Seconds: seconds(1)}, {Detail: "the box never appeared"}}})
		for _, want := range []string{"| repeat_start | start | 1/2 | 2 |", "failed sample: the box never appeared"} {
			if !strings.Contains(got, want) {
				t.Errorf("report lacks %q:\n%s", want, got)
			}
		}
	})
	t.Run("a case with no successes reports no numbers", func(t *testing.T) {
		got := report(&caseResult{Name: "cancelled_stop", Boundary: "stop", Samples: []sample{{Detail: "did not exit"}}})
		row := ""
		for line := range strings.Lines(got) {
			if strings.HasPrefix(line, "| cancelled_stop") {
				row = line
			}
		}
		// Dashes, not a NaN dressed up as a percentile.
		if !strings.Contains(row, "0/1") || strings.Contains(strings.ToLower(row), "nan") || strings.Count(row, "—") != 4 {
			t.Errorf("row = %q", row)
		}
	})
	t.Run("unverified is carried into the report", func(t *testing.T) {
		got := report(&caseResult{Name: "cold_start", Boundary: "start", Unverified: "only the first sample is genuinely cold",
			Samples: []sample{{OK: true, Seconds: seconds(0.5)}}})
		if !strings.Contains(got, "unverified: only the first sample is genuinely cold") {
			t.Error(got)
		}
	})
	t.Run("the repeat command names the workspace", func(t *testing.T) {
		// The same command in a different tree measures a different product.
		got := report(&caseResult{Name: "repeat_start", Boundary: "start"})
		for _, want := range []string{"go run ./tools/lifecyclebench --samples 2 --cases repeat_start", "--repo /tmp/workspace"} {
			if !strings.Contains(got, want) {
				t.Errorf("report lacks %q:\n%s", want, got)
			}
		}
	})
	t.Run("evidence is listed after its sample", func(t *testing.T) {
		got := report(&caseResult{Name: "acp_switch_warm", Boundary: "switch", Samples: []sample{
			{OK: true, Seconds: seconds(0.4), Extra: evidence{{"box", "warm"}, {"ack_seconds", 0.012}, {"agent", map[string]any{"name": "coop"}}}},
			{Detail: "wanted a warm box", Extra: evidence{{"ack_seconds", 1.0}}}}})
		for _, want := range []string{`  - box=warm, ack_seconds=0.012, agent={"name":"coop"}`, "  - failed sample: wanted a warm box (ack_seconds=1.0)"} {
			if !strings.Contains(got, want) {
				t.Errorf("report lacks %q:\n%s", want, got)
			}
		}
	})
}

func TestEnvironmentMetadataIsBestEffort(t *testing.T) {
	collect := func(t *testing.T, info, version func() (result, error)) map[string]any {
		b := &bench{coop: "coop", runtime: "runtime", command: func(timeout time.Duration, name string, args ...string) (result, error) {
			switch line := strings.Join(append([]string{name}, args...), " "); {
			case line == "coop version":
				return result{stdout: "coop v1"}, nil
			case strings.HasPrefix(line, "runtime info --format "):
				if timeout != 60*time.Second {
					t.Errorf("runtime info timeout = %s", timeout)
				}
				return info()
			case line == "runtime --version":
				return version()
			case line == "sysctl -n hw.memsize":
				return result{stdout: strconv.Itoa(8 << 30)}, nil
			default:
				t.Errorf("unexpected metadata command: %s", line)
				return result{}, errors.New("unexpected")
			}
		}}
		return b.collectEnvironment()
	}
	answer := func(res result, err error) func() (result, error) { return func() (result, error) { return res, err } }
	versionV1 := answer(result{stdout: "runtime v1"}, nil)

	t.Run("a missing runtime keeps the rest", func(t *testing.T) {
		got := collect(t, answer(result{}, exec.ErrNotFound), answer(result{}, exec.ErrNotFound))
		want := map[string]any{"runtime_daemon": "unknown", "runtime_version": "unavailable", "coop_version": "coop v1", "runtime": "runtime"}
		for key, value := range want {
			if got[key] != value {
				t.Errorf("%s = %v, want %v", key, got[key], value)
			}
		}
	})
	t.Run("a daemon timeout keeps the runtime version", func(t *testing.T) {
		got := collect(t, answer(result{}, errTimeout), versionV1)
		if got["runtime_daemon"] != "unknown" || got["runtime_version"] != "runtime v1" {
			t.Errorf("daemon %v, version %v", got["runtime_daemon"], got["runtime_version"])
		}
	})
	t.Run("a failed or empty daemon answer is unknown", func(t *testing.T) {
		for _, res := range []result{{stdout: "daemon failed", code: 1}, {}} {
			if got := collect(t, answer(res, nil), versionV1); got["runtime_daemon"] != "unknown" {
				t.Errorf("%+v: daemon %v", res, got["runtime_daemon"])
			}
		}
	})
	t.Run("a daemon answer keeps its redacted first line", func(t *testing.T) {
		got := collect(t, answer(result{stdout: "daemon at " + home + "/runtime\nextra line\n"}, nil), versionV1)
		if got["runtime_daemon"] != "daemon at ~/runtime" {
			t.Errorf("daemon = %v", got["runtime_daemon"])
		}
	})
}

// quietRuntime answers every runtime listing with nothing and every other command with a failure.
func quietRuntime(_ time.Duration, name string, _ ...string) (result, error) {
	if name == "docker" {
		return result{}, nil
	}
	return result{code: 1}, nil
}

// sampled runs the command with one fake case and returns its exit code, its output directory and
// what it said on stderr.
func sampled(t *testing.T, ctx context.Context, command commandFunc, measure func(*bench, context.Context, []string) (sample, error), args ...string) (int, string, string) {
	t.Helper()
	out := t.TempDir()
	var stderr bytes.Buffer
	b := &bench{command: command, log: &stderr, caseTimeout: caseTimeout}
	args = append([]string{"--cases", "fake", "--coop", "/bin/sh", "--repo", t.TempDir(), "--out", out}, args...)
	code := run(ctx, args, b, []benchCase{{"fake", "a fake case", "start", measure, nil}}, io.Discard)
	return code, out, stderr.String()
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSamplingLoop(t *testing.T) {
	// The loop around the cases records a failure as a failure and still writes its output.
	t.Run("a timeout becomes a failed sample, not a crash", func(t *testing.T) {
		code, out, _ := sampled(t, context.Background(), quietRuntime, func(*bench, context.Context, []string) (sample, error) {
			return sample{}, errTimeout
		}, "--samples", "2")
		report := read(t, filepath.Join(out, "report.md"))
		if code != 0 || !strings.Contains(report, "0/2") || !strings.Contains(report, "case exceeded 300s") {
			t.Errorf("exit %d, report:\n%s", code, report)
		}
	})
	t.Run("the output is written and names the workspace", func(t *testing.T) {
		code, out, _ := sampled(t, context.Background(), quietRuntime, func(*bench, context.Context, []string) (sample, error) {
			return measured(500 * time.Millisecond), nil
		}, "--samples", "2")
		var written struct {
			Cases       []struct{ Samples []struct{ Case string } }
			Environment map[string]any
			Workspace   map[string]any
		}
		if err := json.Unmarshal([]byte(read(t, filepath.Join(out, "samples.json"))), &written); err != nil {
			t.Fatal(err)
		}
		if code != 0 || len(written.Cases) != 1 || len(written.Cases[0].Samples) != 2 || written.Cases[0].Samples[0].Case != "fake" {
			t.Errorf("exit %d, cases %+v", code, written.Cases)
		}
		if written.Workspace["tracked_files"] == nil || written.Environment["runtime"] != "docker" {
			t.Errorf("workspace %v, environment %v", written.Workspace, written.Environment)
		}
		if !strings.Contains(read(t, filepath.Join(out, "report.md")), "--repo ") {
			t.Error("the report does not repeat the workspace")
		}
	})
	t.Run("an interrupted run still writes what it collected", func(t *testing.T) {
		ctx, interrupt := context.WithCancel(context.Background())
		defer interrupt()
		calls := 0
		code, out, _ := sampled(t, ctx, quietRuntime, func(*bench, context.Context, []string) (sample, error) {
			if calls++; calls > 1 {
				interrupt()
				return sample{}, errInterrupted
			}
			return measured(250 * time.Millisecond), nil
		}, "--samples", "5")
		report := read(t, filepath.Join(out, "report.md"))
		if code != 1 || !strings.Contains(report, "| fake | start | 1/1 |") || !strings.Contains(report, "interrupted before every case finished") {
			t.Errorf("exit %d, report:\n%s", code, report)
		}
	})
}

func TestRunRefusesWhatItCannotMeasure(t *testing.T) {
	launched := 0
	measure := func(*bench, context.Context, []string) (sample, error) {
		launched++
		return measured(time.Second), nil
	}
	notExecutable := filepath.Join(t.TempDir(), "coop")
	if err := os.WriteFile(notExecutable, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Another run's boxes slow every sample, and what they own is theirs.
	busy := func(_ time.Duration, name string, args ...string) (result, error) {
		if name == "docker" && args[0] == "ps" {
			return result{stdout: "a-loop-in-another-repo\n"}, nil
		}
		return quietRuntime(0, name, args...)
	}
	for _, tc := range []struct {
		name    string
		command commandFunc
		args    []string
		said    string
	}{
		{"an unknown case", quietRuntime, []string{"--cases", "fake,nope"}, "unknown case(s): nope"},
		{"no cases", quietRuntime, []string{"--cases", " , "}, "at least one case"},
		{"no samples", quietRuntime, []string{"--samples", "0"}, "samples must be positive"},
		{"negative samples", quietRuntime, []string{"--samples", "-1"}, "samples must be positive"},
		{"a coop it cannot run", quietRuntime, []string{"--coop", notExecutable}, "is not executable"},
		{"no output directory", quietRuntime, []string{"--out", ""}, "usage:"},
		{"a stray argument", quietRuntime, []string{"extra"}, "usage:"},
		{"a busy runtime", busy, nil, "1 coop container(s) or volume(s) from other runs are present"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, said := sampled(t, context.Background(), tc.command, measure, tc.args...)
			if code != 2 || !strings.Contains(said, tc.said) {
				t.Errorf("exit %d, said %q", code, said)
			}
		})
	}
	if launched != 0 {
		t.Errorf("a refused run measured %d sample(s)", launched)
	}
}

// message parses one line of coop's ACP output the way the client does.
func message(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if json.Unmarshal([]byte(line), &m) != nil {
		return nil
	}
	return m
}

func TestInitializeResultIsTheAnswerToRequestOne(t *testing.T) {
	answer := initializeResult(message(t, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentInfo":{"name":"coop"}}}`))
	if info, _ := answer["agentInfo"].(map[string]any); info["name"] != "coop" {
		t.Errorf("answer = %v", answer)
	}
	// Substring matching used to accept all of these, and a log line that happens to contain the
	// right characters would have been timed as the agent's answer.
	for _, line := range []string{
		`log: sending {"id":1,"result":...}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"id":1,"result":true}}`,
		`{"jsonrpc":"2.0","id":2,"result":{}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32601}}`,
		`{"jsonrpc":"2.0","id":1,"result":"not-an-object"}`,
		"", "not json at all",
	} {
		if got := initializeResult(message(t, line)); got != nil {
			t.Errorf("%q answered initialize: %v", line, got)
		}
	}
}

func TestProviderSwitchEvidence(t *testing.T) {
	options := message(t, `{"configOptions":[
		{"id":"coop_preset","currentValue":"none","options":[{"value":"none"}]},
		{"id":"coop_provider","currentValue":"claude","options":[{"value":"claude"},{"value":"codex"},{"value":"gemini"}]}]}`)
	if lead, choices := providerChoices(options); lead != "claude" || !slices.Equal(choices, []string{"claude", "codex", "gemini"}) {
		t.Errorf("selector read as %q, %q", lead, choices)
	}
	if lead, choices := providerChoices(map[string]any{}); lead != "" || choices != nil {
		t.Errorf("no selector read as %q, %q", lead, choices)
	}

	// The switch is done when the session shows the new provider.
	update := message(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S","update":{
		"sessionUpdate":"config_option_update","configOptions":[
			{"id":"coop_provider","currentValue":"codex","options":[{"value":"codex"}]}]}}}`)
	if !providerUpdate(update, "S", "codex") {
		t.Error("the update was not read as the switch")
	}
	// The same update for another session, or still naming the old provider, is not the switch.
	if providerUpdate(update, "other", "codex") || providerUpdate(update, "S", "gemini") ||
		providerUpdate(map[string]any{"method": "session/update", "params": "S"}, "S", "codex") {
		t.Error("an update that is not the switch was read as it")
	}

	// Which box served it comes from the trace, never the timing.
	trace := strings.Join([]string{
		"12:00:00.001 | spawn box on target=codex preset=",
		"12:00:01.000 | spawn: cold box for gemini@personal",
		"12:00:02.000 | spawn: warm box for codex@work",
		"12:00:02.100 | replay: negotiating codex and restoring 1 session(s) on the restarted box",
		"12:00:02.400 | replay: codex@work is live on codex-acp 0.13.1",
	}, "\n")
	want := map[string]string{"box": "warm", "account": "work", "adapter": "codex-acp 0.13.1", "live_account": "work"}
	if got := switchEvidence(trace, "codex"); !maps.Equal(got, want) {
		t.Errorf("codex evidence = %v", got)
	}
	if got := switchEvidence(trace, "gemini")["box"]; got != "cold" {
		t.Errorf("gemini box = %q", got)
	}
	if got := switchEvidence(trace, "grok"); len(got) != 0 {
		t.Errorf("grok evidence = %v", got)
	}

	// The pool is ready per provider.
	parked := "12:00:00.500 | warm pool: gemini@personal parked"
	if !poolReady(parked, "gemini") || poolReady(parked, "codex") {
		t.Error("pool readiness is not per provider")
	}
}
