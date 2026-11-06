package cli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/acpproxy"
	"github.com/AndrewDryga/coop/internal/box"
)

// A filtered session waiting on build files nobody approved can be built from its thread, in two steps as
// in a terminal: /build saves the files and shows where to review them, and /build <code> builds exactly
// what was saved. The code is random and kept only here, so nothing that can write the checkout can know
// it in advance; it works once, for acpBuildCodeLife, and only for the files it was shown with.
const (
	acpBuildCommand  = "/build"
	acpBuildCodeLife = 10 * time.Minute
	// acpBuildPrepareLimit bounds saving the build files, which runs while the editor's next line waits.
	acpBuildPrepareLimit = 30 * time.Second
)

// acpPreparedBuild is the reviewed snapshot a code builds: box.PreparedProjectBuild.
type acpPreparedBuild interface {
	Directory() string
	Dockerfile() string
	Close() error
}

// acpBuildOffer is the one snapshot a session's thread was last shown, and the code that builds it.
type acpBuildOffer struct {
	prepared acpPreparedBuild
	code     string
	until    time.Time
}

// heldCommand is acpproxy.Hooks.HeldCommand: it answers /build while this session waits on build files
// nobody approved, and lets every other prompt be held as usual. It runs under the proxy's controlMu, so
// nothing it calls may tell the threads (publish): that takes controlMu too.
func (w *acpBoxWait) heldCommand(session string, line []byte) []byte {
	if w == nil || !w.filtered || w.prepareBuild == nil {
		return nil
	}
	id, args, ok := acpBuildPrompt(line)
	if !ok {
		return nil
	}
	w.mu.Lock()
	var required *box.ProjectBuildRequired
	waiting := errors.As(w.blocked, &required) && !required.Approved && !w.closed
	building, reviewing := w.building, w.reviewing
	w.mu.Unlock()
	var text string
	switch {
	case !waiting:
		return nil // nothing to build from here: it is an ordinary message
	case building && reviewing:
		text = acpReviewedBuildText
	case building:
		text = acpRebuildingText
	case len(args) == 0:
		text = w.offerBuild(required.BuildCommand())
	default:
		text = w.buildOffered(args[0])
	}
	return append(acpproxy.SessionMessageLine(session, text), acpPromptEndTurn(id)...)
}

// offerBuild saves the build files as they are now and says where to review them. A new offer replaces
// the last one, whose code stops working. command builds them on the host instead.
func (w *acpBoxWait) offerBuild(command string) string {
	ctx, cancel := context.WithTimeout(w.ctx, acpBuildPrepareLimit)
	defer cancel()
	prepared, err := w.prepareBuild(ctx)
	if err != nil {
		w.log(err)
		return acpctl.ProblemText(acpctl.Problem{Title: "Coop could not save this project's build files for review.",
			Detail:  "The reason is in the agent log. Your messages go out once the box is ready.",
			Command: command, Purpose: "in this project, on the host"})
	}
	dir, dockerfile := prepared.Directory(), prepared.Dockerfile()
	if strings.Contains(dir+dockerfile, "`") || acpproxy.Visible(dir+dockerfile) != dir+dockerfile {
		_ = prepared.Close()
		return acpctl.ProblemText(acpctl.Problem{Title: "Build this project's box on the host.",
			Detail:  "Coop cannot show where its saved build files are in this thread.",
			Command: command, Purpose: "in this project, on the host"})
	}
	code := strings.ToLower(rand.Text()[:8])
	w.mu.Lock()
	closed := w.closed
	previous := w.offer
	if !closed {
		w.offer = &acpBuildOffer{prepared: prepared, code: code, until: w.now().Add(acpBuildCodeLife)}
		w.reviewFailed = false
	}
	w.mu.Unlock()
	if closed {
		_ = prepared.Close()
		return "**This session is closing.**\n\nNothing was built."
	}
	if previous != nil {
		_ = previous.prepared.Close()
	}
	return "**Review this project's box build.**\n\n" +
		"Coop saved the build files as they are now. The Dockerfile is here, on this computer, beside the files it copies:\n\n" +
		acpctl.CodeBlock(filepath.Join(dir, dockerfile)) + "\n\n" +
		"The build runs them with open networking, and your agent then runs in the box they make. " +
		"Read them there. To build exactly these saved files, send this within 10 minutes:\n\n" +
		acpctl.CodeBlock(acpBuildCommand+" "+code) + "\n\n" +
		"Edits made after this are not part of it. Send " + acpBuildCommand + " again to save the files anew."
}

// buildOffered builds the offered snapshot if code is the one shown with it. Anything else builds nothing.
func (w *acpBoxWait) buildOffered(code string) string {
	w.mu.Lock()
	offer := w.offer
	expired := offer != nil && !w.now().Before(offer.until)
	valid := offer != nil && !expired && !w.closed && subtle.ConstantTimeCompare([]byte(strings.ToLower(code)), []byte(offer.code)) == 1
	if valid || expired {
		w.offer = nil // it works once, and not at all once it is old
	}
	if valid {
		w.building, w.reviewing = true, true
		w.said = acpReviewedBuildText
		w.done.Add(1) // close waits on done only after it sets closed, which valid checked under mu
	}
	w.mu.Unlock()
	if expired {
		_ = offer.prepared.Close()
	}
	if !valid {
		return "**Nothing was built.**\n\nThat code does not match the build files Coop saved for this thread, or it is more than 10 minutes old. " +
			"Send " + acpBuildCommand + " to save them again and get a new code."
	}
	go w.runReviewedBuild(offer.prepared)
	return acpReviewedBuildText
}

// acpBuildableWaitText is what a filtered session's threads hear while its build files wait for a review.
// command builds them on the host instead.
func acpBuildableWaitText(failed bool, command string) string {
	const waits = "Your messages go out once it is ready."
	if failed {
		return acpctl.ProblemText(acpctl.Problem{Title: "Coop could not build this project's box.",
			Detail: "The reason is in the agent log. Fix it, then send " + acpBuildCommand + " to review the files again and build them. " + waits})
	}
	return acpctl.ProblemText(acpctl.Problem{Title: "This project's box needs a build.",
		Detail: "Nobody approved its build files as they are now, and a build runs them with open networking. " +
			"To review the Dockerfile and the files it copies, then build them from here, send " + acpBuildCommand + ". " + waits,
		Command: command, Purpose: "in this project, on the host, to do it there instead"})
}

// acpReviewedBuildText is what threads hear while the box builds from files a person reviewed here.
const acpReviewedBuildText = "**Building this project's box from the files you reviewed.**\n\nYour messages go out once it is ready."

// runReviewedBuild builds one reviewed snapshot, like runRebuild does an approved one. The session
// closing cancels it, and the snapshot is gone afterwards whatever happened.
func (w *acpBoxWait) runReviewedBuild(prepared acpPreparedBuild) {
	defer w.done.Done()
	w.publish(acpReviewedBuildText) // the other threads; once the command's answer has let go of controlMu
	err := w.buildReviewed(w.ctx, prepared)
	err = errors.Join(err, prepared.Close())
	failed := err != nil && w.ctx.Err() == nil
	if failed {
		w.log(err)
	}
	w.mu.Lock()
	w.building, w.reviewing, w.reviewFailed = false, false, failed
	w.seen = "" // ask again at once
	w.mu.Unlock()
	w.kick()
}

// dropOffer closes a snapshot nobody built, once the wait it was for is over.
func (w *acpBoxWait) dropOffer() {
	w.mu.Lock()
	offer := w.offer
	w.offer = nil
	w.mu.Unlock()
	if offer != nil {
		_ = offer.prepared.Close()
	}
}

// acpBuildPrompt reads a session/prompt that is only /build, maybe with a code: its request id and the
// words after the command.
func acpBuildPrompt(line []byte) (json.RawMessage, []string, bool) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Params struct {
			Prompt []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"prompt"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil || len(m.ID) == 0 || len(m.Params.Prompt) != 1 || m.Params.Prompt[0].Type != "text" {
		return nil, nil, false
	}
	fields := strings.Fields(m.Params.Prompt[0].Text)
	if len(fields) == 0 || len(fields) > 2 || fields[0] != acpBuildCommand {
		return nil, nil, false
	}
	return m.ID, fields[1:], true
}

func acpPromptEndTurn(id json.RawMessage) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + string(id) + `,"result":{"stopReason":"end_turn"}}` + "\n")
}
