package agent

import (
	"regexp"
	"testing"
)

// The wrappers' login check puts each adapter's AuthSignals inside a single-quoted extended regular
// expression, escaping only ".". A signal must therefore be lowercase text with none of the characters
// that would end the quoting or change the pattern's meaning.
func TestAuthSignalsRenderIntoTheWrapperCheck(t *testing.T) {
	safe := regexp.MustCompile(`^[a-z0-9 _/:,·.-]+$`)
	for _, name := range Names() {
		a, _ := Get(name)
		for _, signal := range a.LiveCredentials().AuthSignals {
			if !safe.MatchString(signal) {
				t.Errorf("%s AuthSignal %q cannot be rendered into the wrappers' check", name, signal)
			}
		}
	}
}
