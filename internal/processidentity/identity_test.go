package processidentity

import (
	"os"
	"path/filepath"
	"strings"
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

func TestParentAndCommandOfTheCurrentProcess(t *testing.T) {
	if got, want := Parent(os.Getpid()), os.Getppid(); got != want {
		t.Fatalf("Parent(self) = %d, want %d", got, want)
	}
	name := Command(os.Getpid())
	base := filepath.Base(os.Args[0])
	if name == "" || !strings.HasPrefix(base, name) {
		t.Fatalf("Command(self) = %q, want a prefix of %q", name, base)
	}
	if Parent(1<<30) != 0 || Command(1<<30) != "" {
		t.Fatal("an impossible pid must read as unknown, not as a process")
	}
}
