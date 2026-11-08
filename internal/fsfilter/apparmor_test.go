package fsfilter

import (
	"strings"
	"testing"
)

// The helper's profile names itself, keeps mounts denied and adds only the bus calls the helper
// makes, one member per rule: apparmor_parser refuses a member list.
func TestAppArmorPolicyAllowsOnlyTheHelpersBusCalls(t *testing.T) {
	policy := AppArmorPolicy()
	if strings.Contains(policy, "@NAME@") || !strings.Contains(policy, "profile "+AppArmorProfile+" ") || !strings.HasPrefix(AppArmorProfile, AppArmorProfilePrefix) {
		t.Fatalf("profile name not applied:\n%s", policy)
	}
	for _, want := range []string{"deny mount,", "deny /sys/kernel/security/** rwklx,", "deny @{PROC}/kcore rwklx,"} {
		if !strings.Contains(policy, want) {
			t.Errorf("missing %q", want)
		}
	}
	var members []string
	for _, line := range strings.Split(policy, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "dbus") {
			continue
		}
		_, member, ok := strings.Cut(line, " member=")
		member, _, _ = strings.Cut(member, " ")
		if !ok || strings.ContainsAny(member, "() ") || !strings.HasPrefix(line, "dbus send bus=system ") {
			t.Errorf("bus rule %q", line)
		}
		members = append(members, member)
	}
	if strings.Join(members, ",") != "Hello,StartTransientUnit,StopUnit" {
		t.Errorf("bus calls = %v", members)
	}
}
