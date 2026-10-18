package randid

import (
	"regexp"
	"testing"
)

func TestHexIsLowercaseHexOfTheAskedLength(t *testing.T) {
	lowerHex := regexp.MustCompile(`^[0-9a-f]+$`)
	for _, n := range []int{4, 8, 16, 32} {
		if got := Hex(n); len(got) != 2*n || !lowerHex.MatchString(got) {
			t.Fatalf("Hex(%d) = %q", n, got)
		}
	}
	if first, second := Hex(16), Hex(16); first == second {
		t.Fatal("two identifiers came out the same")
	}
}
