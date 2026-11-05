package processidentity

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Every process starts after its host boots, this test's included. A boot time that read as now or
// as the epoch would let a sweep take a live run's files, or none.
func TestBootTimeIsInThePast(t *testing.T) {
	boot, ok := BootTime()
	if !ok {
		t.Fatal("this host's boot time could not be read")
	}
	if !boot.Before(time.Now()) || boot.Year() < 2000 {
		t.Fatalf("boot time = %v", boot)
	}
}

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

// Every kind of pid and token Inspect can be handed, against a stand-in kernel. A pid of 1 or below,
// a token of no known shape and another system's token are decided before the kernel is asked
// anything: even when it would say the pid is missing, a record that cannot name the kernel it was
// read on is Unknown, never Gone. Only this kernel's own answer makes a pid Gone or Mismatch.
func TestInspectEveryInputClass(t *testing.T) {
	native := map[string]string{"linux-proc-v2:": "linux-proc-v2:7:boot:7", "darwin-kinfo-v1:": "darwin-kinfo-v1:7:7"}[platformTokenPrefix]
	foreign := map[string]string{"linux-proc-v2:": "darwin-kinfo-v1:7:7", "darwin-kinfo-v1:": "linux-proc-v2:1:boot:7"}[platformTokenPrefix]
	missing, refused := syscall.ESRCH, syscall.EPERM
	for _, tc := range []struct {
		name    string
		pid     int
		token   string
		answers []error // what kill(pid, 0) returns, call by call
		current string  // the start token the kernel reads for pid now
		want    State
		asked   int // kill(pid, 0) calls Inspect makes
	}{
		{name: "pid 1", pid: 1, token: native, answers: []error{nil}, current: native, want: Unknown},
		{name: "pid 0", pid: 0, token: native, answers: []error{nil}, current: native, want: Unknown},
		{name: "pid -1, kill's every-process target", pid: -1, token: native, answers: []error{nil}, current: native, want: Unknown},
		{name: "no token, pid missing", pid: 4242, token: "", answers: []error{missing}, want: Unknown},
		{name: "no token, pid live", pid: 4242, token: "", answers: []error{nil}, current: native, want: Unknown},
		{name: "token of no known shape, pid missing", pid: 4242, token: "homemade-v9:1", answers: []error{missing}, want: Unknown},
		{name: "another system's token, pid missing", pid: 4242, token: foreign, answers: []error{missing}, want: Foreign},
		{name: "another system's token, pid live", pid: 4242, token: foreign, answers: []error{nil}, current: native, want: Foreign},
		{name: "pid missing", pid: 4242, token: native, answers: []error{missing}, want: Gone, asked: 1},
		{name: "kill refused, start token unreadable", pid: 4242, token: native, answers: []error{refused, refused}, want: Unknown, asked: 2},
		{name: "kill refused, pid reused by another user", pid: 4242, token: native, answers: []error{refused}, current: native + "0", want: Mismatch, asked: 1},
		{name: "kill refused, same process", pid: 4242, token: native, answers: []error{refused}, current: native, want: Match, asked: 1},
		{name: "kill failed otherwise", pid: 4242, token: native, answers: []error{syscall.EINVAL}, current: native + "0", want: Unknown, asked: 1},
		{name: "start token unreadable, then the pid is missing", pid: 4242, token: native, answers: []error{nil, missing}, want: Gone, asked: 2},
		{name: "start token unreadable, pid still live", pid: 4242, token: native, answers: []error{nil, nil}, want: Unknown, asked: 2},
		{name: "start token unreadable, then kill refused", pid: 4242, token: native, answers: []error{nil, refused}, want: Unknown, asked: 2},
		{name: "pid reused", pid: 4242, token: native, answers: []error{nil}, current: native + "0", want: Mismatch, asked: 1},
		{name: "same process", pid: 4242, token: native, answers: []error{nil}, current: native, want: Match, asked: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := 0
			kernel := Kernel{
				Signal: func(pid int, sig syscall.Signal) error {
					if pid != tc.pid || sig != 0 || asked >= len(tc.answers) {
						t.Fatalf("kill(%d, %d) as call %d, want kill(%d, 0) at most %d times", pid, sig, asked+1, tc.pid, len(tc.answers))
					}
					asked++
					return tc.answers[asked-1]
				},
				StartToken: func(pid int) string {
					if pid != tc.pid {
						t.Fatalf("start token read for pid %d, want %d", pid, tc.pid)
					}
					return tc.current
				},
				Namespace: func() (string, bool) { return "pid:[7]", true }, // the one native names on Linux
			}
			if got := kernel.Inspect(tc.pid, tc.token); got != tc.want || asked != tc.asked {
				t.Fatalf("Inspect(%d, %q) = %v after %d kill calls, want %v after %d", tc.pid, tc.token, got, asked, tc.want, tc.asked)
			}
		})
	}
}

// A token the other operating system wrote names a pid this kernel cannot check, so it is Foreign
// whether that pid is live here or missing: never Gone, Mismatch or Match.
func TestAnotherSystemsTokenIsForeign(t *testing.T) {
	own := StartToken(os.Getpid())
	foreign := map[string]string{"linux-proc-v2:": "darwin-kinfo-v1:1:1", "darwin-kinfo-v1:": "linux-proc-v2:1:boot:1"}[platformTokenPrefix]
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
	for token, want := range map[string]string{"linux-proc-v1:b:1": "", "linux-proc-v2:1:b:1": "Linux", "darwin-kinfo-v1:1:1": "macOS", "homemade-v9:1": ""} {
		if got := System(token); got != want {
			t.Errorf("System(%q) = %q, want %q", token, got, want)
		}
	}
}

// Linux names this process's PID namespace, the same way every time; macOS has one pid space per
// host and answers with an empty name.
func TestPIDNamespace(t *testing.T) {
	namespace, ok := PIDNamespace()
	if !ok {
		t.Fatal("the PID namespace of this process is unreadable")
	}
	again, _ := PIDNamespace()
	if again != namespace {
		t.Fatalf("PID namespace changed from %q to %q", namespace, again)
	}
	switch runtime.GOOS {
	case "linux":
		if !strings.HasPrefix(namespace, "pid:[") || !strings.HasSuffix(namespace, "]") {
			t.Fatalf("PID namespace = %q, want pid:[<inode>]", namespace)
		}
	case "darwin":
		if namespace != "" {
			t.Fatalf("PID namespace = %q, want none on macOS", namespace)
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

// Only a pid that is gone, or now another process, proves the recorded process gone; every caller
// that deletes a record or reaps a box on that proof shares this one rule.
func TestOnlyGoneAndMismatchProveGone(t *testing.T) {
	for state, want := range map[State]bool{Gone: true, Mismatch: true, Match: false, Unknown: false, Foreign: false} {
		if got := state.ProvesGone(); got != want {
			t.Errorf("%v.ProvesGone() = %v, want %v", state, got, want)
		}
	}
}

// Two tokens name one process when what the kernel says about it matches; two namespaces never do,
// and a token of no known shape, a v1 Linux token included, names none, even beside itself.
func TestSameComparesTheProcess(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"linux-proc-v2:4026531836:boot:42", "linux-proc-v1:boot:42", false},
		{"linux-proc-v1:boot:42", "linux-proc-v1:boot:42", false},
		{"linux-proc-v2:4026531836:boot:42", "linux-proc-v2:4026531836:boot:42", true},
		{"linux-proc-v2:4026531836:boot:42", "linux-proc-v2:4026532000:boot:42", false},
		{"linux-proc-v1:boot:42", "linux-proc-v1:boot:43", false},
		{"darwin-kinfo-v1:1:2", "darwin-kinfo-v1:1:2", true},
		{"", "", false},
	} {
		if got := Same(tc.a, tc.b); got != tc.want {
			t.Errorf("Same(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	if System("linux-proc-v2:1:boot:42") != "Linux" || !Stable("linux-proc-v2:1:boot:42") {
		t.Error("a v2 Linux token is not a stable Linux token")
	}
}

// A Linux v1 token names no PID namespace. It is unverifiable on every platform: Unknown for a live
// pid and for a missing one, never a match and never proof the process is gone.
func TestLinuxV1TokenIsUnverifiable(t *testing.T) {
	v1 := "linux-proc-v1:boot:42"
	if Stable(v1) || Native(v1) || System(v1) != "" || Same(v1, v1) {
		t.Fatalf("v1 token: stable %v, native %v, system %q, same as itself %v; want none", Stable(v1), Native(v1), System(v1), Same(v1, v1))
	}
	for _, pid := range []int{os.Getpid(), 1 << 30} {
		if got := Inspect(pid, v1); got != Unknown || got.ProvesGone() {
			t.Errorf("Inspect(%d, %q) = %v, want unknown", pid, v1, got)
		}
	}
}
