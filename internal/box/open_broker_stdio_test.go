package box

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestOpenBrokerReadinessReleasesItsReader(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		wantError    bool
	}{
		{"ready", "printf '127.0.0.1\\n'; while read -r line; do :; done", false},
		{"invalid address", "printf 'invalid\\n'; while read -r line; do :; done", true},
		{"early exit", "exit 7", true},
		{"timeout", "while read -r line; do :; done", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helper, err := (runtime.Runtime{Name: "sh"}).StartHelper(io.Discard, "-c", tc.script)
			if err != nil {
				t.Fatal(err)
			}
			broker := &openBroker{helper: helper}
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
			if err := broker.waitReadyWithin(timeout); (err != nil) != tc.wantError {
				t.Fatalf("readiness = %v, want error %t", err, tc.wantError)
			}
			if _, err := helper.Stdout.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("readiness reader remains open: %v", err)
			}
		})
	}
}
