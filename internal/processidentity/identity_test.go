package processidentity

import (
	"os"
	"testing"
)

func TestCurrentProcessIdentity(t *testing.T) {
	token := StartToken(os.Getpid())
	if !Stable(token) {
		t.Fatalf("current process token is not stable: %q", token)
	}
	if got := Inspect(os.Getpid(), token); got != Match {
		t.Fatalf("current process identity = %v, want match", got)
	}
	if got := Inspect(os.Getpid(), token+"-stale"); got != Mismatch {
		t.Fatalf("stale process identity = %v, want mismatch", got)
	}
	if got := Inspect(-1, token); got != Unknown {
		t.Fatalf("invalid pid identity = %v, want unknown", got)
	}
}
