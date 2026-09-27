package loop

import (
	"os"
	"strconv"
)

// Keep these stateless helpers local to the loop; exporting generic formatting or path
// predicates would create a dependency for no shared behavior.

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// paintCount renders a count, applying paint only when it's nonzero so a zero stays plain — a
// "0 blocked" shouldn't read as an alarm.
func paintCount(v int, paint func(string) string) string {
	if v > 0 {
		return paint(strconv.Itoa(v))
	}
	return strconv.Itoa(v)
}

// truncate shortens s to n runes, marking elision with an ellipsis.
func truncate(s string, n int) string {
	if n <= 0 {
		return "" // guards the r[:n-1] / r[:n] negative-index panic on a non-positive width
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
