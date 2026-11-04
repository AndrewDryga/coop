package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/ui"
)

type fakeApprovalReview struct {
	before, after *networkstate.Approval
	pending       *networkstate.PendingApproval
	committed     int
	commitErr     error
	discarded     int
}

func (f *fakeApprovalReview) Project() string                { return "/private/tmp/project" }
func (f *fakeApprovalReview) Before() *networkstate.Approval { return f.before }
func (f *fakeApprovalReview) After() *networkstate.Approval  { return f.after }
func (f *fakeApprovalReview) Pending() *networkstate.PendingApproval {
	return f.pending
}
func (f *fakeApprovalReview) Commit(context.Context) error  { f.committed++; return f.commitErr }
func (f *fakeApprovalReview) Discard(context.Context) error { f.discarded++; return nil }

func approvalRule(domain string) egress.Rule {
	return egress.Rule{To: egress.Destination{Domain: domain}, Protocol: "tls", Ports: []int{443}}
}

// The review is one stable diff under two uppercase sections: the access mode,
// then every rule marked as it is — unchanged, new or removed — with one legend
// for the markers and no label column. Its exact bytes, with color off, are the
// approved transcript: the markers and the legend carry the meaning alone.
func TestApprovalReviewIsOneStableDiff(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	review := &fakeApprovalReview{
		before: &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("github.com"), approvalRule("registry.npmjs.org")}},
		after:  &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("github.com"), approvalRule("api.example.com")}}}
	var out bytes.Buffer
	if err := confirmProjectApproval(context.Background(), review, nil, &out, func() bool { return true }); err != nil {
		t.Fatalf("confirmNetApproval: %v", err)
	}
	assertApprovedOutput(t, "21m-net-approve-review", out.String())
	if review.committed != 1 {
		t.Errorf("committed %d times, want 1", review.committed)
	}
	if netApprovePrompt != "Approve this access for new runs and running editor sessions?" ||
		netApproveApproved != "Approved for new runs and running editor sessions" {
		t.Errorf("the question and the answer no longer name everything an approval applies to: %q / %q", netApprovePrompt, netApproveApproved)
	}
	for _, unwanted := range []string{"Remember", "remembered", "(unchanged)", "websites", "Egress", "/private/tmp/project"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("approval review still says %q:\n%s", unwanted, out.String())
		}
	}
}

// A mode change is the same review with a before/after pair under NETWORK
// ACCESS, in the shared mode words.
func TestApprovalReviewShowsTheModeChange(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	review := &fakeApprovalReview{
		before: &networkstate.Approval{Posture: egress.None},
		after:  &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("docs.example.com")}}}
	var out bytes.Buffer
	if err := confirmProjectApproval(context.Background(), review, nil, &out, func() bool { return true }); err != nil {
		t.Fatalf("confirmNetApproval: %v", err)
	}
	assertApprovedOutput(t, "21n-net-approve-mode-change", out.String())
}

func TestApprovalReviewExplainsIdentityOnlyReapproval(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for name, cause := range map[string]string{
		"older approval":  "This project's network access was approved by an older Coop.",
		"replaced folder": "This project folder was replaced after its network access was approved.",
	} {
		t.Run(name, func(t *testing.T) {
			approved := &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("docs.example.com")}}
			review := &fakeApprovalReview{before: approved, after: approved, pending: &networkstate.PendingApproval{Cause: cause}}
			var out bytes.Buffer
			if err := confirmProjectApproval(context.Background(), review, nil, &out, func() bool { return true }); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"Why approval is needed:", cause, "NETWORK RULES\n    docs.example.com:443 · TLS\n"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("identity-only review lacks %q:\n%s", want, out.String())
				}
			}
		})
	}
}

// Additions expand access, so they are yellow; removals are dim; nothing new is
// ever green. The markers carry the same meaning uncolored.
func TestApprovalReviewColorsAdditionsYellowNeverGreen(t *testing.T) {
	review := &fakeApprovalReview{
		before: &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("old.example")}},
		after:  &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("new.example")}}}
	var b bytes.Buffer
	writeNetAccessChange(&b, ui.Colored(), review.before, review.after.Posture, review.after.Envelope)
	text := b.String()
	if !strings.Contains(text, "\x1b[33m  + new.example:443 · TLS\x1b[0m\n") {
		t.Errorf("the addition is not the yellow row:\n%q", text)
	}
	if !strings.Contains(text, "\x1b[2m  - old.example:443 · TLS\x1b[0m\n") {
		t.Errorf("the removal is not a dim row:\n%q", text)
	}
	if !strings.Contains(text, "NETWORK RULES\x1b[0m\x1b[0m \x1b[2m(+ new, - removed)\x1b[0m\n") {
		t.Errorf("the legend is not a dim hint after the heading:\n%q", text)
	}
	if strings.Contains(text, "\x1b[32m") {
		t.Errorf("a review colored something green:\n%q", text)
	}
}

// The mode is always the shared description, and an unrestricted request earns
// its red warning whether or not the mode itself changed.
func TestApprovalReviewExplainsTheModeInHumanWords(t *testing.T) {
	filtered := &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("github.com")}}
	for name, tc := range map[string]struct {
		before *networkstate.Approval
		after  *networkstate.Approval
		want   []string
		absent []string
	}{
		"first approval": {after: &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("github.com")}},
			want: []string{"NETWORK ACCESS\n  Filtered: only approved network traffic is allowed.\n", "NETWORK RULES (+ new)\n  + github.com:443 · TLS\n"}},
		"escalation to open": {before: filtered, after: &networkstate.Approval{Posture: egress.Open},
			want: []string{"  - Filtered: only approved network traffic is allowed.\n  + Unrestricted: nothing is blocked.\n",
				netOpenWarning + "\n", "NETWORK RULES (- removed)\n  - github.com:443 · TLS\n"}},
		"offline": {before: filtered, after: &networkstate.Approval{Posture: egress.None},
			want: []string{"  - Filtered: only approved network traffic is allowed.\n  + Offline: internet access is blocked.\n"}},
		"open unchanged": {before: &networkstate.Approval{Posture: egress.Open}, after: &networkstate.Approval{Posture: egress.Open},
			want:   []string{"NETWORK ACCESS\n  Unrestricted: nothing is blocked.\n", netOpenWarning},
			absent: []string{"NETWORK RULES"}},
	} {
		t.Run(name, func(t *testing.T) {
			var b bytes.Buffer
			writeNetAccessChange(&b, ui.Palette{}, tc.before, tc.after.Posture, tc.after.Envelope)
			for _, want := range tc.want {
				if !strings.Contains(b.String(), want) {
					t.Errorf("review is missing %q:\n%s", want, b.String())
				}
			}
			for _, absent := range append(tc.absent, "open is", "none is", "filtered (", "(unchanged)", "websites") {
				if strings.Contains(b.String(), absent) {
					t.Errorf("review says %q:\n%s", absent, b.String())
				}
			}
		})
	}
}

// The review must be shown BEFORE the question, and a "no" must change
// nothing: a human who declines has decided, not deferred.
func TestApprovalCancelledLeavesNothingBehind(t *testing.T) {
	review := &fakeApprovalReview{after: &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{approvalRule("new.example")}}}
	var out bytes.Buffer
	err := confirmProjectApproval(context.Background(), review, nil, &out, func() bool {
		if out.Len() == 0 {
			t.Error("the confirmation was asked before the review was shown")
		}
		return false
	})
	if err == nil || !strings.Contains(err.Error(), "cancelled — nothing was approved") {
		t.Fatalf("cancelled approval = %v", err)
	}
	if review.committed != 0 {
		t.Error("a declined approval was committed")
	}
	if review.discarded != 1 {
		t.Errorf("a declined review discarded its pending marker %d times, want once", review.discarded)
	}
}

func TestApprovalContextCancellationCannotCommitAnAnswer(t *testing.T) {
	review := &fakeApprovalReview{after: &networkstate.Approval{Posture: egress.Filtered}}
	ctx, cancel := context.WithCancel(context.Background())
	err := confirmProjectApproval(ctx, review, nil, &bytes.Buffer{}, func() bool {
		cancel()
		return true
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled approval = %v, want context cancellation", err)
	}
	if review.committed != 0 {
		t.Fatal("approval committed after its context was cancelled")
	}
}

func TestApprovalRefusesAnEmptyReview(t *testing.T) {
	err := confirmProjectApproval(context.Background(), &fakeApprovalReview{}, nil, &bytes.Buffer{}, func() bool { return true })
	if err == nil {
		t.Fatal("a review with nothing to approve was confirmed")
	}
}

func TestApprovalSurfacesACommitFailure(t *testing.T) {
	review := &fakeApprovalReview{after: &networkstate.Approval{Posture: egress.Filtered},
		commitErr: errors.New("network approval changed after review")}
	err := confirmProjectApproval(context.Background(), review, nil, &bytes.Buffer{}, func() bool { return true })
	if err == nil || !strings.Contains(err.Error(), "changed after review") {
		t.Fatalf("commit failure = %v", err)
	}
}

// There is no --mode: the mode comes from .agent/project.yaml and nowhere else.
func TestApproveRejectsUnrelatedFlags(t *testing.T) {
	a := &app{}
	for _, args := range [][]string{{"--mode", "open"}, {"--mode=none"}, {"--yes"}, {"extra"}} {
		if code, err := a.cmdApprove(args); code != 2 || err == nil {
			t.Errorf("cmdApprove(%q) = (%d, %v), want a usage error", args, code, err)
		}
	}
	if code, err := a.cmdApprove([]string{"--case-exposure", "--case-exposure"}); code != 2 || err == nil {
		t.Fatalf("repeated flag = %d, %v", code, err)
	}
	if !ui.IsTerminal(os.Stdin) {
		if code, err := a.cmdApprove([]string{"--case-exposure"}); code != 1 || err == nil || !strings.Contains(err.Error(), "terminal") {
			t.Fatalf("explicit file approval bypassed terminal requirement: %d, %v", code, err)
		}
	}
	if code, err := a.dispatch([]string{"approve", "extra"}); code != 2 || err == nil {
		t.Errorf("dispatch coop approve extra = (%d, %v), want a usage error", code, err)
	}
}

func TestApproveDoesNotRequestCaseExposure(t *testing.T) {
	for _, networkChange := range []bool{false, true} {
		t.Run(fmt.Sprint(networkChange), func(t *testing.T) {
			netHostFixture(t)
			state := filepath.Join(t.TempDir(), "private-state")
			t.Setenv(box.ServiceStateRootEnv, state)
			repo := netFixtureProject(t)
			if err := os.WriteFile(filepath.Join(repo, "credentials.json"), []byte("inert"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(repo, "CREDENTIALS.JSON")); err != nil {
				t.Skip("requires a disk that folds case")
			}
			if networkChange {
				if err := os.Mkdir(filepath.Join(repo, ".agent"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, ".agent/project.yaml"), []byte("box:\n  egress: open\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			a := &app{cfg: &config.Config{RepoOverride: repo}}
			confirmations := 0
			var code int
			var err error
			out := captureStderr(t, func() {
				code, err = a.approveProject(t.Context(), repo, false, func(string) bool {
					confirmations++
					return true
				})
			})
			if code != 0 || err != nil || (confirmations == 1) != networkChange || confirmations > 1 || strings.Contains(out, "FILE ACCESS") {
				t.Fatalf("network change %t: code=%d err=%v confirmations=%d output=%s", networkChange, code, err, confirmations, out)
			}
			if pending := box.UnapprovedCaseExposures(repo, []string{"credentials.json"}); len(pending) != 1 {
				t.Fatal("ordinary approval granted file exposure")
			}
			for _, path := range []string{filepath.Join(state, "case-exposure-approvals"), filepath.Join(repo, ".coop-service-approval")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("ordinary approval touched file authority %s: %v", path, err)
				}
			}
		})
	}
}

func TestMountFallbackRemedyRequiresExplicitCaseApproval(t *testing.T) {
	for _, who := range []string{"the box", "your services"} {
		_, _, fix := uncoveredSpellingsBlock([]string{"credentials.json"}, who, "a service")
		if !strings.Contains(fix, "coop approve --case-exposure") {
			t.Fatal(fix)
		}
	}
}

func TestApproveExplicitCaseExposure(t *testing.T) {
	netHostFixture(t)
	state := t.TempDir()
	t.Setenv(box.ServiceStateRootEnv, state)
	repo := netFixtureProject(t)
	if err := os.WriteFile(filepath.Join(repo, "credentials.json"), []byte("inert"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "CREDENTIALS.JSON")); err != nil {
		t.Skip("requires a disk that folds case")
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	for _, accept := range []bool{false, true} {
		var code int
		var err error
		confirmations := 0
		out := captureStderr(t, func() {
			code, err = a.approveProject(t.Context(), repo, true, func(prompt string) bool {
				confirmations++
				if !strings.Contains(prompt, "using mount protection") {
					t.Error(prompt)
				}
				return accept
			})
		})
		if (code == 0 && err == nil) != accept || confirmations != 1 || !strings.Contains(out, "FILE ACCESS WITH MOUNT PROTECTION") {
			t.Fatalf("accepted=%t: %d, %v, confirmations=%d, output=%s", accept, code, err, confirmations, out)
		}
		if pending := box.UnapprovedCaseExposures(repo, []string{"credentials.json"}); (len(pending) == 0) != accept {
			t.Fatalf("accepted=%t: pending=%v", accept, pending)
		}
	}
	files, err := filepath.Glob(filepath.Join(state, "case-exposure-approvals", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("approval files: %v, %v", files, err)
	}
	before, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "production-secret.pem"), []byte("inert"), 0600); err != nil {
		t.Fatal(err)
	}
	captureStderr(t, func() {
		if code, err := a.approveProject(t.Context(), repo, false, func(string) bool {
			t.Error("ordinary approval asked for exposure")
			return false
		}); code != 0 || err != nil {
			t.Fatalf("ordinary approval: %d, %v", code, err)
		}
	})
	after, err := os.ReadFile(files[0])
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("saved approval changed: %v", err)
	}
	if pending := box.UnapprovedCaseExposures(repo, []string{"credentials.json", "production-secret.pem"}); len(pending) != 1 || pending[0] != "production-secret.pem" {
		t.Fatal(pending)
	}
}

func TestApproveDoesNotInspectMountPolicy(t *testing.T) {
	netHostFixture(t)
	t.Setenv(box.ServiceStateRootEnv, t.TempDir())
	repo := netFixtureProject(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(repo, ".coopignore")); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	captureStderr(t, func() {
		if code, err := a.approveProject(t.Context(), repo, false, func(string) bool {
			t.Error("ordinary approval asked for exposure")
			return false
		}); code != 0 || err != nil {
			t.Fatalf("ordinary approval inspected mount policy: %d, %v", code, err)
		}
		if code, err := a.approveProject(t.Context(), repo, true, func(string) bool {
			t.Error("invalid policy was offered for approval")
			return true
		}); code != 1 || err == nil {
			t.Fatalf("explicit review accepted invalid mount policy: %d, %v", code, err)
		}
	})
}

// With nothing to approve and no box stuck on service rules, coop approve says its one line. Asking for
// nothing, it writes nothing and succeeds.
func TestApproveWithNothingToApproveSaysSo(t *testing.T) {
	for name, test := range map[string]struct {
		project, compose string
		approved         bool // a person approved the file first
		configure        func(*config.Config)
	}{
		"no services":            {project: "box:\n  egress: filtered\n"},
		"service rules approved": {project: approveServiceRule, compose: approveCompose, approved: true},
		"box.network: false":     {project: "box:\n  network: false\n", compose: approveCompose},
		"COOP_NETWORK off":       {compose: approveCompose, configure: func(c *config.Config) { c.Network = false }},
		"open":                   {project: "box:\n  egress: open\n", compose: approveCompose, approved: true},
		"offline":                {project: "box:\n  egress: offline\n", compose: approveCompose},
		"open on this host":      {compose: approveCompose, configure: func(c *config.Config) { c.SetEgress("open") }},
		"offline on this host":   {compose: approveCompose, configure: func(c *config.Config) { c.SetEgress(string(egress.None)) }},
	} {
		t.Run(name, func(t *testing.T) {
			a, repo := servicesProject(t, test.project, test.compose)
			if test.configure != nil {
				test.configure(a.cfg)
			}
			if test.approved {
				captureStderr(t, func() {
					if code, err := a.approveProject(t.Context(), repo, false, func(string) bool { return true }); code != 0 || err != nil {
						t.Fatalf("first approval: %d, %v", code, err)
					}
				})
			}
			root, err := box.NetworkStatePath()
			if err != nil {
				t.Fatal(err)
			}
			before := directoryListing(t, root, repo)
			var code int
			out := captureStderr(t, func() {
				code, err = a.approveProject(t.Context(), repo, false, func(string) bool {
					t.Error("nothing to approve, but it asked")
					return false
				})
			})
			if code != 0 || err != nil {
				t.Fatalf("code=%d err=%v, want it to exit 0", code, err)
			}
			if want := netApproveUnchanged + "\n"; out != want {
				t.Errorf("printed\n%s\nwant\n%s", out, want)
			}
			if after := directoryListing(t, root, repo); after != before {
				t.Errorf("a no-op wrote something:\n%s\nwas\n%s", after, before)
			}
		})
	}
}

const (
	approveCompose     = "services:\n  db:\n    image: postgres:18\n    expose: [\"5432\"]\n"
	approveServiceRule = "box:\n  egress_rules:\n    - to: {service: \"db\"}\n      protocol: tcp\n      ports: [5432]\n"
	approveOtherRule   = "box:\n  egress_rules:\n    - to: {domain: docs.example.com}\n      protocol: tls\n      ports: [443]\n"
	approveWithHistory = "# Why this project looks like this.\nbox:\n  # Reach the docs.\n  egress_rules:\n    - to: {domain: docs.example.com} # for the guide\n      protocol: tls\n      ports: [443]\n\n  # Limits.\n  memory: 4g\n"
)

// runApprove answers every question it hears, returning those questions and what was printed.
func runApprove(t *testing.T, a *app, repo string, answer func(string) bool) (code int, err error, asked []string, out string) {
	t.Helper()
	out = captureStderr(t, func() {
		code, err = a.approveProject(t.Context(), repo, false, func(prompt string) bool {
			asked = append(asked, prompt)
			return answer(prompt)
		})
	})
	return code, err, asked, out
}

// A box stuck on missing service rules: one yes shows them, writes them where the file's own layout
// keeps them, keeps the file's comments and order, and approves them so the box can start. Each
// service's definition is saved with them, so a later edit asks again.
func TestApproveAddsTheServiceRulesItShowed(t *testing.T) {
	a, repo := servicesProject(t, "", "services:\n  db:\n    image: postgres:18\n    expose: [\"5432\"]\n  api:\n    image: example/api:1\n    expose: [\"3000\", \"9229\"]\n")
	code, err, asked, out := runApprove(t, a, repo, func(string) bool { return true })
	if code != 0 || err != nil || len(asked) != 1 || asked[0] != serviceRulesPrompt {
		t.Fatalf("code=%d err=%v asked=%q out=%s", code, err, asked, out)
	}
	const want = "⚠ This project's box cannot start because filtered boxes reach project services only through approved network rules\n\n" +
		"  Coop can add rules for these services to .agent/project.yaml and approve them:\n" +
		"    api  TCP 3000, 9229\n" +
		"    db   TCP 5432\n\n" +
		"✓ " + serviceRulesApproved + "\n"
	if out != want {
		t.Errorf("printed\n%s\nwant\n%s", out, want)
	}
	if refusal, err := box.ProjectServicesRefusal(t.Context(), a.cfg, repo); err != nil || refusal != nil {
		t.Fatalf("the box is still refused: %v, %v", refusal, err)
	}
	access, err := box.ProjectNetworkAccess(t.Context(), a.cfg, repo)
	if err != nil || access.Pending != nil || access.Approval == nil || len(access.Approval.Services) != 2 {
		t.Fatalf("approval = %+v, %v; want it saved with both digests", access.Approval, err)
	}
	// Nothing left to say.
	if code, err, asked, out = runApprove(t, a, repo, func(string) bool { return false }); code != 0 || err != nil || len(asked) != 0 || out != netApproveUnchanged+"\n" {
		t.Fatalf("again: code=%d err=%v asked=%q out=%s", code, err, asked, out)
	}
	// Approval saved the definition, so changing it asks again.
	if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"), []byte("services:\n  db:\n    image: postgres:19\n    expose: [\"5432\"]\n  api:\n    image: example/api:1\n    expose: [\"3000\", \"9229\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if access, err = box.ProjectNetworkAccess(t.Context(), a.cfg, repo); err != nil || access.Pending == nil {
		t.Fatalf("an edited service did not ask again: %+v, %v", access.Pending, err)
	}
}

// Comments and order survive, and rules already approved stay approved beside the new ones.
func TestApproveKeepsTheFileItAddsRulesTo(t *testing.T) {
	a, repo := servicesProject(t, approveWithHistory, approveCompose)
	code, err, asked, out := runApprove(t, a, repo, func(string) bool { return true })
	if code != 0 || err != nil || len(asked) != 2 || asked[0] != netApprovePrompt || asked[1] != serviceRulesPrompt {
		t.Fatalf("a pending rule and missing service rules are two questions: code=%d err=%v asked=%q out=%s", code, err, asked, out)
	}
	const want = "# Why this project looks like this.\nbox:\n  # Reach the docs.\n  egress_rules:\n    - to: {domain: docs.example.com} # for the guide\n      protocol: tls\n      ports: [443]\n" +
		"    - to: {service: \"db\"}\n      protocol: tcp\n      ports: [5432]\n\n  # Limits.\n  memory: 4g\n"
	if got, _ := os.ReadFile(filepath.Join(repo, ".agent", "project.yaml")); string(got) != want {
		t.Errorf("project.yaml =\n%s\nwant\n%s", got, want)
	}
	access, err := box.ProjectNetworkAccess(t.Context(), a.cfg, repo)
	if err != nil || access.Pending != nil || access.Approval == nil || len(access.Approval.Envelope) != 2 {
		t.Fatalf("approval = %+v, %v", access.Approval, err)
	}
}

// No changes nothing, exits 0, and says what stays blocked. The default answer is no.
func TestApproveDecliningServiceRulesChangesNothing(t *testing.T) {
	a, repo := servicesProject(t, approveOtherRule, approveCompose)
	code, err, _, _ := runApprove(t, a, repo, func(prompt string) bool { return prompt == netApprovePrompt })
	if code != 0 || err != nil {
		t.Fatalf("declining the rules after approving the domain: %d, %v", code, err)
	}
	root, err := box.NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	before := directoryListing(t, root, repo)
	code, err, asked, out := runApprove(t, a, repo, func(string) bool { return false })
	if code != 0 || err != nil || len(asked) != 1 {
		t.Fatalf("code=%d err=%v asked=%q", code, err, asked)
	}
	for _, line := range []string{"cannot start because", "db", "Nothing changed. This project's box stays blocked", "box.network: false"} {
		if !strings.Contains(out, line) {
			t.Errorf("no %q in\n%s", line, out)
		}
	}
	if strings.Contains(out, netApproveUnchanged) || !strings.HasPrefix(out, "⚠ This project's box cannot start") {
		t.Errorf("it does not lead with the problem:\n%s", out)
	}
	if after := directoryListing(t, root, repo); after != before {
		t.Errorf("a no wrote something:\n%s\nwas\n%s", after, before)
	}
}

// Rules the file already asks for are the real question, so declining them ends it: nothing more is
// offered on top of an answer of no.
func TestApproveDecliningThePendingRulesOffersNothing(t *testing.T) {
	a, repo := servicesProject(t, approveServiceRule, approveCompose)
	code, err, asked, out := runApprove(t, a, repo, func(string) bool { return false })
	if code != 1 || err == nil || len(asked) != 1 || asked[0] != netApprovePrompt || strings.Contains(out, "cannot start") {
		t.Fatalf("code=%d err=%v asked=%q out=%s", code, err, asked, out)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, ".agent", "project.yaml")); string(got) != approveServiceRule {
		t.Errorf("project.yaml changed:\n%s", got)
	}
}

// A network no filtered box joins has no rules to offer, so it says what to change and asks nothing.
func TestApproveWithOnlyAServicesNetworkOffersNothing(t *testing.T) {
	a, repo := servicesProject(t, "", "")
	a.cfg.ServicesNet = "shared"
	code, err, asked, out := runApprove(t, a, repo, func(string) bool { return true })
	if code != 0 || err != nil || len(asked) != 0 {
		t.Fatalf("code=%d err=%v asked=%q", code, err, asked)
	}
	const want = "⚠ This project's box cannot start because a filtered box does not join the network COOP_SERVICES_NET names\n\n" +
		"  Unset COOP_SERVICES_NET, or set box.network: false in .agent/project.yaml\n"
	if out != want {
		t.Errorf("printed\n%s\nwant\n%s", out, want)
	}
	if _, err := os.Stat(filepath.Join(repo, ".agent", "project.yaml")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("it wrote a project file: %v", err)
	}
}

// A service's required dependency is pinned with it, and a yes still approves. Its definition is part of
// what was reviewed, so editing only the dependency during the question stops the approval.
func TestApproveCoversAServiceDependency(t *testing.T) {
	const compose = "services:\n  api:\n    image: example/api:1\n    expose: [\"3000\"]\n    depends_on: [db]\n  db:\n    image: postgres:18\n"
	a, repo := servicesProject(t, "", compose)
	code, err, asked, out := runApprove(t, a, repo, func(string) bool { return true })
	if code != 0 || err != nil || len(asked) != 1 {
		t.Fatalf("code=%d err=%v asked=%q out=%s", code, err, asked, out)
	}
	access, err := box.ProjectNetworkAccess(t.Context(), a.cfg, repo)
	if err != nil || access.Pending != nil || access.Approval == nil || len(access.Approval.Services) != 2 {
		t.Fatalf("approval = %+v, %v; want the dependency's digest saved too", access.Approval, err)
	}

	a, repo = servicesProject(t, "", compose)
	code, err, _, _ = runApprove(t, a, repo, func(string) bool {
		edited := strings.Replace(compose, "postgres:18", "example/other:1", 1)
		if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"), []byte(edited), 0o600); err != nil {
			t.Fatal(err)
		}
		return true
	})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "nothing was approved") {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

// A port Coop cannot tell is never guessed: that service stays the person's, shown as a rule to finish.
func TestApproveNeverGuessesAPort(t *testing.T) {
	a, repo := servicesProject(t, "", servicesCompose)
	code, err, asked, out := runApprove(t, a, repo, func(string) bool { return true })
	if code != 0 || err != nil || len(asked) != 1 {
		t.Fatalf("code=%d err=%v asked=%q out=%s", code, err, asked, out)
	}
	if !strings.Contains(out, "Coop cannot tell these services' ports") || !strings.Contains(out, "{service: \"web\"}") || !strings.Contains(out, "<port>") {
		t.Errorf("web's rule is not left to the person:\n%s", out)
	}
	got, err := os.ReadFile(filepath.Join(repo, ".agent", "project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "{service: \"web\"}") || !strings.Contains(string(got), "{service: \"db\"}") {
		t.Errorf("project.yaml =\n%s\nwant only db", got)
	}

	// With no port it can tell, there is nothing to offer, so it asks nothing and writes nothing.
	a, repo = servicesProject(t, "", "services:\n  web:\n    image: nginx:1\n")
	root, err := box.NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	before := directoryListing(t, root, repo)
	code, err, asked, out = runApprove(t, a, repo, func(string) bool { return true })
	if code != 0 || err != nil || len(asked) != 0 {
		t.Fatalf("code=%d err=%v asked=%q", code, err, asked)
	}
	if !strings.Contains(out, "<port>") || !strings.Contains(out, "box.network: false") || strings.Contains(out, "offers to add") || strings.Contains(out, netApproveUnchanged) {
		t.Errorf("printed\n%s", out)
	}
	if after := directoryListing(t, root, repo); after != before {
		t.Errorf("it wrote something:\n%s\nwas\n%s", after, before)
	}
}

// The file a yes writes to is one a box can edit while the question waits. The person saw only the
// rules on screen and the services as they were, so anything else stops the approval.
func TestApproveDoesNotCarryALaterEditAlong(t *testing.T) {
	for name, edit := range map[string]func(t *testing.T, repo string){
		"another rule": func(t *testing.T, repo string) {
			if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte(approveOtherRule), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"another definition": func(t *testing.T, repo string) {
			if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"), []byte("services:\n  db:\n    image: example/other:1\n    expose: [\"5432\"]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"another port": func(t *testing.T, repo string) {
			if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("box:\n  egress_rules:\n    - to: {service: \"db\"}\n      protocol: tcp\n      ports: [22]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, repo := servicesProject(t, "", approveCompose)
			code, err, _, _ := runApprove(t, a, repo, func(string) bool {
				edit(t, repo)
				return true
			})
			if code != 1 || err == nil || !strings.Contains(err.Error(), "nothing was approved") {
				t.Fatalf("code=%d err=%v", code, err)
			}
			access, err := box.ProjectNetworkAccess(t.Context(), a.cfg, repo)
			if err != nil || access.Approval != nil || access.Pending == nil {
				t.Fatalf("approval=%+v pending=%v err=%v, want nothing approved", access.Approval, access.Pending, err)
			}
		})
	}
}

// With no terminal it decides nothing. It shows the rules and the box.network: false choice only when
// they are the whole problem, and fails, since nothing got settled.
func TestApproveWithoutATerminal(t *testing.T) {
	a, repo := servicesProject(t, "", approveCompose)
	root, err := box.NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	before := directoryListing(t, root, repo)
	code, err := a.cmdApprove(nil)
	if code != 1 || err == nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	for _, line := range []string{"{service: \"db\"}", "ports: [5432]", "box.network: false", "coop approve"} {
		if !strings.Contains(err.Error(), line) {
			t.Errorf("no %q in\n%v", line, err)
		}
	}
	if after := directoryListing(t, root, repo); after != before {
		t.Errorf("it wrote something:\n%s\nwas\n%s", after, before)
	}
	const onlyTerminal = "needs your confirmation in a terminal"
	if _, err := a.cmdApprove([]string{"--case-exposure"}); err == nil || !strings.Contains(usageBlock(t, err), onlyTerminal) {
		t.Errorf("--case-exposure: %v", err)
	}
	// A real question stays a refusal, with nothing to act on until a person sees the review.
	a, repo = servicesProject(t, approveOtherRule, approveCompose)
	if code, err := a.cmdApprove(nil); code != 1 || err == nil || !strings.Contains(usageBlock(t, err), onlyTerminal) {
		t.Errorf("pending: code=%d err=%v", code, err)
	}
	if got, _ := os.ReadFile(filepath.Join(repo, ".agent", "project.yaml")); string(got) != approveOtherRule {
		t.Errorf("project.yaml changed:\n%s", got)
	}
	a, _ = servicesProject(t, "box:\n  egress: filtered\n", "")
	if _, err := a.cmdApprove(nil); err == nil || !strings.Contains(usageBlock(t, err), onlyTerminal) {
		t.Errorf("no services: %v", err)
	}
}

// A project file that does not parse is a failure to look, never a project with nothing wrong.
func TestApproveReportsAProjectItCannotRead(t *testing.T) {
	a, repo := servicesProject(t, "box: [\n", "services:\n  db:\n    image: postgres:18\n")
	if refusal, err := box.ProjectServicesRefusal(t.Context(), a.cfg, repo); err == nil || refusal != nil {
		t.Fatalf("refused=%t err=%v, want a failure to look", refusal != nil, err)
	}
	var code int
	var err error
	out := captureStderr(t, func() {
		code, err = a.approveProject(t.Context(), repo, false, func(string) bool {
			t.Error("it asked about a file it could not read")
			return false
		})
	})
	if code != 1 || err == nil || strings.Contains(out, netApproveUnchanged) {
		t.Fatalf("code=%d err=%v out=%s", code, err, out)
	}
}

// servicesProject is a project with these .agent files, on a host with no network state yet, and the app
// that serves it, with services on as they are by default.
func servicesProject(t *testing.T, projectFile, compose string) (*app, string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := netFixtureProject(t)
	if err := os.Mkdir(filepath.Join(repo, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	for file, body := range map[string]string{"project.yaml": projectFile, "compose.yml": compose} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(repo, ".agent", file), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &app{cfg: &config.Config{ConfigDir: t.TempDir(), RepoOverride: repo, Network: true}}, repo
}

// directoryListing names everything under these folders, with each file's size and time, so that a test
// can see a command wrote nothing. A folder that does not exist lists as empty.
func directoryListing(t *testing.T, roots ...string) string {
	t.Helper()
	var b strings.Builder
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "%s %d %s\n", path, info.Size(), info.ModTime())
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

func TestRetiredNetApprovePointsAtApprove(t *testing.T) {
	code, err := (&app{}).cmdNet([]string{"approve"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), `"coop net approve" has moved`) || !strings.Contains(err.Error(), "coop approve") {
		t.Fatalf("coop net approve = (%d, %v)", code, err)
	}

	code, err = helpForPath([]string{"net", "approve"}, nil, true)
	if code != 2 || err == nil || !strings.Contains(err.Error(), `"coop net approve" has moved`) || !strings.Contains(err.Error(), "coop approve") {
		t.Fatalf("coop help net approve = (%d, %v)", code, err)
	}
}

// A change that was saved but could not reach running editor sessions is still the success a person
// confirmed: it gets its ✓ and one caution, exits 0, and only a real failure exits 1.
func TestNetChangeSavedButUntoldIsASuccessWithACaution(t *testing.T) {
	var code int
	var err error
	out := captureStderr(t, func() {
		code, err = reportNetChange(fmt.Errorf("%w (disk full)", networkstate.ErrSessionsNotTold), netApproveApproved)
	})
	if code != 0 || err != nil || !strings.Contains(out, netApproveApproved) || !strings.Contains(out, netSessionsNotTold) || strings.Contains(out, "disk full") {
		t.Fatalf("an untold change = (%d, %v), printed %q; want the ✓, one caution and no cause", code, err, out)
	}
	failure := errors.New("the request changed while you were reviewing it")
	out = captureStderr(t, func() { code, err = reportNetChange(failure, netApproveApproved) })
	if code != 1 || !errors.Is(err, failure) || strings.Contains(out, netApproveApproved) {
		t.Fatalf("a failed change = (%d, %v), printed %q; want exit 1 and no success line", code, err, out)
	}
}

func TestApprovalIncludesCaseOnlyAccessAndFreezesPaths(t *testing.T) {
	for _, networkChange := range []bool{false, true} {
		t.Run(fmt.Sprint(networkChange), func(t *testing.T) {
			t.Setenv(box.ServiceStateRootEnv, t.TempDir())
			repo := t.TempDir()
			file := filepath.Join(repo, "credentials.json")
			if err := os.WriteFile(file, []byte("inert"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(repo, "CREDENTIALS.JSON")); err != nil {
				t.Skip("requires a disk that folds case")
			}
			cases, err := box.ReviewCaseExposure(t.Context(), nil, repo)
			if err != nil {
				t.Fatal(err)
			}
			network := &fakeApprovalReview{}
			if networkChange {
				network.after = &networkstate.Approval{Posture: egress.Filtered}
			}
			var out bytes.Buffer
			err = confirmProjectApproval(t.Context(), network, cases, &out, func() bool { return false })
			if err == nil || len(box.UnapprovedCaseExposures(repo, []string{"credentials.json"})) != 1 {
				t.Fatal("declined approval persisted")
			}
			out.Reset()
			err = confirmProjectApproval(t.Context(), network, cases, &out, func() bool {
				if err := os.WriteFile(filepath.Join(repo, "production-secret.pem"), []byte("inert"), 0600); err != nil {
					t.Fatal(err)
				}
				return true
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "FILE ACCESS WITH MOUNT PROTECTION") || !strings.Contains(out.String(), "credentials.json") ||
				!strings.Contains(out.String(), "Qualified eBPF runs still protect these files without this approval.") {
				t.Fatal(out.String())
			}
			if (network.committed == 1) != networkChange {
				t.Fatalf("network commits=%d", network.committed)
			}
			pending := box.UnapprovedCaseExposures(repo, []string{"credentials.json", "production-secret.pem"})
			if len(pending) != 1 || pending[0] != "production-secret.pem" {
				t.Fatal(pending)
			}
		})
	}
}

func TestApprovalFileFailureDoesNotPublishNetworkOrReportSuccess(t *testing.T) {
	t.Setenv(box.ServiceStateRootEnv, t.TempDir())
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "credentials.json"), []byte("inert"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "CREDENTIALS.JSON")); err != nil {
		t.Skip("requires a disk that folds case")
	}
	cases, err := box.ReviewCaseExposure(t.Context(), nil, repo)
	if err != nil {
		t.Fatal(err)
	}
	network := &fakeApprovalReview{after: &networkstate.Approval{Posture: egress.Filtered}, commitErr: networkstate.ErrSessionsNotTold}
	err = confirmProjectApproval(t.Context(), network, cases, &bytes.Buffer{}, func() bool {
		if err := os.Remove(filepath.Join(repo, ".coop-service-approval")); err != nil {
			t.Fatal(err)
		}
		return true
	})
	if err == nil || network.committed != 0 || errors.Is(err, networkstate.ErrSessionsNotTold) {
		t.Fatalf("failed file approval: network commits=%d err=%v", network.committed, err)
	}
}
