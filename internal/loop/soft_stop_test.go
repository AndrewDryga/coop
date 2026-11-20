package loop

import (
	"strings"
	"testing"
)

// The first Ctrl-C's notice names what the run does next for each kind of pass: a work iteration
// still gets its own review; a fork's rebase or check pass, and its candidate review, stop once they
// finish, with no review or rework started after them.
func TestSoftStopWordsSayWhatEachPassDoes(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec RunSpec
		want string
	}{
		{"work", RunSpec{}, "Finishing this attempt and its review, then stopping."},
		{"rebase", RunSpec{Rebase: &RebaseSpec{}}, "Finishing this rebase attempt, then stopping before its review."},
		{"check", RunSpec{Rebase: &RebaseSpec{CheckOnly: true}}, "Finishing this check, then stopping before the review."},
		{"review", RunSpec{CandidateReview: &CandidateReviewSpec{}}, "Finishing this review, then stopping."},
	} {
		finishing, finishes := softStopWords(tc.spec)
		if finishing != tc.want || !strings.HasPrefix(finishes, "Ctrl-C finishes this") {
			t.Errorf("%s: %q / %q; want %q", tc.name, finishing, finishes, tc.want)
		}
	}
}
