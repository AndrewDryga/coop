package networkstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/egress"
)

// withdraw forgets one project's approval the way `coop net forget` does: read
// the record, then consume it.
func withdraw(t *testing.T, s *Store, project string) {
	t.Helper()
	record, err := s.ApprovalAt(project)
	if err != nil || record.Approval == nil {
		t.Fatalf("ApprovalAt(%s) = %+v, %v", project, record, err)
	}
	removed, err := s.Forget(context.Background(), record)
	if err != nil || !removed {
		t.Fatalf("Forget = (%v, %v)", removed, err)
	}
}

// pendingAfter is what an ordinary launch of this project would find.
func pendingAfter(t *testing.T, s *Store, project string, input Admission) *PendingApproval {
	t.Helper()
	preview, err := PreviewAdmission(s.Path(), project, nil, input)
	if err != nil {
		t.Fatalf("PreviewAdmission: %v", err)
	}
	return preview.Pending
}

// The withdrawal barrier exists because removing an approval is NOT enough to
// fail closed: with the project's YAML gone too, mode resolution falls through
// to coop's built-in default, which is open. This is the matrix the design
// named — YAML removed, no rules, each requested mode, a repeat, a deleted
// checkout, unreadable state, and the approval that lifts it.
func TestWithdrawalBarrierRefusesTheDefaultAfterAForget(t *testing.T) {
	filtered, open, offline := egress.Filtered, egress.Open, egress.None
	cases := []struct {
		name    string
		request Admission // what the project asks for AFTER the withdrawal
		refused bool
		barrier bool // refused BY the withdrawal, not by the request itself
	}{
		// The dangerous one: the YAML is gone, so nothing selects a mode and the
		// default would be unrestricted access nobody approved.
		{"yaml removed", Admission{}, true, true},
		{"filtered with no rules", Admission{ProjectMode: &filtered}, true, true},
		// These two were already pending on their own request; the barrier is
		// not what refuses them, and it must not make them read as something else.
		{"filtered with rules", Admission{ProjectMode: &filtered, Requests: []egress.Rule{rule("docs.example.com")}}, true, false},
		{"open request", Admission{ProjectMode: &open}, true, false},
		// An offline run has nothing to regain, so the barrier does not stand in
		// its way: refusing it would protect nothing.
		{"offline request", Admission{ProjectMode: &offline}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			project := t.TempDir()
			if err := approve(s, project, egress.Filtered, []egress.Rule{rule("approved.example.com")}, nil); err != nil {
				t.Fatal(err)
			}
			withdraw(t, s, project)
			pending := pendingAfter(t, s, project, tc.request)
			if tc.refused && pending == nil {
				t.Fatalf("a withdrawn project still launched %s", tc.name)
			}
			if !tc.refused {
				if pending != nil {
					t.Fatalf("the barrier refused an offline run: %s", pending.Reason)
				}
				return
			}
			if !strings.HasSuffix(pending.Sentence(), ".") {
				t.Errorf("pending cause is not a sentence: %q", pending.Sentence())
			}
			if tc.barrier && !strings.Contains(pending.Reason, "withdrawn") {
				t.Errorf("the withdrawal was reported as %q", pending.Reason)
			}
			// Admit is the other door into authority, and it is barred too.
			if _, err := s.Admit(project, tc.request); err == nil {
				t.Fatal("Admit walked through the withdrawal barrier")
			}
			if _, err := s.Resolve(project, tc.request); err == nil {
				t.Fatal("Resolve walked through the withdrawal barrier")
			}
		})
	}
}

// A second forget is idempotent, and the barrier it left is unchanged.
func TestWithdrawalBarrierSurvivesARepeatedForget(t *testing.T) {
	s := openStore(t)
	project := t.TempDir()
	if err := approve(s, project, egress.Filtered, []egress.Rule{rule("a.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	record, err := s.ApprovalAt(project)
	if err != nil {
		t.Fatal(err)
	}
	withdraw(t, s, project)
	if removed, err := s.Forget(context.Background(), record); err != nil || removed {
		t.Fatalf("a second forget = (%v, %v), want no removal", removed, err)
	}
	if pendingAfter(t, s, project, Admission{}) == nil {
		t.Fatal("a repeated forget cleared the barrier")
	}
}

// The case forget exists for: the checkout is gone, so nothing else can reach
// the record — and the barrier still has to outlive it, because a checkout can
// come back at the same path.
func TestWithdrawalBarrierOutlivesADeletedCheckout(t *testing.T) {
	s := openStore(t)
	project := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := approve(s, project, egress.Filtered, []egress.Rule{rule("gone.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	record, err := s.ApprovalAt(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(project); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.Forget(context.Background(), record); err != nil || !removed {
		t.Fatalf("Forget on a deleted checkout = (%v, %v)", removed, err)
	}
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if pendingAfter(t, s, project, Admission{}) == nil {
		t.Fatal("a recreated checkout inherited the open default after a withdrawal")
	}
}

// A marker coop cannot read is not a marker it may ignore: an unreadable or
// mis-filed barrier fails the read instead of quietly reporting "not withdrawn".
func TestUnreadableWithdrawalFailsClosed(t *testing.T) {
	s := openStore(t)
	project := t.TempDir()
	if err := approve(s, project, egress.Filtered, []egress.Rule{rule("b.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	id, err := s.projectID(project)
	if err != nil {
		t.Fatal(err)
	}
	withdraw(t, s, project)
	path := filepath.Join(s.Path(), withdrawalRecord(id))
	for _, content := range []string{"", "{", `{"version":2,"project_id":"` + id + `"}`, `{"version":1,"project_id":"deadbeef"}`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := PreviewAdmission(s.Path(), project, nil, Admission{}); err == nil {
			t.Fatalf("a withdrawal marker containing %q was ignored", content)
		}
		if _, err := s.Admit(project, Admission{}); err == nil {
			t.Fatalf("Admit ignored a withdrawal marker containing %q", content)
		}
	}
}

// The only thing that lifts the barrier is a fresh human approval — including
// an approval of an empty request, which is how a project whose YAML is gone
// gets moving again.
func TestFreshApprovalClearsTheWithdrawalBarrier(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  egress.Mode
		rules []egress.Rule
	}{
		{"empty request", egress.Filtered, nil},
		{"the same rules as before", egress.Filtered, []egress.Rule{rule("again.example.com")}},
		{"offline", egress.None, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			project := t.TempDir()
			if err := approve(s, project, egress.Filtered, []egress.Rule{rule("again.example.com")}, nil); err != nil {
				t.Fatal(err)
			}
			withdraw(t, s, project)
			if err := approve(s, project, tc.mode, tc.rules, nil); err != nil {
				t.Fatalf("re-approval: %v", err)
			}
			input := Admission{Requests: tc.rules}
			if pending := pendingAfter(t, s, project, input); pending != nil {
				t.Fatalf("the barrier survived a fresh approval: %s", pending.Reason)
			}
			if _, err := s.Admit(project, input); err != nil {
				t.Fatalf("Admit after a fresh approval: %v", err)
			}
			id, err := s.projectID(project)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(s.Path(), withdrawalRecord(id))); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the withdrawal record is still on disk: %v", err)
			}
		})
	}
}

// The barrier is written BEFORE the grant is cleared, so the only state a crash
// between the two can leave is the one that existed a moment earlier: the old
// approval still in force. It never leaves the open default.
func TestWithdrawalIsPersistedBeforeTheGrantIsCleared(t *testing.T) {
	s := openStore(t)
	project := t.TempDir()
	if err := approve(s, project, egress.Filtered, []egress.Rule{rule("c.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	id, err := s.projectID(project)
	if err != nil {
		t.Fatal(err)
	}
	// The interrupted state, written by hand: the marker is down, the approval
	// is not yet gone.
	if err := s.recordWithdrawal(id); err != nil {
		t.Fatal(err)
	}
	if pending := pendingAfter(t, s, project, Admission{Requests: []egress.Rule{rule("c.example.com")}}); pending != nil {
		t.Fatalf("an interrupted withdrawal refused a run its approval still covers: %s", pending.Reason)
	}
	// Finishing the removal is what turns it into a withdrawal.
	record, err := s.ApprovalAt(project)
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := s.Forget(context.Background(), record); err != nil || !removed {
		t.Fatalf("Forget = (%v, %v)", removed, err)
	}
	if pendingAfter(t, s, project, Admission{}) == nil {
		t.Fatal("the completed withdrawal did not fail closed")
	}
}

// A named host policy carries its own authority, so a session it governs is not
// blocked by a project's withdrawal.
func TestWithdrawalBarrierLeavesANamedPolicyAlone(t *testing.T) {
	s := openStore(t)
	project := t.TempDir()
	if err := approve(s, project, egress.Filtered, []egress.Rule{rule("d.example.com")}, nil); err != nil {
		t.Fatal(err)
	}
	withdraw(t, s, project)
	policy := egress.Filtered
	if pending := pendingAfter(t, s, project, Admission{PolicyMode: &policy}); pending != nil {
		t.Fatalf("a named policy was refused by a project withdrawal: %s", pending.Reason)
	}
}
