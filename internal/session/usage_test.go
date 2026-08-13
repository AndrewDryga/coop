package session

import "testing"

// A turn's cost has to survive the round trip, and "nothing reported" has to be
// distinguishable from "free". A caller that read a zero as a measurement would
// bill itself for a turn nobody measured.
func TestUsageRecordedDistinguishesAbsenceFromZero(t *testing.T) {
	if (Usage{}).Recorded() {
		t.Error("an unreported usage claims to be recorded")
	}
	for _, usage := range []Usage{
		{InputTokens: 1}, {CachedInputTokens: 1}, {OutputTokens: 1}, {ReasoningTokens: 1},
		{CostRecorded: true},
	} {
		if !usage.Recorded() {
			t.Errorf("%+v is a real measurement and reports otherwise", usage)
		}
	}
}
