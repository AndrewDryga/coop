package runtime

import (
	"slices"
	"testing"
)

func TestSELinuxLabelGoesOnlyOnContainerCreation(t *testing.T) {
	for _, tc := range []struct{ in, want []string }{
		{[]string{"run", "--rm", "img"}, []string{"run", selinuxLabel, "--rm", "img"}},
		{[]string{"create", "img"}, []string{"create", selinuxLabel, "img"}},
		{[]string{"container", "create", "--name", "x", "img"}, []string{"container", "create", selinuxLabel, "--name", "x", "img"}},
		{[]string{"container", "run", "img"}, []string{"container", "run", selinuxLabel, "img"}},
		{[]string{"--config", "/c", "--host", "unix:///s", "run", "img"}, []string{"--config", "/c", "--host", "unix:///s", "run", selinuxLabel, "img"}},
		// Not container creation, even when an argument is spelled run or create.
		{[]string{"exec", "box", "run"}, []string{"exec", "box", "run"}},
		{[]string{"container", "inspect", "run"}, []string{"container", "inspect", "run"}},
		{[]string{"volume", "create", "v"}, []string{"volume", "create", "v"}},
		{[]string{"--host", "run", "info"}, []string{"--host", "run", "info"}},
		{nil, nil},
	} {
		if got := withSELinuxLabel(true, tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("%q = %q, want %q", tc.in, got, tc.want)
		}
		if got := withSELinuxLabel(false, tc.in); !slices.Equal(got, tc.in) {
			t.Errorf("without SELinux %q changed to %q", tc.in, got)
		}
	}
	in := []string{"run", "img"}
	_ = withSELinuxLabel(true, in)
	if !slices.Equal(in, []string{"run", "img"}) {
		t.Fatal("the caller's argv was modified")
	}
}

func TestSELinuxDaemonReadsDockerSecurityOptions(t *testing.T) {
	if !selinuxDaemon([]string{"name=seccomp,profile=builtin", "name=selinux"}) || selinuxDaemon([]string{"name=apparmor", "name=seccomp,profile=builtin"}) || selinuxDaemon(nil) {
		t.Fatal("misread Docker's security options")
	}
}
