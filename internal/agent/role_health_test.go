package agent

import (
	"regexp"
	"testing"
)

// The wrappers' login check puts each adapter's AuthSignals inside a single-quoted extended regular
// expression with regexp metacharacters escaped. Keep them lowercase and forbid characters
// that would break the surrounding single quotes or introduce another line.
func TestAuthSignalsRenderIntoTheWrapperCheck(t *testing.T) {
	safe := regexp.MustCompile(`^[a-z0-9 _/:,·.()-]+$`)
	for _, name := range Names() {
		a, _ := Get(name)
		for _, signal := range a.LiveCredentials().AuthSignals {
			if !safe.MatchString(signal) {
				t.Errorf("%s AuthSignal %q cannot be rendered into the wrappers' check", name, signal)
			}
		}
	}
}
