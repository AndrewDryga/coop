//go:build linux

package networkgateway

import (
	"strings"
	"testing"
)

func TestServiceRoleRequiresExactCapabilitiesAndSandboxFlags(t *testing.T) {
	status := "CapEff:\t0000000000001000\nCapPrm:\t0000000000001000\nCapBnd:\t0000000000001000\nCapInh:\t0\nCapAmb:\t0\nNoNewPrivs:\t1\nSeccomp:\t2\n"
	if err := checkRoleStatus(status, 1<<12); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(status, "NoNewPrivs:\t1", "NoNewPrivs:\t0", 1),
		strings.Replace(status, "Seccomp:\t2", "Seccomp:\t0", 1),
		strings.Replace(status, "CapEff:\t0000000000001000", "CapEff:\t0000000000001001", 1),
		strings.Replace(status, "CapAmb:\t0\n", "", 1),
	} {
		if checkRoleStatus(bad, 1<<12) == nil {
			t.Fatal("widened/missing role restriction accepted")
		}
	}
	if checkRoleStatus(status, 0) == nil {
		t.Fatal("guard accepted NET_ADMIN")
	}
}
