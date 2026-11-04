package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A filtered session's box must not reach the host through the editor, and only the supervisor knows a
// session is filtered, so every supervisor has to say so. A new one that forgets gets an open channel.
func TestEverySupervisorTellsTheProxyWhetherTheSessionIsFiltered(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "acpproxy.Run(") {
				t.Errorf("%s: acpproxy.Run has no way to close the editor channel: %s", source, strings.TrimSpace(line))
			}
			if strings.Contains(line, "acpproxy.RunWith(") {
				found++
				if !strings.Contains(line, "NoEditorHostAccess: a.acpCapture != nil") {
					t.Errorf("%s: this supervisor does not say whether its session is filtered: %s", source, strings.TrimSpace(line))
				}
			}
		}
	}
	if found < 2 {
		t.Errorf("found %d supervisors, want coop acp and coop fork acp", found)
	}
}
