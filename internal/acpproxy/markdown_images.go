package acpproxy

import (
	"bytes"
	"encoding/base64"
	"math"
	"strings"
)

// Zed loads an image in an agent's Markdown by itself, with no click: from a remote address, past a
// filtered session's network rules, or from a project path, which a box can make a symlink that the editor
// follows on the host. imageEscaper takes that out of the Markdown and leaves everything else as it was.
// Escaping every `!` would show a backslash in `vec![1, 2]`, and parsing blocks to find the code would trust
// a parser that can disagree with the editor's, so this knows only the two things that can name an image:
//   - `![text](address` gets a backslash before the `!`, which leaves a link that needs a click;
//   - a definition, `[label]: address`, gets a backslash before the `:`, because then `![label]` and
//     `![a][label]` have nothing to load. A label starts a line, after any indent and container markers,
//     which keeps `a[0]: b` and `x[i]: int` as they are.
//
// Only `![text](address` with image data Zed decodes by itself stays an image: see readAddress. No definition
// stays, whatever its address, because parsers end one differently and a second can start where one ended.
//
// A box can split all this across chunks anywhere, so text that could still become one of these is held
// back until the next text decides it. The text after a pause may be a new Markdown, so it starts a line.

// maxHeldText bounds what a stream holds back. An image needs a closing bracket and a parser takes a
// long one, so text that has not decided by here is treated as an image: the worst of being wrong is a
// backslash in a very long `vec![`, or a link in place of a large `data:` image.
const maxHeldText = 16 << 10

// beforeLabel holds every byte that can stand between a line's start and a definition's label: an indent of
// any depth, and quote and list markers. Taking them in any order costs a backslash in `**[a]: b` at worst,
// and asks nothing of a parser's idea of what a list marker is.
var beforeLabel = []byte(" \t\v\f>-*+.)0123456789")

// imageFormats are the types Zed decodes that cannot refer to anything else, which rules out SVG.
var imageFormats = [][]byte{
	[]byte("data:image/png;base64,"),
	[]byte("data:image/jpeg;base64,"),
	[]byte("data:image/gif;base64,"),
	[]byte("data:image/webp;base64,"),
}

// textEnd says what the end of the text read so far means.
type textEnd int

const (
	moreText  textEnd = iota // more of this text comes: hold what it could still change
	lastText                 // nothing can follow, as with a tool's title: what is open stays harmless
	pauseText                // the stream may go on after this: send it all, and guard what comes next
	sealText                 // text no escaper will see may follow, as in a list of blocks: close everything
)

type verdict int

const (
	harmless  verdict = iota
	dangerous         // could load something other than image data
	undecided         // the text ran out first
	closedEnd         // the text ran out right after the `]` that closes an image
)

type imageEscaper struct {
	held []byte
	// exposed means what was sent last ended in a raw `!` or `]` at a pause, so the next byte could still
	// finish what those began, and nothing can be added before them any more.
	exposed bool
	// imageTo is how far the last read of an image reached, and imageIs what it found. A `![` inside it
	// shares the verdict, so a box cannot make the reads quadratic with `![![![`.
	imageTo int
	imageIs bool
	// midLine means what was read of this line can no longer come before a definition's label.
	midLine bool
}

// write takes the next text of a stream and returns what is decided.
func (e *imageEscaper) write(text []byte) []byte {
	if len(text) == 0 {
		return nil
	}
	var out []byte
	if e.exposed {
		e.exposed = false
		if first := text[0]; first == '[' || first == '(' || first == ':' {
			out = append(out, '\\', first)
			text = text[1:]
			e.midLine = true
		}
	}
	e.held = append(e.held, text...)
	return append(out, e.scan(moreText)...)
}

// finish returns everything still held, as end says it should go.
func (e *imageEscaper) finish(end textEnd) []byte {
	return e.scan(end)
}

// escapeWhole rewrites a text that is complete, as end says its last bytes should go.
func escapeWhole(text string, end textEnd) string {
	if !strings.ContainsAny(text, `\![]`) {
		return text
	}
	escaper := imageEscaper{held: []byte(text)}
	return string(escaper.scan(end))
}

// scan walks the held text and returns what is decided, keeping the rest. Only a few bytes can begin
// anything: the `!` of an image, the `[` of a label at the start of a line, a `]` at the end of the text, and
// a backslash, because it hides what it precedes and adding one after an odd run would undo the run.
func (e *imageEscaper) scan(end textEnd) []byte {
	s := e.held
	limit := math.MaxInt
	if end == moreText {
		limit = maxHeldText
	}
	var out []byte
	sent := 0
	escape := func(at int) {
		out = append(append(out, s[sent:at]...), '\\')
		sent = at
	}
	i := 0
scan:
	for i < len(s) {
		if e.midLine {
			next := bytes.IndexAny(s[i:], "\\!]\r\n")
			if next < 0 {
				i = len(s)
				break
			}
			i += next
		}
		c := s[i]
		switch {
		case c == '\n' || c == '\r':
			e.midLine = false
			i++
			continue
		case e.midLine:
		case bytes.IndexByte(beforeLabel, c) >= 0:
			i++
			continue
		case c == '[':
			closing, found := readLabel(s, i, limit)
			if end == moreText && (found == undecided || found == closedEnd) {
				break scan
			}
			e.midLine = true
			if closing >= 0 {
				escape(closing + 1)
				i = closing + 2
				continue
			}
			// A label still open could close later. One that closed at the end meets the `]` case below.
			if found == dangerous || found == undecided && end != lastText {
				escape(i)
			}
			i++
			continue
		default:
			e.midLine = true
			if c != '\\' && c != '!' && c != ']' { // the first thing on a line, and it begins nothing
				i++
				continue
			}
		}
		if i == len(s)-1 { // only the next byte can say what this one is
			switch {
			case end == moreText:
				break scan
			case end == lastText:
			case c == '\\': // doubled, so the next text starts unescaped in this Markdown or a new one
				escape(i)
			case end == pauseText: // raw, because most messages that end in `!` are just messages
				e.exposed = true
			default:
				escape(i)
			}
			i++
			continue
		}
		switch c {
		case '\\':
			i++
			if next := s[i]; next != '\n' && next != '\r' { // an escaped line break still ends the line
				i++
			}
		case ']':
			i++
		case '!':
			if s[i+1] != '[' {
				i++
				continue
			}
			if i >= e.imageTo {
				found, to := readImage(s, i, limit)
				if found == undecided || found == closedEnd {
					if end == moreText {
						break scan
					}
					// A closing `]` at a pause is guarded like any other. Anything still open could close later.
					if end == lastText || end == pauseText && found == closedEnd {
						found = harmless
					} else {
						found = dangerous
					}
				}
				e.imageIs, e.imageTo = found == dangerous, to
			}
			if e.imageIs {
				escape(i)
			}
			i += 2
		}
	}
	out = append(out, s[sent:i]...)
	e.held = append(e.held[:0], s[i:]...)
	e.imageTo = max(0, e.imageTo-i)
	if end != moreText {
		e.midLine = false
	}
	return out
}

// readLabel reads what follows the `[` at s[i] and returns where the `]` of a `]:` is, or -1 and why not. A
// label holds no unescaped `[` and no blank line.
func readLabel(s []byte, i, limit int) (int, verdict) {
	for j := i + 1; j < len(s); j++ {
		if j-i >= limit {
			return -1, dangerous
		}
		switch s[j] {
		case '\\':
			j++
		case '[':
			return -1, harmless
		case ']':
			switch {
			case j+1 == len(s):
				return -1, closedEnd
			case s[j+1] == ':':
				return j, harmless
			}
			return -1, harmless
		case '\n':
			if blankLineEnd(s, j) >= 0 {
				return -1, harmless
			}
		}
	}
	return -1, undecided
}

// blankLineEnd returns where the blank line after the `\n` at s[at] ends, or -1 if none follows. Only `\n`
// counts here: a parser that takes `\r` as a line end has seen one blank line more than this, not fewer.
func blankLineEnd(s []byte, at int) int {
	k := at + 1
	for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\r') {
		k++
	}
	if k < len(s) && s[k] == '\n' {
		return k
	}
	return -1
}

// readImage reads what follows the `![` at s[i], up to where that decides whether it can load anything
// but image data, and returns how far it read.
func readImage(s []byte, i, limit int) (verdict, int) {
	depth, sure := 1, true
	for j := i + 2; j < len(s); j++ {
		if j-i >= limit {
			return dangerous, j
		}
		switch s[j] {
		case '\\':
			j++ // what it escapes is not syntax
		case '`':
			sure = false // a code span hides brackets from the parser, so counting them proves nothing
		case '<':
			// So does a tag or an autolink, but only up to its `>`. Most of these in code are like
			// `vec![T::<u8>::new()]`, which hides nothing. A quote can hold a `>`, a comment need not stop at
			// the first one, and `a < b` is no tag at all.
			rest := s[j+1:]
			switch {
			case len(rest) == 0:
			case rest[0] == '!' || rest[0] == '?':
				sure = false
			case rest[0] == '/' || rest[0]|0x20 >= 'a' && rest[0]|0x20 <= 'z':
				if end := bytes.IndexAny(rest, "`>[]\"'\n"); end >= 0 && rest[end] == '>' {
					j += end + 1
				} else {
					sure = false
				}
			}
		case '[':
			depth++
		case ']':
			if j+1 == len(s) {
				if sure && depth == 1 {
					return closedEnd, len(s)
				}
				return undecided, len(s)
			}
			if s[j+1] == '(' {
				switch readAddress(s[j+2:], limit-(j-i)) {
				case dangerous:
					return dangerous, j
				case undecided:
					return undecided, len(s)
				}
				if sure && depth == 1 {
					return harmless, j
				}
				sure = false // a link inside the text hides any brackets in its destination
			}
			depth--
			if sure && depth == 0 {
				return harmless, j
			}
		case '\n':
			// A paragraph ends at a blank line and takes any open bracket with it.
			if k := blankLineEnd(s, j); k >= 0 {
				return harmless, k
			}
		}
	}
	return undecided, len(s)
}

// readAddress reads what follows `](`. Only image data Zed decodes itself is harmless. Zed takes `data:` as
// image data only with a type it knows and base64 that decodes under strict rules. Anything else it takes
// for a project path, and a box can name a symlink `data:a`, so anything short of that exact form counts as
// a path, down to a title or a capital letter.
func readAddress(s []byte, limit int) verdict {
	i := 0
	for i < len(s) && bytes.IndexByte([]byte(" \t\r\n"), s[i]) >= 0 {
		i++
	}
	if i >= limit {
		return dangerous
	}
	s, limit = s[i:], limit-i
	if len(s) == 0 {
		return undecided
	}
	for _, format := range imageFormats {
		if len(s) < len(format) && bytes.HasPrefix(format, s) {
			return undecided
		}
		if bytes.HasPrefix(s, format) {
			return readBase64(s[len(format):], limit-len(format))
		}
	}
	return dangerous
}

// readBase64 reads image data up to the `)` that closes its address, and says if it decodes.
func readBase64(s []byte, limit int) verdict {
	end := bytes.IndexFunc(s, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=')
	})
	switch {
	case end < 0 && len(s) >= limit, end >= limit:
		return dangerous
	case end < 0:
		return undecided
	case s[end] != ')':
		return dangerous
	}
	if _, err := base64.StdEncoding.Strict().DecodeString(string(s[:end])); err != nil {
		return dangerous
	}
	return harmless
}
