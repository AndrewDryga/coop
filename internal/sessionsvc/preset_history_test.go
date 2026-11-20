package sessionsvc

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// A warm box can outlive both a completed prompt and a peer it started. A delayed
// report must stay with the worker invocation that opened it.
func TestWarmPeerReportsDoNotCrossPromptBoundaries(t *testing.T) {
	root := t.TempDir()
	roles := []workerproto.JobPresetRole{{Name: "reader", Mode: "consult"}, {Name: "unused", Mode: "consult"}, {Name: "native", Mode: "native"}}
	start := func(id string) (*sessionOutputDirectory, *presetReport) {
		dir, _, err := prepareSessionOutputDirAtRoot(root, id)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = removeSessionOutputDir(dir) })
		report, err := openPresetReport(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = report.file.Close(); _ = report.marker.Close() })
		return dir, report
	}
	write := func(dir *sessionOutputDirectory, report *presetReport, rows string) {
		f, err := os.OpenFile(filepath.Join(dir.path, report.name), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = fmt.Fprint(f, rows); err != nil {
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	first, a := start("turn-1")
	write(first, a, "reader 41 started\nreader 41 failed\nreader 41 success\n")
	got := readPresetReport(first, a, roles, true)
	if got["reader"].status != "answered" || got["reader"].failures != 1 || got["unused"].status != "not_called" || got["native"].status != "unknown" {
		t.Fatalf("first report: %+v", got)
	}
	second, b := start("turn-2")
	write(first, a, "reader 91 started\nreader 91 success\n")
	write(second, b, "reader 56 started\n")
	got = readPresetReport(second, b, roles, false)
	if got["reader"].status != "unknown" || got["unused"].status != "unknown" || a.current(first) {
		t.Fatal("interrupted or previous prompt was reported as complete")
	}
}
