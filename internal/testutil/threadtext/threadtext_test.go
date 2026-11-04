package threadtext

import (
	"fmt"
	"strings"
	"testing"
)

// recorder is a testing.TB that keeps a failure instead of failing the test that provoked it.
type recorder struct {
	testing.TB
	failures []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func TestCheckFindsSentencesInCodeBlocks(t *testing.T) {
	block := func(line string) string { return "**Title.**\n\nRun this:\n\n```\n" + line + "\n```" }
	for name, tc := range map[string]struct {
		line string
		bad  bool
	}{
		"a command":             {line: "coop build --egress filtered"},
		"rules to paste":        {line: "  allow: [example.com, api.example.com]"},
		"a long path":           {line: "cat " + strings.Repeat("/very/long/path", 10)},
		"a long command":        {line: "docker run --rm -e A=b -e C=d " + strings.Repeat("x", 80)},
		"a stop at the end":     {line: "Fix this.", bad: true},
		"a colon at the end":    {line: "Run this:", bad: true},
		"a long line of prose":  {line: ".agent/project.yaml: yaml: line 1: did not find expected node content", bad: true},
		"prose cut off with …":  {line: strings.Repeat("did not find expected key ", 30) + "…", bad: true},
		"short words, no stops": {line: "did not find key"},
	} {
		t.Run(name, func(t *testing.T) {
			r := &recorder{TB: t}
			Check(r, block(tc.line))
			if bad := len(r.failures) > 0; bad != tc.bad {
				t.Fatalf("Check(%q) failures = %q, want a failure: %v", tc.line, r.failures, tc.bad)
			}
		})
	}
}
