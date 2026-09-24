package box

import "testing"

func TestAgentIdentitySeparatesNativeLinuxVolumesAndGatewayUser(t *testing.T) {
	defaultUser := agentIdentity{1000, 1000}
	linuxUser := agentIdentity{1001, 1002}
	if got := defaultUser.volume("coop-cache"); got != "coop-cache" {
		t.Fatal(got)
	}
	if got := linuxUser.volume("coop-cache"); got != "coop-cache-1001-1002" {
		t.Fatal(got)
	}
	if got := linuxUser.projectImage("coop-myrepo"); got != "coop-myrepo-u1001g1002" {
		t.Fatal(got)
	}
	for _, id := range []agentIdentity{{0, 0}, {65532, 1002}, {1001, 0}} {
		if id.valid() {
			t.Errorf("invalid box identity accepted: %+v", id)
		}
	}
	if !linuxUser.valid() || linuxUser.user() != "1001:1002" {
		t.Fatal("ordinary Linux identity rejected", linuxUser)
	}
}
