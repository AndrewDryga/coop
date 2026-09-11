package cli

import (
	"strings"
	"testing"
)

// `coop help credentials codex personal rm` is the spelling a person types: the agent and the
// account sit between the family and its verb. All three account pages resolve on the last word,
// whichever account is named, so the page a reader reaches matches the command they ran.
func TestAccountHelpResolvesOnTheSpellingPeopleType(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"credentials codex personal", "coop credentials <agent> <account> — show an account"},
		{"credentials codex personal default", "coop credentials <agent> <account> default — choose the default account"},
		{"credentials claude work rm", "coop credentials <agent> <account> rm — remove a saved account"},
	} {
		out := captureStdout(t, func() {
			if code, err := helpForPath(strings.Fields(tc.path), freshConfig(t), true); code != 0 || err != nil {
				t.Fatalf("coop help %s = (%d, %v)", tc.path, code, err)
			}
		})
		if !strings.Contains(out, tc.want) {
			t.Errorf("coop help %s should print %q, got:\n%s", tc.path, tc.want, out)
		}
	}
}
