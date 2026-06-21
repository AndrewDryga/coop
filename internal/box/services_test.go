package box

import (
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// TestAutoUpServices: box.Run auto-starts sibling services only when enabled (COOP_AUTO_UP),
// the box is on the services network, it's online, and the runtime has compose — Apple
// `container` does not.
func TestAutoUpServices(t *testing.T) {
	cases := []struct {
		name    string
		autoUp  bool
		network bool
		egress  string
		rtName  string
		want    bool
	}{
		{"defaults: on, networked, online, docker", true, true, "open", "docker", true},
		{"podman too", true, true, "open", "podman", true},
		{"COOP_AUTO_UP=0 opts out", false, true, "open", "docker", false},
		{"no services network (COOP_NETWORK=0)", true, false, "open", "docker", false},
		{"offline box (COOP_EGRESS=none)", true, true, "none", "docker", false},
		{"Apple container has no compose", true, true, "open", "container", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{AutoUp: c.autoUp, Egress: c.egress}
			spec := RunSpec{Network: c.network}
			if got := autoUpServices(cfg, spec, c.rtName); got != c.want {
				t.Errorf("autoUpServices = %v, want %v", got, c.want)
			}
		})
	}
}
