package cli

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Every RESULT kind a probe script can print must be one parseProbeResults understands. A kind it
// drops does not show up as a broken probe: the check that reads it sees an empty value and reports
// the box as FAILING, which is what the writable-config-home check did on every host for two months.
func TestProbeKindsAreAllParsed(t *testing.T) {
	scripts := []string{
		doctorProbe,
		doctorCredAndHomeProbe("/home/node"),
		doctorTaskProbe,
	}
	kind := regexp.MustCompile(`RESULT ([A-Z_]+)`)
	seen := map[string]bool{}
	for _, script := range scripts {
		for _, m := range kind.FindAllStringSubmatch(script, -1) {
			seen[m[1]] = true
		}
	}
	if len(seen) == 0 {
		t.Fatal("no RESULT kinds found — the probe scripts changed shape")
	}
	for k := range seen {
		if k == "PASS" || k == "FAIL" {
			continue
		}
		if !slices.Contains(probeMeasurements, k) {
			t.Errorf("probe kind %q is printed but not parsed: add it to probeMeasurements", k)
		}
		got := parseProbeResults("RESULT " + k + " value")
		if got[k] != "value" {
			t.Errorf("parseProbeResults dropped %q: got %v", k, got)
		}
	}
	// The measurement list may not grow stale either: a kind nothing prints is dead weight.
	for _, k := range probeMeasurements {
		if !seen[k] {
			t.Errorf("probeMeasurements lists %q, but no probe prints it", k)
		}
	}
}

// The writable-config-home check reads the probe's own word for it, and nothing else passes it.
func TestConfigHomeCheckReadsTheProbe(t *testing.T) {
	for _, tc := range []struct {
		result string
		want   string
	}{
		{"writable", "The box can write its settings directory"},
		{"blocked", "The box cannot write its settings directory"},
		{"", "The box cannot write its settings directory"},
	} {
		s := &doctorSection{}
		doctorCheckHome(s, tc.result, true)
		if len(s.rows) != 1 || !strings.Contains(s.rows[0].label, tc.want) {
			t.Errorf("doctorCheckHome(%q) = %+v, want one row labeled %q", tc.result, s.rows, tc.want)
		}
	}
}
