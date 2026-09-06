package wait

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recorder struct {
	testing.TB
	failed string
}

func (r *recorder) Helper()                           {}
func (r *recorder) Fatalf(format string, args ...any) { r.failed = fmt.Sprintf(format, args...) }

func TestForReturnsWhenTheConditionHoldsAndNamesWhatDidNot(t *testing.T) {
	calls := 0
	For(t, "the third poll", func() bool { calls++; return calls == 3 })
	if calls != 3 {
		t.Fatalf("condition polled %d times, want 3", calls)
	}
	path := filepath.Join(t.TempDir(), "marker")
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = os.WriteFile(path, nil, 0o600)
	}()
	ForFile(t, path)

	old := deadline
	deadline = 30 * time.Millisecond
	defer func() { deadline = old }()
	r := &recorder{}
	For(r, "a marker nobody writes", func() bool { return false })
	if !strings.Contains(r.failed, "a marker nobody writes did not happen within 30ms") {
		t.Fatalf("failure message = %q; want the missing event and the deadline named", r.failed)
	}
}
