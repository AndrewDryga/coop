package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQueueHasTodo(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "TASKS.md")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const legend = "# [ ] todo   [w] claimed   [x] done   [B] blocked\n"

	// The legend documents "[ ]" and the # Example block uses [E]; neither is work.
	if queueHasTodo(write(legend + "\n# Example\n- [E] sample task\n\n## Active\n")) {
		t.Error("legend + [E] example must not count as a todo")
	}
	// Claimed/done/blocked items aren't open todos either.
	if queueHasTodo(write(legend + "## Active\n- [x] done\n- [w] in progress\n- [B] blocked\n")) {
		t.Error("[x]/[w]/[B] must not count as a todo")
	}
	// A real unclaimed task does.
	if !queueHasTodo(write(legend + "## Active\n- [ ] do the thing\n")) {
		t.Error("an open - [ ] task should count")
	}
	// A missing file is not a todo.
	if queueHasTodo(filepath.Join(t.TempDir(), "nope.md")) {
		t.Error("a missing queue should be false")
	}
}
