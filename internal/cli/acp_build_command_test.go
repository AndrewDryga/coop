package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/acpproxy"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/threadtext"
)

// fakeReviewedBuild is a saved snapshot that records whether it was closed.
type fakeReviewedBuild struct {
	dir    string
	mu     sync.Mutex
	closed int
}

func (b *fakeReviewedBuild) Directory() string  { return b.dir }
func (b *fakeReviewedBuild) Dockerfile() string { return ".agent/Dockerfile" }
func (b *fakeReviewedBuild) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed++
	return nil
}

func (b *fakeReviewedBuild) closes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// buildCommandHarness is a filtered session's wait whose snapshots and builds are fakes. Setting
// prepareErr makes saving the files fail, and refusal is what Coop then says of the failure: "" is no
// refusal at all. Every failure it was asked about is in recorded.
type buildCommandHarness struct {
	*boxWaitHarness
	mu         sync.Mutex
	snapshots  []*fakeReviewedBuild
	built      []acpPreparedBuild
	result     chan error
	prepareErr error
	refusal    string
	recorded   []error
}

func newBuildCommandHarness(t *testing.T) *buildCommandHarness {
	t.Helper()
	h := &buildCommandHarness{boxWaitHarness: newBoxWaitHarness(t), result: make(chan error, 4)}
	h.w.filtered = true
	h.w.prepareBuild = func(context.Context) (acpPreparedBuild, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.prepareErr != nil {
			return nil, h.prepareErr
		}
		b := &fakeReviewedBuild{dir: "/private/coop/review-" + string(rune('a'+len(h.snapshots)))}
		h.snapshots = append(h.snapshots, b)
		return b, nil
	}
	h.w.recordRefusal = func(_ context.Context, err error) string {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.recorded = append(h.recorded, err)
		return h.refusal
	}
	h.w.buildReviewed = func(ctx context.Context, prepared acpPreparedBuild) error {
		h.mu.Lock()
		h.built = append(h.built, prepared)
		h.mu.Unlock()
		select {
		case err := <-h.result:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h.set(func() { h.answer, h.holding = errNeedsAPerson, true })
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *buildCommandHarness) builds() []acpPreparedBuild {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]acpPreparedBuild(nil), h.built...)
}

func buildPrompt(id, text string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"session/prompt","params":{"sessionId":"S1","prompt":[{"type":"text","text":"` + text + `"}]}}`)
}

var buildCodePattern = regexp.MustCompile(`/build ([a-z2-7]{8})`)

// send types text in the thread and returns what Coop answered, checking the answer ends that prompt.
func (h *buildCommandHarness) send(t *testing.T, id, text string) string {
	t.Helper()
	answer := string(h.w.heldCommand("S1", buildPrompt(id, text)))
	if answer == "" {
		t.Fatalf("%q was not answered", text)
	}
	if !strings.Contains(answer, `"id":`+id+`,"result":{"stopReason":"end_turn"}`) || !strings.Contains(answer, `"sessionId":"S1"`) {
		t.Fatalf("%q got %s, want a message in S1 that ends prompt %s", text, answer, id)
	}
	threadtext.Check(t, h.saidIn(t, answer))
	return answer
}

// saidIn is the text of the message in an answer.
func (h *buildCommandHarness) saidIn(t *testing.T, answer string) string {
	t.Helper()
	var m struct {
		Params struct {
			Update struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(answer, "\n", 2)[0]), &m); err != nil || m.Params.Update.Content.Text == "" {
		t.Fatalf("no message in %s: %v", answer, err)
	}
	return m.Params.Update.Content.Text
}

// A filtered session waiting on build files nobody approved says it can build from the thread, and
// /build only saves them and shows where: nothing is built until the code it gives comes back.
func TestBuildCommandShowsTheSavedFilesBeforeBuilding(t *testing.T) {
	h := newBuildCommandHarness(t)
	said := h.lastSaid()
	if !strings.Contains(said, "/build") || !strings.Contains(said, "coop build --egress filtered") {
		t.Fatalf("the thread heard %q, want both ways to build", said)
	}
	threadtext.Check(t, said)
	threadtext.Check(t, acpBuildableWaitText(true, "coop build --egress filtered"))
	answer := h.send(t, "3", "/build")
	if !strings.Contains(answer, "/private/coop/review-a/.agent/Dockerfile") || !strings.Contains(answer, "open networking") {
		t.Fatalf("the review answer is %s", answer)
	}
	if len(h.builds()) != 0 {
		t.Fatal("/build alone built something")
	}
	code := buildCodePattern.FindStringSubmatch(answer)
	if code == nil {
		t.Fatalf("no code in %s", answer)
	}

	answer = h.send(t, "4", "/build "+code[1])
	if !strings.Contains(answer, "Building this project's box") {
		t.Fatalf("the build answer is %s", answer)
	}
	for deadline := time.Now().Add(3 * time.Second); h.lastSaid() != acpReviewedBuildText; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the other threads heard %q", h.lastSaid())
		}
	}
	// While it builds, /build says so and builds nothing more.
	if answer := h.send(t, "5", "/build "+code[1]); !strings.Contains(answer, "Building this project's box") {
		t.Fatalf("a second /build during the build got %s", answer)
	}
	h.result <- nil
	h.settled(t)
	built := h.builds()
	if len(built) != 1 || built[0] != h.snapshots[0] {
		t.Fatalf("built %v, want exactly the reviewed snapshot once", built)
	}
	if h.snapshots[0].closes() != 1 {
		t.Fatalf("the snapshot was closed %d times after its build", h.snapshots[0].closes())
	}

	// The code worked once.
	h.set(func() { h.sign = "two" })
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if answer := h.send(t, "6", "/build "+code[1]); !strings.Contains(answer, "Nothing was built") {
		t.Fatalf("a used code got %s", answer)
	}
	if len(h.builds()) != 1 {
		t.Fatal("a used code built again")
	}
}

// A wrong, replaced or old code builds nothing, and a replaced or old snapshot is let go.
func TestBuildCommandRefusesAnyOtherCode(t *testing.T) {
	h := newBuildCommandHarness(t)
	first := buildCodePattern.FindStringSubmatch(h.send(t, "3", "/build"))[1]
	if answer := h.send(t, "4", "/build aaaaaaaa"); !strings.Contains(answer, "Nothing was built") {
		t.Fatalf("a wrong code got %s", answer)
	}
	second := buildCodePattern.FindStringSubmatch(h.send(t, "5", "/build"))[1]
	if first == second {
		t.Fatal("two snapshots got the same code")
	}
	if h.snapshots[0].closes() != 1 {
		t.Fatal("the replaced snapshot was kept")
	}
	if answer := h.send(t, "6", "/build "+first); !strings.Contains(answer, "Nothing was built") {
		t.Fatalf("the replaced code got %s", answer)
	}
	h.set(func() { h.clock = h.clock.Add(acpBuildCodeLife) })
	if answer := h.send(t, "7", "/build "+second); !strings.Contains(answer, "Nothing was built") {
		t.Fatalf("an old code got %s", answer)
	}
	if h.snapshots[1].closes() != 1 {
		t.Fatal("the old snapshot was kept")
	}
	if len(h.builds()) != 0 {
		t.Fatal("a refused code built something")
	}
}

// /build is a command only while this session waits on build files nobody approved, in a filtered
// session, typed alone. Anything else is an ordinary message, held for the box.
func TestBuildCommandIsOnlyACommandWhileABuildWaitsForAPerson(t *testing.T) {
	h := newBuildCommandHarness(t)
	for _, line := range [][]byte{
		buildPrompt("3", "/build it please now"),
		buildPrompt("3", "please /build"),
		buildPrompt("3", "/rebuild"),
		[]byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"S1","prompt":[{"type":"text","text":"/build"},{"type":"text","text":"more"}]}}`),
		[]byte(`{"jsonrpc":"2.0","method":"session/prompt","params":{"sessionId":"S1","prompt":[{"type":"text","text":"/build"}]}}`),
	} {
		if answer := h.w.heldCommand("S1", line); answer != nil {
			t.Errorf("%s was answered: %s", line, answer)
		}
	}
	for name, answer := range map[string]error{"approved": errApprovedStale, "ready": nil, "could not look": errCouldNotLook} {
		h := newBuildCommandHarness(t)
		h.set(func() { h.answer, h.sign = answer, "changed" })
		_ = h.w.refresh(t.Context())
		if name == "could not look" {
			// A failure to look proves nothing: the last answer stands, and so does the command.
			if h.w.heldCommand("S1", buildPrompt("3", "/build")) == nil {
				t.Error("a failure to look took the command away")
			}
			continue
		}
		if h.w.heldCommand("S1", buildPrompt("3", "/build")) != nil {
			t.Errorf("%s: /build was a command", name)
		}
	}
	unfiltered := newBuildCommandHarness(t)
	unfiltered.w.filtered = false
	if unfiltered.w.heldCommand("S1", buildPrompt("3", "/build")) != nil {
		t.Error("an unfiltered session offered a build")
	}
}

// A failed build says so in the thread and lets the person try again; the session closing cancels a
// build and lets go of any snapshot nobody built.
func TestBuildCommandFailureAndClose(t *testing.T) {
	h := newBuildCommandHarness(t)
	code := buildCodePattern.FindStringSubmatch(h.send(t, "3", "/build"))[1]
	h.send(t, "4", "/build "+code)
	h.result <- errors.New("the build failed")
	h.settled(t)
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if said := h.lastSaid(); !strings.Contains(said, "could not build") || !strings.Contains(said, "/build") {
		t.Fatalf("after a failed build the thread heard %q", said)
	}
	h.read(func() {
		if len(h.logged) == 0 || !strings.Contains(h.logged[len(h.logged)-1].Error(), "the build failed") {
			t.Errorf("the agent log got %v", h.logged)
		}
	})

	h.send(t, "5", "/build")
	code = buildCodePattern.FindStringSubmatch(h.send(t, "6", "/build"))[1]
	h.send(t, "7", "/build "+code)
	done := make(chan struct{})
	go func() { h.w.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("closing the session did not cancel the build")
	}
	for i, snapshot := range h.snapshots {
		if snapshot.closes() != 1 {
			t.Errorf("snapshot %d closed %d times", i, snapshot.closes())
		}
	}
}

// Once the wait is over, the snapshot nobody built is let go.
func TestBuildCommandLetsGoOfTheSnapshotWhenTheWaitEnds(t *testing.T) {
	h := newBuildCommandHarness(t)
	h.send(t, "3", "/build")
	h.set(func() { h.answer, h.sign = nil, "built on the host" })
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h.snapshots[0].closes() != 1 {
		t.Fatal("the snapshot outlived its wait")
	}
}

// linkedDockerfileRefusal is the miss readiness hands a session whose project's Dockerfile is a
// symbolic link: Coop refused the build, and the reason is the one `coop build` found. It also returns
// the project.
func linkedDockerfileRefusal(t *testing.T) (*box.ProjectBuildRequired, string) {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "real.Dockerfile"), []byte("FROM ${COOP_BASE_IMAGE}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../real.Dockerfile", filepath.Join(repo, ".agent", "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	daemon := filepath.Join(t.TempDir(), "docker") // one that answers, since a plan asks before it reads
	if err := os.WriteFile(daemon, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := box.PlanBuild(runtime.Runtime{Name: daemon}, &config.Config{}, repo, false)
	if err == nil {
		t.Fatal("a Dockerfile that is a link was planned")
	}
	return &box.ProjectBuildRequired{Dockerfile: ".agent/Dockerfile", Reason: err.Error(), Refused: true}, repo
}

// Build files Coop refused would be refused again by a /build or a review, so the thread says why and
// what to fix, as `coop run` does, and offers neither. A project that only has not been built keeps
// its offer, and so does one whose files were fixed (QA, 2026-11-06: a linked Dockerfile).
func TestBuildCommandSaysWhyTheBuildFilesWereRefused(t *testing.T) {
	refused, _ := linkedDockerfileRefusal(t)
	h := newBuildCommandHarness(t)
	if said := h.lastSaid(); !strings.Contains(said, "needs a build") || !strings.Contains(said, "/build") {
		t.Fatalf("a project that is not built heard %q, want the offer to build it here", said)
	}
	h.send(t, "3", "/build") // a review saves the files, which a refusal must not leave open

	h.set(func() { h.answer, h.sign = refused, "linked" })
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	said := h.lastSaid()
	for _, want := range []string{
		"Coop refused the last build of this project's box.",
		"Dockerfile is a symbolic link. Replace it with the file it points to.",
		"After you fix it, build it again.",
		"go out once",
		"```\ncoop build --egress filtered\n```",
	} {
		if !strings.Contains(said, want) {
			t.Errorf("the thread heard %q, want %q in it", said, want)
		}
	}
	for _, unwanted := range []string{"/build", "needs a build", "review"} {
		if strings.Contains(said, unwanted) {
			t.Errorf("the thread heard %q, which offers %q for files that would be refused again", said, unwanted)
		}
	}
	threadtext.Check(t, said)
	if h.snapshots[0].closes() != 1 {
		t.Error("the files saved for a review stayed open after Coop refused them")
	}
	h.read(func() {
		if len(h.logged) == 0 || h.logged[len(h.logged)-1].Error() != refused.Error() {
			t.Errorf("the agent log got %v, want the refusal as `coop run` words it: %q", h.logged, refused.Error())
		}
	})

	// /build is answered with the same words, and saves and builds nothing.
	if answer := h.send(t, "4", "/build"); strings.TrimSpace(h.saidIn(t, answer)) != said {
		t.Fatalf("/build got %s, want the thread's own words", answer)
	}
	if len(h.snapshots) != 1 || len(h.builds()) != 0 {
		t.Fatalf("%d snapshots saved and %d builds started for files Coop refused", len(h.snapshots), len(h.builds()))
	}

	// Fixing the files brings back the plain wait, and its offer.
	h.set(func() { h.answer, h.sign = errNeedsAPerson, "fixed" })
	if err := h.w.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if said := h.lastSaid(); !strings.Contains(said, "needs a build") || !strings.Contains(said, "/build") {
		t.Fatalf("after the fix the thread heard %q, want the offer back", said)
	}
	if answer := h.send(t, "5", "/build"); !strings.Contains(answer, "Review this project's box build") || len(h.snapshots) != 2 {
		t.Fatalf("/build after the fix got %s with %d snapshots, want a review", answer, len(h.snapshots))
	}
}

// A /build whose files Coop refuses to save is a refusal too: it is remembered as a host `coop build`
// remembers one, the answer says why and what to fix instead of sending the person to the agent log,
// and every thread is told at once. A failure the files did not cause still points at the log.
func TestBuildCommandRecordsAndNamesARefusalOfItsOwn(t *testing.T) {
	refused, _ := linkedDockerfileRefusal(t)
	h := newBuildCommandHarness(t)
	failure := errors.New("staging the build context: " + refused.Reason)
	h.mu.Lock()
	h.prepareErr, h.refusal = failure, refused.Reason
	h.mu.Unlock()
	h.w.mu.Lock()
	h.w.seen = "answered"
	h.w.mu.Unlock()

	answer := h.send(t, "3", "/build")
	if said := strings.TrimSpace(h.saidIn(t, answer)); said != acpBoxWaitText(refused, refused, false) {
		t.Fatalf("/build got %q, want the words a refusal found by a host build gets", said)
	}
	if strings.Contains(answer, "agent log") {
		t.Errorf("/build sent the person to the agent log: %s", answer)
	}
	h.mu.Lock()
	recorded, saved := slices.Clone(h.recorded), len(h.snapshots)
	h.mu.Unlock()
	if len(recorded) != 1 || recorded[0] != failure || saved != 0 {
		t.Errorf("recorded %v and saved %d copies of the files, want the one failure recorded and nothing saved", recorded, saved)
	}
	h.w.mu.Lock()
	seen := h.w.seen
	h.w.mu.Unlock()
	if seen != "" || len(h.w.kicks) != 1 {
		t.Errorf("seen=%q kicks=%d, want the wait to look again at once, so every thread hears it", seen, len(h.w.kicks))
	}
	h.read(func() {
		if !slices.Contains(h.logged, failure) {
			t.Errorf("the agent log got %v, want the whole failure", h.logged)
		}
	})

	// Some other failure is no refusal: nothing to remember, and the reason is in the log.
	select {
	case <-h.w.kicks:
	default:
	}
	other := errors.New("no space left on device")
	h.mu.Lock()
	h.prepareErr, h.refusal = other, ""
	h.mu.Unlock()
	h.w.mu.Lock()
	h.w.seen = "answered"
	h.w.mu.Unlock()
	answer = h.send(t, "4", "/build")
	if said := h.saidIn(t, answer); !strings.Contains(said, "could not save this project's build files") || !strings.Contains(said, "agent log") || strings.Contains(said, "refused") {
		t.Errorf("a failure the files did not cause got %q", said)
	}
	h.w.mu.Lock()
	seen = h.w.seen
	h.w.mu.Unlock()
	if seen != "answered" || len(h.w.kicks) != 0 {
		t.Errorf("seen=%q kicks=%d, want a failure that is no refusal to leave the wait alone", seen, len(h.w.kicks))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.recorded) != 2 || h.recorded[1] != other {
		t.Errorf("recorded %v, want every failure put to the record, which keeps only refusals", h.recorded)
	}
}

// The same, through the real saving of the build files and the real record: a project whose Dockerfile
// is a link gets, from its thread's /build, the words `coop build` gives it.
func TestBuildCommandOnALinkedDockerfileGivesWhatCoopBuildGives(t *testing.T) {
	host, repo := linkedDockerfileRefusal(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	a := &app{cfg: &config.Config{BoxHome: t.TempDir(), ConfigDir: t.TempDir()}, rt: runtime.Runtime{Name: fakeDocker(t)}}
	w := a.newACPBoxWait(&acpctl.Control{}, &acpproxy.Restarter{}, box.RunSpec{Repo: repo}, func() {}, func() {})
	t.Cleanup(w.cancel)
	w.log = func(error) {}
	said := w.offerBuild("coop build --egress filtered")
	if want := acpBoxWaitText(host, host, false); said != want {
		t.Fatalf("/build got\n%s\nwant what a refusal found by `coop build` says:\n%s", said, want)
	}
	for _, want := range []string{"Dockerfile is a symbolic link. Replace it with the file it points to.", "After you fix it, build it again."} {
		if !strings.Contains(said, want) {
			t.Errorf("/build got %q, want %q in it", said, want)
		}
	}
	if strings.Contains(said, "agent log") {
		t.Errorf("/build sent the person to the agent log: %s", said)
	}
	threadtext.Check(t, said)
}

// The proxy runs the command under the lock that telling the threads takes, so the command itself must
// never tell them: the editor would stop being read for good.
func TestBuildCommandNeverTellsTheThreadsWhileTheProxyWaitsOnIt(t *testing.T) {
	h := newBuildCommandHarness(t)
	var controlMu sync.Mutex
	told := make(chan string, 4)
	h.w.publish = func(text string) {
		controlMu.Lock() // what restarter.TellHeld takes
		defer controlMu.Unlock()
		told <- text
	}
	run := func(id, text string) string {
		t.Helper()
		answered := make(chan string, 1)
		controlMu.Lock()
		go func() { answered <- h.send(t, id, text) }()
		select {
		case answer := <-answered:
			controlMu.Unlock()
			return answer
		case <-time.After(3 * time.Second):
			t.Fatalf("%q waited on the lock the proxy holds while it runs", text)
			return ""
		}
	}
	code := buildCodePattern.FindStringSubmatch(run("3", "/build"))[1]
	run("4", "/build "+code)
	select {
	case text := <-told:
		if text != acpReviewedBuildText {
			t.Fatalf("the threads heard %q", text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the other threads never heard that the box is building")
	}
	h.result <- nil
	h.settled(t)
}

// A code that arrives as the session closes builds nothing, and nothing outlives the close.
func TestBuildCommandAfterCloseBuildsNothing(t *testing.T) {
	h := newBuildCommandHarness(t)
	code := buildCodePattern.FindStringSubmatch(h.send(t, "3", "/build"))[1]
	h.w.mu.Lock()
	h.w.closed = true // close has begun: done.Wait may already be running
	h.w.mu.Unlock()
	if answer := h.w.buildOffered(code); !strings.Contains(answer, "Nothing was built") {
		t.Fatalf("a code during close got %q", answer)
	}
	if answer := h.w.offerBuild("coop build --egress filtered"); !strings.Contains(answer, "closing") {
		t.Fatalf("/build during close got %q", answer)
	}
	if len(h.builds()) != 0 {
		t.Fatal("a build started during close")
	}
	if h.snapshots[1].closes() != 1 {
		t.Fatal("a snapshot saved during close was kept")
	}
}
