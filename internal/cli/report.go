package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The column-zero twin of ui.Fail/ui.Caution: a command's OWN outcome block, as opposed to a
// result inside a launch section (which ui indents by two). Shape, identical for both marks:
//
//	✗ <headline>
//
//	      <reason>
//
//	  <action>
//
// The headline carries the mark's color and nothing else — a reason painted red is the one line
// a person must be able to read, and colour never carries the only meaning. The reason sits six
// spaces in, the actions two, exactly like ui.Fail one level out.
//
// One renderer for every block in this family so the depths cannot drift between commands. It
// lives here rather than in internal/ui only because internal/ui is being reworked alongside
// this change; fold it in when the shared error renderer lands.

// failBlock prints a failure outcome: red ✗ headline, the concrete reason, then what to do.
func failBlock(headline, reason string, actions ...string) {
	outcomeBlock(ui.Red("✗"), headline, reason, actions)
}

// warnBlock prints a non-fatal outcome the person should still read: amber ⚠, same shape.
func warnBlock(headline, reason string, actions ...string) {
	outcomeBlock(ui.Yellow("⚠"), headline, reason, actions)
}

// outcomeBlock renders one block, line by line through ui so a live view positions each line
// itself. An empty reason or an empty action list simply omits that part, so a bare headline
// stays a single line.
func outcomeBlock(mark, headline, reason string, actions []string) {
	ui.Note("%s %s", mark, headline)
	if reason != "" {
		ui.Note("")
		for _, line := range strings.Split(strings.TrimRight(reason, "\n"), "\n") {
			ui.Note("      %s", line)
		}
	}
	if len(actions) > 0 {
		ui.Note("")
		for _, line := range actions {
			ui.Note("  %s", line)
		}
	}
}

// reported renders a failure block and returns the sentinel the dispatcher recognises, so the
// generic "✗ <err>" line never repeats what the block already said.
func reported(headline, reason string, actions ...string) error {
	failBlock(headline, reason, actions...)
	return ui.ErrReported
}

// runtimeTitle is a container runtime's name as a person writes it — "Docker", "Apple
// container" — for the prose coop speaks about it. An explicit COOP_RUNTIME pointing at something
// else keeps its own spelling: coop does not know a nicer name for it.
func runtimeTitle(name string) string {
	// An explicit COOP_RUNTIME can be an absolute path to the same program, and the person who
	// set it still calls it Docker.
	switch filepath.Base(name) {
	case "docker":
		return "Docker"
	case "container":
		return "Apple container"
	}
	return name
}

// runtimeUnavailable reports whether err is the container runtime being installed but not
// answering — the one failure whose remedy is "start it", not "fix your command".
func runtimeUnavailable(err error) bool { return errors.Is(err, runtime.ErrDaemonUnavailable) }

// firstLine is the one sentence of a runtime/OS error worth putting in a reason slot: a bounded,
// single line. A compose or Docker failure can arrive with a page of diagnostics attached — that
// page is already on the terminal as the runtime's own output, and repeating its tail inside the
// block turns a readable outcome into a wall.
func firstLine(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	return msg
}

// pathReason renders a filesystem failure as the one sentence a reason slot holds: what coop was
// doing ("read", "create"), the path made repo-relative so it reads like the rest of the report,
// and the OS's own cause. The verb belongs to the caller — a scaffold that could not CREATE a file
// and a scan that could not READ one are different facts, and saying the wrong one sends the
// reader to the wrong place.
func pathReason(repo, verb string, err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		shown := pathErr.Path
		if rel, relErr := filepath.Rel(repo, pathErr.Path); relErr == nil && !strings.HasPrefix(rel, "..") {
			shown = filepath.ToSlash(rel)
		}
		return fmt.Sprintf("Could not %s %s: %s.", verb, shown, pathErr.Err)
	}
	return sentence(err.Error())
}

// sentence renders a reason as a sentence: capitalised, ending in a period. Go errors are lower
// case and unpunctuated by convention; a reason slot is prose a person reads.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] = r[0] - 'a' + 'A'
	}
	s = string(r)
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "!") && !strings.HasSuffix(s, "?") {
		s += "."
	}
	return s
}
