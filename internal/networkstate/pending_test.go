package networkstate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

// The one pending check: what the file asks for, compared exactly with what a
// human approved. Without an approval only a widening needs one; with one, the
// mode, the rules and the service identities must all match.
func TestPendingApprovalIsTheExactRequest(t *testing.T) {
	filtered, none, open := admissionMode(egress.Filtered), admissionMode(egress.None), admissionMode(egress.Open)
	a, b := rule("a.example.com"), rule("b.example.com")
	service := egress.Rule{To: egress.Destination{Service: "db"}, Protocol: "tcp", Ports: []int{5432}}
	approvedFiltered := &Approval{Posture: egress.Filtered, Envelope: []egress.Rule{a}}
	approvedOpen := &Approval{Posture: egress.Open}
	approvedService := &Approval{Posture: egress.Filtered, Envelope: []egress.Rule{service}, Services: map[string]string{"db": strings.Repeat("1", 64)}}
	for name, tc := range map[string]struct {
		input    Admission
		approval *Approval
		want     string // "" for nothing pending, else a fragment of the reason
	}{
		"default project":               {input: Admission{}},
		"fresh filtered":                {input: Admission{ProjectMode: filtered}},
		"fresh offline":                 {input: Admission{ProjectMode: none}},
		"fresh open":                    {input: Admission{ProjectMode: open}, want: "unrestricted internet access"},
		"fresh open under --egress":     {input: Admission{ProjectMode: open, InvocationMode: filtered}},
		"fresh open under COOP_EGRESS":  {input: Admission{ProjectMode: open, HostPreference: none}},
		"fresh open under a policy":     {input: Admission{ProjectMode: open, PolicyMode: filtered}},
		"fresh rules":                   {input: Admission{Requests: []egress.Rule{a}}, want: "has not been approved"},
		"fresh rules under --egress":    {input: Admission{Requests: []egress.Rule{a}, InvocationMode: filtered}, want: "has not been approved"},
		"approved, same":                {input: Admission{Requests: []egress.Rule{a}}, approval: approvedFiltered},
		"approved, same with mode":      {input: Admission{ProjectMode: filtered, Requests: []egress.Rule{a}}, approval: approvedFiltered},
		"approved, reordered dup":       {input: Admission{Requests: []egress.Rule{a, a}}, approval: approvedFiltered},
		"approved, rule swapped":        {input: Admission{Requests: []egress.Rule{b}}, approval: approvedFiltered, want: "has not been approved"},
		"approved, rule added":          {input: Admission{Requests: []egress.Rule{a, b}}, approval: approvedFiltered, want: "has not been approved"},
		"approved, rule removed":        {input: Admission{}, approval: approvedFiltered, want: "has not been approved"},
		"approved, widened to open":     {input: Admission{ProjectMode: open}, approval: approvedFiltered, want: "has not been approved"},
		"approved, narrowed to offline": {input: Admission{ProjectMode: none}, approval: approvedFiltered, want: "has not been approved"},
		"approved open, silent file":    {input: Admission{}, approval: approvedOpen},
		"approved open, file filtered":  {input: Admission{ProjectMode: filtered}, approval: approvedOpen, want: "has not been approved"},
		"approved open, overridden":     {input: Admission{ProjectMode: filtered, InvocationMode: none}, approval: approvedOpen},
		"service, same definition":      {input: Admission{Requests: []egress.Rule{service}, Services: map[string]string{"db": strings.Repeat("1", 64)}}, approval: approvedService},
		"service, rewritten":            {input: Admission{Requests: []egress.Rule{service}, Services: map[string]string{"db": strings.Repeat("2", 64)}}, approval: approvedService, want: `service "db" changed`},
		"service, identity missing":     {input: Admission{Requests: []egress.Rule{service}}, approval: approvedService, want: "has not been approved"},
	} {
		t.Run(name, func(t *testing.T) {
			pending, err := tc.input.pendingApproval(tc.approval)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.want == "" && pending != nil:
				t.Fatalf("pending = %q, want nothing pending", pending.Reason)
			case tc.want != "" && pending == nil:
				t.Fatalf("nothing pending, want %q", tc.want)
			case tc.want != "" && !strings.Contains(pending.Reason, tc.want):
				t.Fatalf("pending = %q, want %q", pending.Reason, tc.want)
			case pending != nil && strings.Contains(pending.Reason, "coop net"):
				t.Fatalf("the reason carries the remedy: %q", pending.Reason)
			}
		})
	}
	if _, err := (Admission{Requests: []egress.Rule{{Protocol: "tls"}}}).pendingApproval(nil); err == nil {
		t.Error("a malformed rule read as a verdict instead of an error")
	}
}

// Host setups serialize: a second process waits for the first instead of
// building and proving the same images beside it, and gives up only when its
// own context does.
func TestLockSetupSerializesAcrossHandles(t *testing.T) {
	s := openStore(t)
	other, err := Open(s.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	held := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.LockSetup(context.Background(), func() error { close(held); <-release; return nil })
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := other.LockSetup(ctx, func() error { t.Error("the lock was taken twice"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting on a held lock = %v, want the caller's deadline", err)
	}
	close(release)
	wg.Wait()
	ran := false
	if err := other.LockSetup(context.Background(), func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("a released lock was not taken: ran=%v err=%v", ran, err)
	}
}
