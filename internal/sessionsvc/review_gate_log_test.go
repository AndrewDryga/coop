package sessionsvc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewGateLogKeepsTheExactPrefixAndReportsTheLimit(t *testing.T) {
	previous := maxReviewGateOutputBytes
	maxReviewGateOutputBytes = 1024
	t.Cleanup(func() { maxReviewGateOutputBytes = previous })
	path := filepath.Join(t.TempDir(), "gate.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	log := &reviewGateLog{file: file}
	printed := strings.Repeat("x", 2000)
	if n, err := log.Write([]byte(printed)); n != 2000 || err != nil {
		t.Fatalf("first write = %d, %v", n, err)
	}
	if n, err := log.Write([]byte("after the cap")); n != 13 || err != nil {
		t.Fatalf("later write = %d, %v", n, err)
	}
	output := log.finish(ReviewGateResult{}, nil)
	if output.Bytes != 1024 || output.Complete || output.Lost != "" || output.Incomplete != "The check printed more than 1 KiB; Coop kept the first 1 KiB." {
		t.Fatalf("limited output = %+v", output)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != strings.Repeat("x", 1024) {
		t.Fatalf("kept prefix = %q, %v", body, err)
	}
}

func TestReviewGateRuneCutUsesLiteralCharacterBoundaries(t *testing.T) {
	for _, tc := range []struct {
		page string
		cut  int
	}{
		{"ab\xe2", 2}, {"ab\xe2\x9c", 2}, {"ab✗", 5},
		{"\xe2\x9c", 2}, {"ab\xff", 3}, {"abcd", 4},
	} {
		if got := reviewGateOutputRuneCut([]byte(tc.page)); got != tc.cut {
			t.Errorf("cut(%q) = %d, want %d", tc.page, got, tc.cut)
		}
	}
}
