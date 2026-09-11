package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// captureTerminal records what a person at a terminal would SEE: coop's own lines and the
// runtime's own output, interleaved on one stream in the order they were written. A transcript
// that captured only stderr would drop the Compose output the services fixtures are laid out
// around, and one that captured only stdout would drop every line coop speaks.
func captureTerminal(t *testing.T, fn func()) string {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = w, w
	fn()
	_ = w.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	out, _ := io.ReadAll(r)
	return string(out)
}

// typedAnswers installs the answers a person would type at init's questions, through the same seam
// ui.confirmInput uses for a confirmation. It is what makes the complete first-run transcripts
// reachable: without a real terminal there is nothing to read a reply from.
func typedAnswers(t *testing.T, answers ...string) {
	t.Helper()
	t.Cleanup(stub(&initInput, io.Reader(&typingTerminal{lines: answers})))
}

// typingTerminal stands in for the tty on both halves of a question: it hands back ONE line per
// read, so a scanner never runs ahead of the prompt that asked for it, and it echoes that line the
// way a terminal echoes what you type — which is why the answers appear in the transcript at all.
// Coop never writes them; the terminal does.
type typingTerminal struct{ lines []string }

func (r *typingTerminal) Read(p []byte) (int, error) {
	if len(r.lines) == 0 {
		return 0, io.EOF // a closed stdin: every prompt reads it as "none" and stops asking
	}
	line := r.lines[0] + "\n"
	r.lines = r.lines[1:]
	fmt.Fprint(os.Stderr, line)
	return copy(p, line), nil
}

// typed is the list of replies a table-driven case hands to the terminal, named so a case reads
// as the session it produces.
func typed(lines ...string) []string { return lines }

// assertApprovedSession is assertApprovedOutput for a transcript that contains PROMPTS. A prompt
// ends with a space, because the cursor sits after it while you type — and a text file cannot
// carry a trailing space through an editor that strips them. Comparing on the trimmed-right form
// of each line keeps the fixture an honestly editable file; nothing else coop prints ends in a
// space, so no real drift hides behind this.
func assertApprovedSession(t *testing.T, name, got string) {
	t.Helper()
	assertApprovedOutput(t, name, trimLineEnds(got))
}

// trimLeadingBlank drops the blank line a result begins with when the fixture starts at the
// result itself — in a session that newline separates it from the command line above.
func trimLeadingBlank(out string) string { return strings.TrimPrefix(out, "\n") }

func trimLineEnds(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	return strings.Join(lines, "\n")
}
