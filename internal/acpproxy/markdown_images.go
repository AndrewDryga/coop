package acpproxy

import (
	"bytes"
	"math"
	"strings"
)

// Zed fetches a remote image in an agent's Markdown by itself, with no click and past a filtered
// session's network rules. imageEscaper takes that out of the Markdown and leaves everything else as it
// was. Escaping every `!` would show a backslash in `vec![1, 2]`, and parsing blocks to find the code
// would trust a parser that can disagree with the editor's, so this knows only the two things that can
// point an image at an address, wherever they stand:
//   - `![text](address` gets a backslash before the `!`, which leaves a link that needs a click;
//   - `]: address` gets a backslash before the `:`, which is all it takes to stop being a definition, so
//     the lists, quotes, indents and footnotes around one do not matter. Without one, `![a]` and
//     `![a][b]` have nothing to load.
//
// Either way only an address that could be remote counts: an image of a local path stays one.
//
// A box can split all this across chunks anywhere, so text that could still become one of these is held
// back until the next text decides it.

// maxHeldText bounds what a stream holds back. An image needs a closing bracket and a parser takes a
// long one, so text that has not decided by here is treated as an image: the worst of being wrong is a
// backslash in a very long `vec![`.
const maxHeldText = 16 << 10

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
	dangerous         // could reach a remote address
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
	if !strings.ContainsAny(text, `\!]`) {
		return text
	}
	escaper := imageEscaper{held: []byte(text)}
	return string(escaper.scan(end))
}

// scan walks the held text and returns what is decided, keeping the rest. Only three bytes can begin
// anything: the `!` and `]` of the two rules, and a backslash, because it hides what it precedes and
// adding one after an odd run would undo the run.
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
		next := bytes.IndexAny(s[i:], `\!]`)
		if next < 0 {
			i = len(s)
			break
		}
		i += next
		c := s[i]
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
			i += 2
		case ']':
			if s[i+1] != ':' {
				i++
				continue
			}
			switch readAddress(s[i+2:], limit) {
			case dangerous:
				escape(i + 1)
			case undecided:
				if end == moreText {
					break scan
				}
				if end != lastText { // the rest of the address may still come
					escape(i + 1)
				}
			}
			i += 2
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
	return out
}

// readImage reads what follows the `![` at s[i], up to where that decides whether it can reach a remote
// address, and returns how far it read.
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
			// A paragraph ends at a blank line and takes any open bracket with it. Only `\n` counts here:
			// a parser that takes `\r` as a line end has seen one blank line more than this, not fewer.
			k := j + 1
			for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\r') {
				k++
			}
			if k < len(s) && s[k] == '\n' {
				return harmless, k
			}
		}
	}
	return undecided, len(s)
}

// readAddress reads what follows `](` or `]:`. Only what can decode to http:// or https:// can reach a
// remote address, which keeps `![logo](logo.png)`, a TypeScript `[key: string]: number;` and a Python
// `if a[0]:` as they are.
func readAddress(s []byte, limit int) verdict {
	i := 0
	// An address may be on the next line, and in a quote that line starts with `>`. Going past a blank
	// line too only ever finds more, never less.
	for i < len(s) && bytes.IndexByte([]byte(" \t\v\f\r\n>"), s[i]) >= 0 {
		i++
	}
	if i >= limit {
		return dangerous
	}
	if i == len(s) {
		return undecided
	}
	switch s[i] {
	case '<', '&', '/', '\\': // brackets can hold spaces, an entity can be the first letter, a path can be a mount, `\/` is one
		return dangerous
	case 'h', 'H':
		for ; i < len(s) && s[i] > ' '; i++ {
			// The `:` can be written as itself, as an entity or with a backslash.
			if s[i] == ':' || s[i] == '&' || s[i] == '\\' || i >= limit {
				return dangerous
			}
		}
		if i == len(s) {
			return undecided
		}
	}
	return harmless
}
