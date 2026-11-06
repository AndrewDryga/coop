package tasks

import "testing"

// Only a landed or discarded candidate history closes a fork generation to more tasks; every phase
// that still has work to review, publish or land leaves it open.
func TestForkCandidateRoundStatusSaysWhenAGenerationIsClosed(t *testing.T) {
	for _, tc := range []struct {
		phase          string
		closed, landed bool
	}{
		{forkCandidateActive, false, false},
		{forkCandidateSuperseding, false, false},
		{forkCandidateReviewing, false, false},
		{forkCandidateReworking, false, false},
		{forkCandidatePublishing, false, false},
		{forkCandidateLanded, true, true},
		{forkCandidateDiscarded, true, false},
	} {
		status := ForkCandidateRoundStatus{Phase: tc.phase}
		if status.Closed() != tc.closed || status.Landed() != tc.landed {
			t.Errorf("%s: closed=%v landed=%v, want closed=%v landed=%v", tc.phase, status.Closed(), status.Landed(), tc.closed, tc.landed)
		}
	}
}
