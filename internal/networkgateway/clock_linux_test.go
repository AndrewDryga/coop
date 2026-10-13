package networkgateway

import (
	"testing"
)

func TestLinuxBootClockQualifiesCallingThread(t *testing.T) {
	clock, err := OpenBootClock()
	if err != nil || !clock.Domain().Valid() {
		t.Fatalf("kernel clock proof unavailable: %v", err)
	}
	if _, err := clock.Now(); err != nil {
		t.Fatal(err)
	}
}
