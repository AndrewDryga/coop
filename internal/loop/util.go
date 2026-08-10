package loop

import (
	"os"
	"strconv"
)

// The stateless leaves this package shares with internal/cli, redeclared here rather than exported
// across the boundary — the same call internal/forkctl/util.go, internal/tasks/util.go and
// internal/sessionsvc already make, and for the same reason: a `pathExists` or a `truncate` is not
// an API, and an export would make internal/cli a dependency of everything that formats a line.

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

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
