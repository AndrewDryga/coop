//go:build linux

package processidentity

import "testing"

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
