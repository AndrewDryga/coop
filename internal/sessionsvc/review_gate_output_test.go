package sessionsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// A red review said only "gate failed": what the checks printed went to the
// worker's own log, which only the worker's operator can read, so Ryker's fix
// round could not tell which test failed (emisar, 2026-09-28). Andrew: "Ryker
// should get full access to errors, warnings and all other output to work, like
// any llm model would, it's a sandbox!!"

func reviewGateOutputFixture(t *testing.T, task string, gate ReviewGateFunc) (*sessionFixture, session.Session) {
	t.Helper()
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newReviewTestService(t, repo, 1<<20, gate)
	t.Cleanup(func() { _ = service.Stop() })
	sess := createReviewSession(t, service, task)
	if err := os.WriteFile(filepath.Join(sess.Workspace, "change.txt"), []byte("reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, sess.Workspace, "add", "change.txt")
	sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "review change")
	return service, sess
}

func readWholeReviewGateOutput(t *testing.T, service *sessionFixture, sessionID, operationID string) (string, ReviewGateOutputPage, int) {
	t.Helper()
	var whole strings.Builder
	cursor, pages := "", 0
	for {
		page, err := service.ReadReviewGateOutput(context.Background(), sessionID, operationID, cursor)
		if err != nil {
			t.Fatalf("read page %d from %q: %v", pages+1, cursor, err)
		}
		pages++
		if !utf8.ValidString(page.Output) {
			t.Fatalf("page %d splits a character", pages)
		}
		whole.WriteString(page.Output)
		if page.NextCursor == "" {
			return whole.String(), page, pages
		}
		if pages > 64 {
			t.Fatal("the output never ends")
		}
		cursor = page.NextCursor
	}
}

func TestAFailedReviewKeepsEverythingTheGatePrintedForItsController(t *testing.T) {
	var printed bytes.Buffer
	for line := 0; printed.Len() < 3<<20; line++ {
		stream := "stdout"
		if line%7 == 0 {
			stream = "stderr"
		}
		fmt.Fprintf(&printed, "%s: test %d … expected ✓, got ✗\n", stream, line)
	}
	exit := 2
	service, sess := reviewGateOutputFixture(t, "gate-output-red", func(_ context.Context, request ReviewGateRequest) (ReviewGateResult, error) {
		if request.Output == nil {
			t.Error("the gate was given nowhere to print")
			return ReviewGateResult{Configured: true}, nil
		}
		if _, err := io.Copy(request.Output, bytes.NewReader(printed.Bytes())); err != nil {
			t.Error(err)
		}
		return ReviewGateResult{Configured: true, Command: []string{"./run", "gate", "review"}, ExitCode: &exit}, nil
	})

	dossier, err := service.RunReview(context.Background(), "review-gate-output-red", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if dossier.Gate != ReviewGateFailed {
		t.Fatalf("gate = %s, want failed", dossier.Gate)
	}
	output := dossier.GateOutput
	if output == nil || strings.Join(output.Command, " ") != "./run gate review" || output.ExitCode == nil ||
		*output.ExitCode != 2 || output.Bytes != int64(printed.Len()) || !output.Complete || output.Lost != "" {
		t.Fatalf("the review says of its gate's output %+v", output)
	}

	whole, last, pages := readWholeReviewGateOutput(t, service, sess.ID, dossier.OperationID)
	if whole != printed.String() {
		t.Fatalf("read %d bytes over %d pages, want the %d the gate printed", len(whole), pages, printed.Len())
	}
	if pages < 3 || !last.Complete || last.Bytes != int64(printed.Len()) {
		t.Fatalf("%d pages, last %+v", pages, last)
	}

	// The output is the job's own: no other session reads it.
	other := createReviewSession(t, service, "gate-output-other")
	if _, err := service.ReadReviewGateOutput(context.Background(), other.ID, dossier.OperationID, ""); err == nil {
		t.Fatal("another session read this review's output")
	}
	for _, cursor := range []string{"-1", "x", fmt.Sprint(printed.Len() + 1)} {
		if _, err := service.ReadReviewGateOutput(context.Background(), sess.ID, dossier.OperationID, cursor); session.CodeOf(err) != session.CodeInvalidRequest {
			t.Fatalf("cursor %q = %v, want an invalid request", cursor, err)
		}
	}
}

// Warnings on a green gate matter too: the output is kept whatever the result.
func TestAPassingReviewKeepsItsGatesWarnings(t *testing.T) {
	exit := 0
	service, sess := reviewGateOutputFixture(t, "gate-output-green", func(_ context.Context, request ReviewGateRequest) (ReviewGateResult, error) {
		fmt.Fprintln(request.Output, "warning: variable \"unused\" is unused")
		return ReviewGateResult{Configured: true, Passed: true, Command: []string{"mix", "test"}, ExitCode: &exit}, nil
	})
	dossier, err := service.RunReview(context.Background(), "review-gate-output-green", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil || dossier.Gate != ReviewGatePassed {
		t.Fatalf("review = %+v, %v", dossier.Gate, err)
	}
	whole, _, _ := readWholeReviewGateOutput(t, service, sess.ID, dossier.OperationID)
	if whole != "warning: variable \"unused\" is unused\n" {
		t.Fatalf("kept %q", whole)
	}
}

// A gate that prints without end cannot fill the worker's disk, and the review
// says the output was cut instead of passing the beginning off as all of it.
func TestAReviewSaysWhenItsGatePrintedMoreThanCoopKeeps(t *testing.T) {
	previous := maxReviewGateOutputBytes
	maxReviewGateOutputBytes = 1 << 10
	t.Cleanup(func() { maxReviewGateOutputBytes = previous })
	service, sess := reviewGateOutputFixture(t, "gate-output-flood", func(_ context.Context, request ReviewGateRequest) (ReviewGateResult, error) {
		for range 100 {
			fmt.Fprintln(request.Output, strings.Repeat("x", 99))
		}
		return ReviewGateResult{Configured: true}, nil
	})
	dossier, err := service.RunReview(context.Background(), "review-gate-output-flood", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	output := dossier.GateOutput
	if output == nil || output.Complete || output.Bytes != 1<<10 || !strings.Contains(output.Incomplete, "1 KiB") {
		t.Fatalf("a cut output reads %+v", output)
	}
	whole, last, _ := readWholeReviewGateOutput(t, service, sess.ID, dossier.OperationID)
	if len(whole) != 1<<10 || last.Complete || last.Incomplete != output.Incomplete {
		t.Fatalf("read %d bytes, last page %+v", len(whole), last)
	}
}

// A gate that could not start still leaves what Coop printed while starting it,
// and the start failure itself is no longer hidden from the controller.
func TestAReviewWhoseGateCouldNotStartKeepsWhatItPrinted(t *testing.T) {
	service, sess := reviewGateOutputFixture(t, "gate-output-startup", func(_ context.Context, request ReviewGateRequest) (ReviewGateResult, error) {
		fmt.Fprintln(request.Output, "pulling the check image")
		return ReviewGateResult{Configured: true, StartupError: "a gate is set but image \"coop-box\" isn't built"}, nil
	})
	dossier, err := service.RunReview(context.Background(), "review-gate-output-startup", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil || dossier.Gate != ReviewGateStartupError {
		t.Fatalf("review = %+v, %v", dossier.Gate, err)
	}
	if public := publicReview(dossier); !strings.Contains(public.GateError, "isn't built") {
		t.Fatalf("the controller reads the start failure as %q", public.GateError)
	}
	whole, _, _ := readWholeReviewGateOutput(t, service, sess.ID, dossier.OperationID)
	if whole != "pulling the check image\n" {
		t.Fatalf("kept %q", whole)
	}
}

// Discarding the session removes the output with the rest of the review, and a
// read afterwards says the output is gone rather than returning nothing.
func TestAReviewsGateOutputLeavesWithItsSession(t *testing.T) {
	service, sess := reviewGateOutputFixture(t, "gate-output-discard", func(_ context.Context, request ReviewGateRequest) (ReviewGateResult, error) {
		fmt.Fprintln(request.Output, "1 test, 1 failure")
		return ReviewGateResult{Configured: true}, nil
	})
	dossier, err := service.RunReview(context.Background(), "review-gate-output-discard", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	path, err := service.reviewGateOutputPath(dossier.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no kept output: %v", err)
	}
	ctx := context.Background()
	closed, err := service.Close(ctx, "close-gate-output", session.CloseSessionRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanDiscard(ctx, "plan-gate-output", PlanDiscardRequest{SessionID: sess.ID, ExpectedRevision: closed.Revision, AcceptUnmerged: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Discard(ctx, "discard-gate-output", DiscardRequest{PlanOperationID: plan.OperationID}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the output outlived its session: %v", err)
	}
	page, err := service.ReadReviewGateOutput(context.Background(), sess.ID, dossier.OperationID, "")
	if err != nil || page.Lost == "" || page.Output != "" {
		t.Fatalf("a read after removal = %+v, %v", page, err)
	}
}

// Pages end on character boundaries, so a character split across two pages is
// never read as two replacement characters.
func TestReviewGateOutputPagesEndOnACharacter(t *testing.T) {
	previous := reviewGateOutputPageBytes
	reviewGateOutputPageBytes = 5
	t.Cleanup(func() { reviewGateOutputPageBytes = previous })
	service, sess := reviewGateOutputFixture(t, "gate-output-runes", func(_ context.Context, request ReviewGateRequest) (ReviewGateResult, error) {
		fmt.Fprint(request.Output, "ab✗cd✓e")
		return ReviewGateResult{Configured: true}, nil
	})
	dossier, err := service.RunReview(context.Background(), "review-gate-output-runes", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	whole, _, pages := readWholeReviewGateOutput(t, service, sess.ID, dossier.OperationID)
	if whole != "ab✗cd✓e" || pages < 3 {
		t.Fatalf("read %q over %d pages", whole, pages)
	}
}

// What the controller reads on the wire: a page with the cursor of the next,
// null on the last, and lost alone when nothing was kept.
func TestReviewGateOutputOverHTTP(t *testing.T) {
	previous := reviewGateOutputPageBytes
	reviewGateOutputPageBytes = 8
	t.Cleanup(func() { reviewGateOutputPageBytes = previous })
	service, sess := reviewGateOutputFixture(t, "gate-output-http", func(_ context.Context, request ReviewGateRequest) (ReviewGateResult, error) {
		fmt.Fprint(request.Output, "1 test, 1 failure\n")
		return ReviewGateResult{Configured: true}, nil
	})
	dossier, err := service.RunReview(context.Background(), "review-gate-output-http", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandler(service.Service)
	path := "/v1/sessions/" + sess.ID + "/reviews/" + dossier.OperationID + "/gate-output"
	read := func(query string) map[string]any {
		t.Helper()
		response := sessionHTTPTestRequest(t, handler, http.MethodGet, path+query, "", "", "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", query, response.Code, response.Body.String())
		}
		var page map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	first := read("")
	if first["output"] != "1 test, " || first["next_cursor"] != "8" || first["bytes"] != float64(18) || first["complete"] != true {
		t.Fatalf("first page = %v", first)
	}
	last := read("?cursor=16")
	if next, present := last["next_cursor"]; last["output"] != "e\n" || !present || next != nil {
		t.Fatalf("last page = %v", last)
	}
	if response := sessionHTTPTestRequest(t, handler, http.MethodGet, path+"?offset=0", "", "", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("an unknown query parameter = %d", response.Code)
	}
	if public := publicReview(dossier); public.GateOutput == nil || public.GateOutput.Bytes != 18 {
		t.Fatalf("the review on the wire says of its output %+v", public.GateOutput)
	}

	// A review that ran no check says so, with nothing else.
	none, noneSession := reviewGateOutputFixture(t, "gate-output-none", func(context.Context, ReviewGateRequest) (ReviewGateResult, error) {
		return ReviewGateResult{}, nil
	})
	noneReview, err := none.RunReview(context.Background(), "review-gate-output-none", RunReviewRequest{SessionID: noneSession.ID, ExpectedRevision: noneSession.Revision})
	if err != nil || noneReview.GateOutput != nil {
		t.Fatalf("a review without a gate kept %+v, %v", noneReview.GateOutput, err)
	}
	response := sessionHTTPTestRequest(t, NewHTTPHandler(none.Service), http.MethodGet,
		"/v1/sessions/"+noneSession.ID+"/reviews/"+noneReview.OperationID+"/gate-output", "", "", "")
	var lost map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &lost); err != nil || response.Code != http.StatusOK || len(lost) != 1 ||
		!strings.Contains(fmt.Sprint(lost["lost"]), "no check") {
		t.Fatalf("a review without a gate reads %d %s", response.Code, response.Body.String())
	}
}

// A start failure's words reach the controller; the worker's paths do not.
func TestAReviewsStartFailureKeepsItsWordsButNotTheWorkersPaths(t *testing.T) {
	for detail, want := range map[string]string{
		"docker: command not found":                                  "docker: command not found",
		`a gate is set but image "coop-box" isn't built`:             `a gate is set but image "coop-box" isn't built`,
		"open /var/lib/coop/sessions/r1/job.json: permission denied": "open <path>: permission denied",
		"/secret/host/path":                                          "<path>",
		`exec "/usr/bin/make" failed (see /tmp/x.log)`:               `exec "<path>" failed (see <path>)`,
	} {
		if got := publicReview(ReviewDossier{GateError: detail}).GateError; got != want {
			t.Errorf("%q reads %q, want %q", detail, got, want)
		}
	}
}
