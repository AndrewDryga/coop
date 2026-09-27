package secretscan

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"
)

type credentialLiteralPattern struct {
	re     *regexp.Regexp
	prefix []byte
}

var credentialLiteralPatterns = func() []credentialLiteralPattern {
	patterns := make([]string, 0, len(secretPatterns)+4)
	for _, pattern := range secretPatterns {
		patterns = append(patterns, pattern.re.String())
	}
	// Publication previously enforced these shapes at the controller. Keep that
	// barrier at the host that now exports the bytes, including older token forms.
	patterns = append(patterns, `(?i:\bxox[baprs]-[A-Za-z0-9-]{10,}\b)`,
		`\bxapp-[A-Za-z0-9-]{10,}\b`, `\bemk-[A-Za-z0-9_-]{10,}\b`, `\bgh[pousr]_[A-Za-z0-9]{20,}\b`)
	compiled := make([]credentialLiteralPattern, 0, len(patterns))
	for _, pattern := range patterns {
		// A leading word boundary prevents regexp from using its literal-prefix
		// optimization. Use the prefix only to skip impossible matches; the
		// original expression still checks every boundary and token shape.
		prefix, _ := regexp.MustCompile(strings.TrimPrefix(pattern, `\b`)).LiteralPrefix()
		compiled = append(compiled, credentialLiteralPattern{regexp.MustCompile(pattern), []byte(prefix)})
	}
	return compiled
}()

var credentialStreamPattern = func() *regexp.Regexp {
	patterns := make([]string, 0, len(credentialLiteralPatterns))
	for _, pattern := range credentialLiteralPatterns {
		patterns = append(patterns, "(?:"+pattern.re.String()+")")
	}
	return regexp.MustCompile(strings.Join(patterns, "|"))
}()

// ContainsCredential is the strict export barrier: precise literal shapes only,
// with no placeholder exemptions. Unlike ScanFile's fuzzy heuristics it works on
// arbitrarily large text or binary streams, without retaining lines or matches.
// It consumes the whole stream, including after a match, so buffered read errors
// cannot disappear before the caller verifies a content-addressed stream.
func ContainsCredential(input io.Reader) (bool, error) {
	reader := bufio.NewReaderSize(input, 64<<10)
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// Literal patterns never cross LF. Ordinary complete lines can use
			// regexp's byte-slice engine; only an unbounded line needs its slower
			// streaming engine. Keep the first fragment and every following byte.
			tail := &credentialLineReader{reader: reader}
			runes := &credentialRuneReader{Reader: bufio.NewReaderSize(io.MultiReader(bytes.NewReader(line), tail), 64<<10)}
			if matched := credentialStreamPattern.MatchReader(runes); matched || runes.err != nil {
				_, lineErr := io.Copy(io.Discard, runes.Reader)
				_, tailErr := io.Copy(io.Discard, reader)
				return matched, errors.Join(runes.err, lineErr, tailErr)
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		if credentialLineContains(line) {
			_, err := io.Copy(io.Discard, reader)
			return true, err
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
	}
}

func credentialLineContains(line []byte) bool {
	for _, pattern := range credentialLiteralPatterns {
		if bytes.Contains(line, pattern.prefix) && pattern.re.Match(line) {
			return true
		}
	}
	return false
}

// Presents one arbitrarily long line without buffering it or consuming the next.
type credentialLineReader struct {
	reader  *bufio.Reader
	pending []byte
	err     error
}

func (reader *credentialLineReader) Read(data []byte) (int, error) {
	if len(reader.pending) == 0 && reader.err == nil {
		reader.pending, reader.err = reader.reader.ReadSlice('\n')
		switch reader.err {
		case nil:
			reader.err = io.EOF
		case bufio.ErrBufferFull:
			reader.err = nil
		}
	}
	n := copy(data, reader.pending)
	reader.pending = reader.pending[n:]
	if len(reader.pending) != 0 {
		return n, nil
	}
	return n, reader.err
}

type credentialRuneReader struct {
	*bufio.Reader
	err error
}

func (reader *credentialRuneReader) ReadRune() (rune, int, error) {
	r, size, err := reader.Reader.ReadRune()
	// regexp treats every read error as EOF. A failed read is not a clean scan.
	if err != nil && !errors.Is(err, io.EOF) {
		reader.err = err
	}
	return r, size, err
}
