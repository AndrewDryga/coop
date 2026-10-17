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

// A token the other operating system wrote names a pid this kernel cannot check, so it is Foreign
// whether that pid is live here or missing: never Gone, Mismatch or Match.
func TestAnotherSystemsTokenIsForeign(t *testing.T) {
	own := StartToken(os.Getpid())
	foreign := map[string]string{"linux-proc-v1:": "darwin-kinfo-v1:1:1", "darwin-kinfo-v1:": "linux-proc-v1:boot:1"}[platformTokenPrefix]
	if !Native(own) || Native(foreign) || !Stable(foreign) || Native("homemade-v9:1") {
		t.Fatalf("native: own %q = %v, other platform's %q = %v", own, Native(own), foreign, Native(foreign))
	}
	for _, pid := range []int{os.Getpid(), 1 << 30} {
		if got := Inspect(pid, foreign); got != Foreign {
			t.Errorf("Inspect(%d, %q) = %v, want foreign", pid, foreign, got)
		}
	}
	if got := Inspect(1<<30, own); got != Gone {
		t.Errorf("Inspect(impossible pid, own token) = %v, want gone", got)
	}
	for token, want := range map[string]string{"linux-proc-v1:b:1": "Linux", "darwin-kinfo-v1:1:1": "macOS", "homemade-v9:1": ""} {
		if got := System(token); got != want {
			t.Errorf("System(%q) = %q, want %q", token, got, want)
		}
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
