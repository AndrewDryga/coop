//go:build linux

package processidentity

import (
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestLinuxStartTicks(t *testing.T) {
	valid := "123 (tricky ) command name) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 4242 20"
	for _, tc := range []struct {
		name string
		stat string
		want string
		ok   bool
	}{
		{name: "field 22 after tricky comm", stat: valid, want: "4242", ok: true},
		{name: "missing comm close", stat: "123 command S 1 2", ok: false},
		{name: "short fields", stat: "123 (short) S 1 2", ok: false},
		{name: "nonnumeric start", stat: "123 (bad) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 nope 20", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := linuxStartTicks([]byte(tc.stat))
			if got != tc.want || ok != tc.ok {
				t.Fatalf("linuxStartTicks = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// A pid means something only in the PID namespace it was read in, so a token names that namespace.
// A token from another namespace, a container's sharing the host's fork records or the host's seen
// from a container, is Foreign: here its pid is gone or another process, and acting on that would
// take a live run's state. A token an earlier coop wrote without the namespace is judged as before.
func TestATokenFromAnotherPIDNamespaceIsForeign(t *testing.T) {
	pid := os.Getpid()
	token := StartToken(pid)
	inode, ok := namespaceInode(PIDNamespace())
	if !ok || !strings.HasPrefix(token, linuxTokenV2+inode+":") {
		t.Fatalf("StartToken = %q, want a v2 token naming namespace %s", token, inode)
	}
	identity, _ := kernelIdentity(token)
	old := identity // the v1 shape an earlier coop wrote for this process
	other := linuxTokenV2 + "1" + strings.TrimPrefix(token, linuxTokenV2+inode)
	for _, tc := range []struct {
		name  string
		token string
		want  State
	}{
		{"this namespace", token, Match},
		{"an earlier coop's token", old, Match},
		{"another namespace", other, Foreign},
	} {
		if got := Inspect(pid, tc.token); got != tc.want {
			t.Errorf("Inspect(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
	elsewhere := Kernel{Signal: syscall.Kill, StartToken: StartToken, Namespace: func() (string, bool) { return "pid:[1]", true }}
	if got := elsewhere.Inspect(pid, token); got != Foreign {
		t.Errorf("Inspect from another namespace = %v, want Foreign", got)
	}
	if Origin(other) != "another container or host" || Origin(token) != "Linux" {
		t.Errorf("Origin(other namespace) = %q, Origin(own) = %q", Origin(other), Origin(token))
	}
	if !Same(token, old) || Same(token, other) {
		t.Errorf("Same(v2, v1) = %v, Same(v2, other namespace) = %v; want true, false", Same(token, old), Same(token, other))
	}
}
