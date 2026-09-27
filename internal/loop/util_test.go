package loop

import "testing"

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("truncate(short) = %q, want %q", got, "hello")
	}
	if got := truncate("hello world", 5); got != "hell…" {
		t.Errorf("truncate(long) = %q, want %q", got, "hell…")
	}
	for _, n := range []int{0, -1, -5} {
		if got := truncate("hello", n); got != "" {
			t.Errorf("truncate(%q, %d) = %q, want empty", "hello", n, got)
		}
	}
}

func TestPaintCount(t *testing.T) {
	paint := func(s string) string { return "<" + s + ">" }
	if got := paintCount(0, paint); got != "0" {
		t.Errorf("zero should stay plain, got %q", got)
	}
	if got := paintCount(3, paint); got != "<3>" {
		t.Errorf("nonzero should be painted, got %q", got)
	}
}
