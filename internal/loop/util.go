package loop

import (
	"strconv"

	"github.com/AndrewDryga/coop/internal/ui"
)

// Keep these stateless helpers local to the loop; exporting generic formatting would create a
// dependency for no shared behavior.

// paintCount renders a count, applying paint only when it's nonzero so a zero stays plain — a
// "0 blocked" shouldn't read as an alarm.
func paintCount(v int, paint func(string) string) string {
	if v > 0 {
		return paint(strconv.Itoa(v))
	}
	return strconv.Itoa(v)
}

// truncate shortens s to n terminal columns (ui.RuneWidth: a CJK character or emoji takes two),
// marking elision with an ellipsis. Much of what it shortens lands on a terminal, the live bar
// first, where a line one column too wide wraps. A zero-width rune still counts one, so the result
// also stays within n runes: callers use it to bound prompt text, which a run of combining marks
// must not slip past.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	cut := truncateCut(s, n)
	if cut == len(s) {
		return s
	}
	if n <= 1 {
		return s[:cut]
	}
	return s[:truncateCut(s, n-1)] + "…"
}

// truncateCut is the byte length of s's longest prefix that fits in n columns, by truncate's count.
func truncateCut(s string, n int) int {
	used := 0
	for i, r := range s {
		used += max(ui.RuneWidth(r), 1)
		if used > n {
			return i
		}
	}
	return len(s)
}
