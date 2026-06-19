package cli

import (
	"slices"
	"strings"
	"testing"
)

func TestPromptGateLangs(t *testing.T) {
	// Recognized tokens are kept in order (deduped); unknown ignored.
	if got := promptGateLangs(strings.NewReader("terraform go go bogus\n")); !slices.Equal(got, []string{"terraform", "go"}) {
		t.Errorf("prompt = %v, want [terraform go]", got)
	}
	// Blank / unknown-only / no input → nil (no gate imposed).
	for _, in := range []string{"\n", "nonsense\n", ""} {
		if got := promptGateLangs(strings.NewReader(in)); got != nil {
			t.Errorf("%q → %v, want nil", in, got)
		}
	}
}
