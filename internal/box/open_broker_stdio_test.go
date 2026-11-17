package box

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestOpenBrokerReadinessReleasesItsReader(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		want         string // the error's end; "" when the helper must be ready
	}{
		{"ready", "printf '127.0.0.1\\n'; while read -r line; do :; done", ""},
		{"invalid address", "printf 'invalid\\n'; while read -r line; do :; done", "named no usable address"},
		// The output closes before the exit is seen; that is still a stop, told in the helper's words.
		{"early exit", "echo 'broker: listen failed' >&2; exit 7", "stopped before it was ready: broker: listen failed"},
		{"timeout", "while read -r line; do :; done", "no agent started"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stderr := &tailBuffer{max: 8 << 10}
			helper, err := (runtime.Runtime{Name: "sh"}).StartHelper(stderr, "-c", tc.script)
			if err != nil {
				t.Fatal(err)
			}
			broker := &openBroker{helper: helper, stderr: stderr}
			t.Cleanup(func() {
				if err := broker.stop(runtime.Runtime{Name: "true"}); err != nil {
					t.Errorf("stop: %v", err)
				}
			})
			// No stdout is ever written by the timeout fixture; this is its actual deadline.
			timeout := time.Minute
			if tc.name == "timeout" {
				timeout = 10 * time.Millisecond
			}
			err = broker.waitReadyWithin(timeout)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.HasSuffix(err.Error(), tc.want)) {
				t.Fatalf("readiness = %v, want error ending %q", err, tc.want)
			}
			if _, err := helper.Stdout.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("readiness reader remains open: %v", err)
			}
		})
	}
}
