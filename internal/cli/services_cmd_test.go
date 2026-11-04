package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
)

// The refusal names the folder Coop would not use, and tells the person to change TMPDIR only when
// TMPDIR chose that folder.
func TestComposeRefusalNamesTheExposedFolder(t *testing.T) {
	for _, tc := range []struct {
		fromTMPDIR bool
		want, not  string
	}{
		{true, "The temporary directory /work/tmp is inside a folder exposed to an agent.", "Keep that folder"},
		{false, "Coop's private folder /work/run is inside a folder exposed to an agent.", "TMPDIR"},
	} {
		dir := "/work/run"
		if tc.fromTMPDIR {
			dir = "/work/tmp"
		}
		err := &box.ExposedTempDirError{Dir: dir, FromTMPDIR: tc.fromTMPDIR}
		out := captureStderr(t, func() { reportExit(-1, composeRefusal("Could not start services", "compose.yml", err, "coop up")) })
		if !strings.Contains(out, tc.want) || strings.Contains(out, tc.not) {
			t.Errorf("TMPDIR=%v refusal:\n%s\nwant %q and no %q", tc.fromTMPDIR, out, tc.want, tc.not)
		}
	}
}

// Progress for a wrapper with a hashed name says nothing a person can use; its failure still shows.
func TestDistinctLinesHidesHashedContainerProgress(t *testing.T) {
	var out bytes.Buffer
	d := &distinctLines{w: &out}
	_, _ = d.Write([]byte(" Container coop-fs-438b3f018f2558a35aab-service Running \n" +
		" Container coop-fs-438b3f018f2558a35aab-service Error dependency failed\n" +
		" Container web-1 Running \n"))
	want := " Container coop-fs-438b3f018f2558a35aab-service Error dependency failed\n Container web-1 Running \n"
	if out.String() != want {
		t.Errorf("shown = %q", out.String())
	}
}
