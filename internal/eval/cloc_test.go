package eval

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Real cloc output (2.10), captured — the parser is pinned to the shape the tool actually emits.
const clocJSON = `{"header":{"cloc_version":"2.10","n_files":2},
"Go":{"nFiles":1,"blank":2,"comment":3,"code":10},
"Markdown":{"nFiles":1,"blank":0,"comment":0,"code":5},
"SUM":{"blank":2,"comment":3,"code":15,"nFiles":2}}`

func TestParseClocJSONDropsSumAndHeader(t *testing.T) {
	m, err := parseClocJSON([]byte(clocJSON))
	if err != nil {
		t.Fatal(err)
	}
	if m.ClocVersion != "2.10" {
		t.Errorf("version = %q", m.ClocVersion)
	}
	if len(m.Languages) != 2 { // SUM and header excluded
		t.Errorf("languages = %v", m.Languages)
	}
	if m.Languages["Go"].Code != 10 || m.Languages["Go"].Comment != 3 || m.Languages["Go"].Files != 1 {
		t.Errorf("Go = %+v", m.Languages["Go"])
	}
	if m.TotalCode() != 15 || m.TotalComment() != 3 || m.TotalBlank() != 2 {
		t.Errorf("totals: code=%d comment=%d blank=%d", m.TotalCode(), m.TotalComment(), m.TotalBlank())
	}
}

func TestNetCodeGrowth(t *testing.T) {
	// Net growth is after-minus-before totals, NOT the diff's numbers: a write-then-revert has
	// churn but zero net growth.
	before := SizeMetrics{Languages: map[string]LangCount{"Go": {Code: 100}}}
	after := SizeMetrics{Languages: map[string]LangCount{"Go": {Code: 100}}}
	if NetCodeGrowth(before, after) != 0 {
		t.Errorf("write-then-revert net growth = %d, want 0", NetCodeGrowth(before, after))
	}
	grew := SizeMetrics{Languages: map[string]LangCount{"Go": {Code: 104}}}
	if NetCodeGrowth(before, grew) != 4 {
		t.Errorf("net growth = %d, want 4", NetCodeGrowth(before, grew))
	}
}

func TestParseClocJSONEmptyAndBad(t *testing.T) {
	if m, err := parseClocJSON([]byte("{}")); err != nil || len(m.Languages) != 0 {
		t.Errorf("empty cloc output {}: %+v %v", m, err)
	}
	if m, err := parseClocJSON([]byte("  ")); err != nil || len(m.Languages) != 0 {
		t.Errorf("whitespace cloc output: %+v %v", m, err)
	}
	if _, err := parseClocJSON([]byte("not json")); err == nil {
		t.Error("malformed cloc output was accepted")
	}
}

// End to end against the real pinned tool, when present.
func TestMeasureSizeWithRealCloc(t *testing.T) {
	if _, err := exec.LookPath("cloc"); err != nil {
		t.Skip("cloc not installed")
	}
	a := t.TempDir()
	if err := os.WriteFile(filepath.Join(a, "x.go"), []byte("package x\n// c\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := MeasureSize(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if m.Languages["Go"].Code != 2 {
		t.Errorf("real cloc Go code = %d, want 2", m.Languages["Go"].Code)
	}
	b := t.TempDir()
	if err := os.WriteFile(filepath.Join(b, "x.go"), []byte("package x\n// c\nfunc F() {}\nfunc G() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := MeasureSize(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if growth := NetCodeGrowth(m, after); growth != 1 {
		t.Errorf("real cloc net growth = %d, want 1", growth)
	}
}

// A candidate's symlink is never followed or counted — it is recorded as skipped, so it can neither
// escape the workspace nor steer the size. Two identical files count per path (--skip-uniqueness).
func TestMeasureSizeSkipsSymlinksAndCountsPerPath(t *testing.T) {
	if _, err := exec.LookPath("cloc"); err != nil {
		t.Skip("cloc not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("package a\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "peek.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	m, err := MeasureSize(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Languages["Go"].Files != 2 {
		t.Errorf("Go files = %d, want 2 (both paths, symlink excluded)", m.Languages["Go"].Files)
	}
	skipped := false
	for _, s := range m.Skipped {
		if s == "peek.go" {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("the symlink was not recorded as skipped: %v", m.Skipped)
	}
}

// A missing or non-directory input is an error, never a silent zero.
func TestMeasureSizeRefusesAMissingDir(t *testing.T) {
	if _, err := MeasureSize(context.Background(), filepath.Join(t.TempDir(), "no-such")); err == nil {
		t.Error("measuring a missing directory returned no error")
	}
}
