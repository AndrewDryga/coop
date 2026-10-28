package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/taskmcp"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestSelfUpdateFailureGuidance(t *testing.T) {
	for _, test := range []struct {
		name      string
		err       error
		want      string
		unchanged bool
	}{
		{"check", fmt.Errorf("check: %w", checkError{err: errors.New("offline")}), "again later", false},
		{"download", &installFailure{reason: "download failed", unchanged: true}, "download a fresh copy", true},
		{"replace", &installFailure{reason: "binary could not be replaced", unchanged: true}, "tool that installed it", false},
		{"after replacement", &installFailure{reason: "rebuild failed"}, "tool that installed it", false},
		{"generic", errors.New("failure"), "tool that installed it", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var err error
			out := captureStderr(t, func() { err = selfUpdateFailure(test.err, "coop update --self-only") })
			if !errors.Is(err, ui.ErrReported) || !strings.Contains(out, test.want) || strings.Contains(out, "installed binary was not changed") != test.unchanged {
				t.Fatalf("guidance: %v\n%s", err, out)
			}
		})
	}
}

func TestDoctorTaskProbeEvaluatesRepliesAndCanary(t *testing.T) {
	for _, test := range []struct {
		name             string
		mutate           func(map[string]map[string]any)
		held             string
		passes, failures int
	}{
		{name: "valid", passes: 4},
		{name: "unexpected tool", mutate: func(r map[string]map[string]any) {
			r["list"]["result"] = map[string]any{"tools": []any{map[string]any{"name": "shell"}}}
		}, passes: 3, failures: 1},
		{name: "error and result", mutate: func(r map[string]map[string]any) { r["exec"]["result"] = map[string]any{} }, passes: 3, failures: 1},
		{name: "held write", held: "doctor must not land here", passes: 4, failures: 1},
		{name: "missing own", mutate: func(r map[string]map[string]any) { delete(r, "mine") }, passes: 3, failures: 1},
		{name: "failed own", mutate: func(r map[string]map[string]any) { r["mine"]["result"].(map[string]any)["isError"] = true }, passes: 3, failures: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			tools := []any{}
			for _, name := range taskmcp.ToolNames() {
				tools = append(tools, map[string]any{"name": name})
			}
			result := func(text string, failed bool) map[string]any {
				return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": failed}
			}
			replies := map[string]map[string]any{
				"list": {"result": map[string]any{"tools": tools}}, "exec": {"error": map[string]any{"message": "unsupported"}}, "shell": {"error": map[string]any{"message": "unsupported"}},
				"held": {"result": result("held by another live process", true)}, "mine": {"result": result("appended", false)},
			}
			if test.mutate != nil {
				test.mutate(replies)
			}
			var output strings.Builder
			for _, id := range []string{"list", "exec", "shell", "held", "mine"} {
				if r, ok := replies[id]; ok {
					r["id"] = id
					raw, err := json.Marshal(r)
					if err != nil {
						t.Fatal(err)
					}
					fmt.Fprintf(&output, "REPLY %s\n", raw)
				}
			}
			report := &doctorReport{}
			reportDoctorTaskProbe(report.section("task"), output.String(), "", nil, []byte(test.held))
			got := report.tally()
			if got.passed != test.passes || got.failed != test.failures || got.uncompleted != 0 || got.probeFailed != 0 {
				t.Fatalf("verdict: %+v", got)
			}
		})
	}
	for _, output := range []string{"", "REPLY {broken", "RESULT FAIL task.socket"} {
		report := &doctorReport{}
		reportDoctorTaskProbe(report.section("task"), output, "fixture failure", errors.New("exit 1"), nil)
		got := report.tally()
		if got.probeFailed != 1 || got.uncompleted != 4 || got.passed != 0 {
			t.Fatalf("missing probe %q: %+v", output, got)
		}
	}
}
