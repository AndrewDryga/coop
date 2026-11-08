package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
)

// No session's box may reach the host through the editor, whatever its network mode, and only the
// supervisor knows whether the network is restricted, so every supervisor has to say both. A new one
// that forgets gets an open channel.
func TestEverySupervisorClosesTheEditorChannel(t *testing.T) {
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
				if !strings.Contains(line, "NoEditorHostAccess: true, RestrictedNetwork: a.acpRestrictedNetwork(),") {
					t.Errorf("%s: this supervisor leaves the editor channel open or ignores its network mode: %s", source, strings.TrimSpace(line))
				}
			}
		}
	}
	if found < 2 {
		t.Errorf("found %d supervisors, want coop acp and coop fork acp", found)
	}
}

// Admission stores offline as "none". An offline session is restricted like a filtered one, which has a
// capture; an open one is not.
func TestOfflineAndFilteredSessionsRestrictTheEditorsNetwork(t *testing.T) {
	for _, c := range []struct {
		egress  string
		capture bool
		want    bool
	}{{"none", false, true}, {"filtered", true, true}, {"open", false, false}, {"", false, false}} {
		a := &app{cfg: &config.Config{Egress: c.egress}}
		if c.capture {
			a.acpCapture = &box.CapturedEgress{}
		}
		if got := a.acpRestrictedNetwork(); got != c.want {
			t.Errorf("egress %q (capture %v): restricted = %v, want %v", c.egress, c.capture, got, c.want)
		}
	}
}
