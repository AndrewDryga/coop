package box

import (
	"io"
	"os"

	"github.com/AndrewDryga/coop/internal/ui"
)

// buildOutput is where a build's own output goes: unchanged to a terminal that shows color, and
// without escape sequences anywhere else. Docker colors its build-check warnings ("1 warning
// found") whatever NO_COLOR says and wherever its output goes, and no Docker setting turns that
// off, so a redirected `coop build` logged raw ESC[33m codes (QA, 2026-11-06).
func buildOutput(w io.Writer) io.Writer {
	if w == nil || w == io.Discard {
		return w
	}
	if f, ok := w.(*os.File); ok && ui.For(f).Enabled() {
		return w
	}
	return &plainWriter{w: w}
}

// plainWriter passes text on without terminal escape sequences: a CSI sequence (colors, cursor
// moves), a string sequence such as a hyperlink up to its terminator, and the short escapes. A
// sequence can be split across writes, so the state carries over. A byte that cannot continue a
// sequence ends it and stays, and a line break always ends one, so a broken sequence never
// swallows the rest of a build log.
type plainWriter struct {
	w     io.Writer
	state int
}

const (
	plainText         = iota
	plainEscape       // after ESC
	plainIntermediate // after ESC and an intermediate byte, until a final byte
	plainCSI          // after ESC [, until a final byte
	plainString       // after ESC ] (or P, X, ^, _), until BEL or ESC \
	plainStringEscape // after an ESC inside a string
)

func (p *plainWriter) Write(b []byte) (int, error) {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		switch {
		case c == '\n':
			p.state = plainText
			out = append(out, c)
			continue
		case c == 0x1b && (p.state == plainString || p.state == plainStringEscape):
			p.state = plainStringEscape
			continue
		case c == 0x1b:
			p.state = plainEscape
			continue
		}
		switch p.state {
		case plainText:
			out = append(out, c)
		case plainEscape:
			switch {
			case c == '[':
				p.state = plainCSI
			case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
				p.state = plainString
			case c >= 0x20 && c <= 0x2f:
				p.state = plainIntermediate
			default:
				p.state = plainText
				out = keepUnlessFinal(out, c)
			}
		case plainIntermediate:
			if c < 0x20 || c > 0x2f {
				p.state = plainText
				out = keepUnlessFinal(out, c)
			}
		case plainCSI:
			switch {
			case c >= 0x40 && c <= 0x7e: // the final byte
				p.state = plainText
			case c < 0x20 || c > 0x3f: // not a parameter or intermediate byte: the sequence was broken
				p.state = plainText
				out = append(out, c)
			}
		case plainString:
			if c == 0x07 {
				p.state = plainText
			}
		case plainStringEscape:
			if c == '\\' {
				p.state = plainText
			} else {
				p.state = plainString
			}
		}
	}
	if _, err := p.w.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}

// keepUnlessFinal drops the byte that ends a short escape sequence, and keeps one that cannot.
func keepUnlessFinal(out []byte, c byte) []byte {
	if c >= 0x30 && c <= 0x7e {
		return out
	}
	return append(out, c)
}
