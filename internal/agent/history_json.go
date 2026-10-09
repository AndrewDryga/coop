package agent

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
)

// Native payloads may be large tool outputs or images. Retain only ownership
// strings, while validating and counting the rest of each JSONL row in place.
var errNativeHistoryJSON = errors.New("invalid native JSONL ownership record")

type nativeHistoryJSON struct {
	reader       *bufio.Reader
	offset       int64
	multiline    bool
	digest       hash.Hash
	digestBuffer []byte
}

func (p *nativeHistoryJSON) read() (byte, error) {
	b, err := p.reader.ReadByte()
	if err == nil {
		p.offset++
		if p.digest != nil {
			p.digestBuffer = append(p.digestBuffer, b)
			if len(p.digestBuffer) == cap(p.digestBuffer) {
				_, _ = p.digest.Write(p.digestBuffer)
				p.digestBuffer = p.digestBuffer[:0]
			}
		}
	}
	return b, err
}
func (p *nativeHistoryJSON) peek() (byte, error) {
	data, err := p.reader.Peek(1)
	if err != nil {
		return 0, err
	}
	return data[0], nil
}
func (p *nativeHistoryJSON) expect(want byte) error {
	b, err := p.read()
	if err != nil {
		return err
	}
	if b != want {
		return errNativeHistoryJSON
	}
	return nil
}
func (p *nativeHistoryJSON) space() error {
	for {
		b, err := p.peek()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if b != ' ' && b != '\t' && b != '\r' && !(p.multiline && b == '\n') {
			return nil
		}
		if _, err := p.read(); err != nil {
			return err
		}
	}
}
func (p *nativeHistoryJSON) literal(value string) error {
	for i := range len(value) {
		if err := p.expect(value[i]); err != nil {
			return err
		}
	}
	return nil
}
func nativeHistoryHex(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}
func (p *nativeHistoryJSON) string(capture bool) ([]byte, error) {
	if err := p.expect('"'); err != nil {
		return nil, err
	}
	var out []byte
	take := func(b byte) error {
		if !capture {
			return nil
		}
		if len(out) >= 64<<10 {
			return errors.New("native ownership string exceeds its bound")
		}
		out = append(out, b)
		return nil
	}
	_ = take('"')
	for {
		b, err := p.read()
		if err != nil {
			return nil, err
		}
		if err := take(b); err != nil {
			return nil, err
		}
		if b == '"' {
			return out, nil
		}
		if b < 0x20 {
			return nil, errNativeHistoryJSON
		}
		if b != '\\' {
			continue
		}
		b, err = p.read()
		if err != nil {
			return nil, err
		}
		if err := take(b); err != nil {
			return nil, err
		}
		switch b {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		case 'u':
			for range 4 {
				b, err = p.read()
				if err != nil {
					return nil, err
				}
				if !nativeHistoryHex(b) {
					return nil, errNativeHistoryJSON
				}
				if err := take(b); err != nil {
					return nil, err
				}
			}
		default:
			return nil, errNativeHistoryJSON
		}
	}
}
func nativeHistoryDigit(b byte) bool { return b >= '0' && b <= '9' }
func (p *nativeHistoryJSON) digits() error {
	n := 0
	for {
		b, err := p.peek()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if !nativeHistoryDigit(b) {
			break
		}
		if _, err := p.read(); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return errNativeHistoryJSON
	}
	return nil
}
func (p *nativeHistoryJSON) number() error {
	b, err := p.peek()
	if err != nil {
		return err
	}
	if b == '-' {
		_, _ = p.read()
		b, err = p.peek()
		if err != nil {
			return err
		}
	}
	if b == '0' {
		_, _ = p.read()
		b, err = p.peek()
		if err == nil && nativeHistoryDigit(b) {
			return errNativeHistoryJSON
		}
	} else if b >= '1' && b <= '9' {
		if err := p.digits(); err != nil {
			return err
		}
	} else {
		return errNativeHistoryJSON
	}
	b, err = p.peek()
	if err == nil && b == '.' {
		_, _ = p.read()
		if err := p.digits(); err != nil {
			return err
		}
	}
	b, err = p.peek()
	if err == nil && (b == 'e' || b == 'E') {
		_, _ = p.read()
		b, err = p.peek()
		if err != nil {
			return err
		}
		if b == '+' || b == '-' {
			_, _ = p.read()
		}
		if err := p.digits(); err != nil {
			return err
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
func (p *nativeHistoryJSON) value(depth int) error {
	if depth > 10000 {
		return errors.New("native JSON nesting exceeds its bound")
	}
	if err := p.space(); err != nil {
		return err
	}
	b, err := p.peek()
	if err != nil {
		return err
	}
	switch b {
	case '"':
		_, err := p.string(false)
		return err
	case 't':
		return p.literal("true")
	case 'f':
		return p.literal("false")
	case 'n':
		return p.literal("null")
	case '{', '[':
		_, _ = p.read()
		closing := byte(']')
		if b == '{' {
			closing = '}'
		}
		if err := p.space(); err != nil {
			return err
		}
		next, err := p.peek()
		if err != nil {
			return err
		}
		if next == closing {
			return p.expect(closing)
		}
		for {
			if b == '{' {
				if _, err := p.string(false); err != nil {
					return err
				}
				if err := p.space(); err != nil {
					return err
				}
				if err := p.expect(':'); err != nil {
					return err
				}
			}
			if err := p.value(depth + 1); err != nil {
				return err
			}
			if err := p.space(); err != nil {
				return err
			}
			next, err := p.read()
			if err != nil {
				return err
			}
			if next == closing {
				return nil
			}
			if next != ',' {
				return errNativeHistoryJSON
			}
			if err := p.space(); err != nil {
				return err
			}
		}
	default:
		return p.number()
	}
}

func nativeHistoryRows(reader io.Reader, keys []string, visit func(map[string]string, int64, int64) error) error {
	return nativeHistoryObjects(reader, keys, false, visit)
}

func nativeHistoryObjects(reader io.Reader, keys []string, multiline bool, visit func(map[string]string, int64, int64) error) error {
	return nativeHistoryParse(reader, keys, multiline, false, func(fields map[string]string, offset, size int64, _ string) error { return visit(fields, offset, size) })
}

// NativeHistoryIndexRows streams index payloads without imposing a transcript
// size limit. The stable key allows the host to preserve native deletions.
func NativeHistoryIndexRows(reader io.Reader, key string, visit func(string, int64, int64) error) error {
	var keys []string
	if key != "" {
		keys = []string{key}
	}
	return nativeHistoryKeyedRows(reader, keys, key, func(_ map[string]string, offset, size int64, id string) error { return visit(id, offset, size) })
}

func nativeHistoryKeyedRows(reader io.Reader, keys []string, indexKey string, visit func(map[string]string, int64, int64, string) error) error {
	return nativeHistoryParse(reader, keys, false, true, func(fields map[string]string, offset, size int64, key string) error {
		if indexKey != "" {
			if fields[indexKey] == "" {
				return errors.New("native index entry has no session identity")
			}
			sum := sha256.Sum256([]byte(fields[indexKey]))
			key = hex.EncodeToString(sum[:])
		}
		return visit(fields, offset, size, key)
	})
}

func nativeHistoryParse(reader io.Reader, keys []string, multiline, keyed bool, visit func(map[string]string, int64, int64, string) error) error {
	wanted := make(map[string]bool, len(keys))
	for _, key := range keys {
		wanted[key] = true
	}
	p := nativeHistoryJSON{reader: bufio.NewReaderSize(reader, 64<<10), multiline: multiline}
	if keyed {
		p.digest = sha256.New()
		p.digestBuffer = make([]byte, 0, 64<<10)
	}
	for {
		start := p.offset
		if keyed {
			p.digest.Reset()
			p.digestBuffer = p.digestBuffer[:0]
		}
		if err := p.space(); err != nil {
			return err
		}
		b, err := p.peek()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if b == '\n' {
			_, _ = p.read()
			continue
		}
		if err := p.expect('{'); err != nil {
			return err
		}
		if err := p.space(); err != nil {
			return err
		}
		fields := make(map[string]string, len(keys))
		b, err = p.peek()
		if err != nil {
			return err
		}
		if b == '}' {
			_, _ = p.read()
		} else {
			for {
				raw, err := p.string(true)
				if err != nil {
					return err
				}
				var key string
				if json.Unmarshal(raw, &key) != nil {
					return errNativeHistoryJSON
				}
				if err := p.space(); err != nil {
					return err
				}
				if err := p.expect(':'); err != nil {
					return err
				}
				if err := p.space(); err != nil {
					return err
				}
				if wanted[key] {
					if _, duplicate := fields[key]; duplicate {
						return errors.New("duplicate native ownership field")
					}
					b, err = p.peek()
					if err != nil {
						return err
					}
					var value string
					if b == 'n' {
						if err := p.literal("null"); err != nil {
							return err
						}
					} else {
						raw, err = p.string(true)
						if err != nil {
							return err
						}
						if json.Unmarshal(raw, &value) != nil {
							return errNativeHistoryJSON
						}
					}
					fields[key] = value
				} else if err := p.value(0); err != nil {
					return err
				}
				if err := p.space(); err != nil {
					return err
				}
				b, err = p.read()
				if err != nil {
					return err
				}
				if b == '}' {
					break
				}
				if b != ',' {
					return errNativeHistoryJSON
				}
				if err := p.space(); err != nil {
					return err
				}
			}
		}
		if err := p.space(); err != nil {
			return err
		}
		b, err = p.peek()
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if err == nil && (multiline || b != '\n') {
			return errNativeHistoryJSON
		}
		key := ""
		if keyed {
			_, _ = p.digest.Write(p.digestBuffer)
			key = hex.EncodeToString(p.digest.Sum(nil))
		}
		if err == nil {
			_, _ = p.read()
		}
		if err := visit(fields, start, p.offset-start, key); err != nil {
			return err
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}
