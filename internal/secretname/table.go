package secretname

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// The filesystem helper's eBPF normalizer walks the last name of a path through this table, one code
// point at a time, after it has decoded the name and folded and decomposed it the way the volume
// compares names (internal/fsfilter/assets/filter.c). Columns 0-127 are ASCII once A-Z are lowered,
// and otherColumn is any other code point except U+0130 and U+212A, the two that strings.ToLower turns
// into ASCII (i and k). So for a name read as is, the table ends hidden exactly when Hidden says so.
const (
	columns     = 129
	otherColumn = 128
	// maxStates bounds the table the helper builds into its read-only data. The built-in names need
	// 484 states, about 125 KB.
	maxStates = 2048
)

// column is the table column of one code point of a name, the mapping filter.c applies.
func column(r rune) int {
	switch {
	case 'A' <= r && r <= 'Z':
		return int(r - 'A' + 'a')
	case 0 <= r && r < otherColumn:
		return int(r)
	case r == 0x130:
		return 'i'
	case r == 0x212a:
		return 'k'
	}
	return otherColumn
}

type table struct {
	next   [][columns]uint16
	hidden []bool
}

// token is one element of a glob: a literal byte, ? (one code point) or * (any run of them).
type token struct {
	star, any bool
	literal   byte
}

// tokens reads a glob made of lowercase ASCII literals, ? and *, the only syntax the built-in names
// use. A class, an escape or a capital would need a reading of its own, so it is refused.
func tokens(glob string) ([]token, error) {
	var out []token
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; {
		case c == '*':
			out = append(out, token{star: true})
		case c == '?':
			out = append(out, token{any: true})
		case c == '[' || c == '\\' || c == '/' || c >= otherColumn || 'A' <= c && c <= 'Z':
			return nil, fmt.Errorf("secret name glob %q uses syntax the kernel table cannot read", glob)
		default:
			out = append(out, token{literal: c})
		}
	}
	return out, nil
}

// positions is a set of places in the globs, one bit each. A glob of n tokens owns n+1 consecutive
// bits, the last one meaning "matched", so moving past a token is a shift by one.
type positions []uint64

func (p positions) and(q positions) positions {
	out := make(positions, len(p))
	for i := range p {
		out[i] = p[i] & q[i]
	}
	return out
}

func (p positions) shifted() positions {
	out := make(positions, len(p))
	for i := range p {
		out[i] = p[i] << 1
		if i > 0 {
			out[i] |= p[i-1] >> 63
		}
	}
	return out
}

// absorb adds q to p and reports whether that added anything.
func (p positions) absorb(q positions) bool {
	grew := false
	for i := range p {
		grew = grew || q[i]&^p[i] != 0
		p[i] |= q[i]
	}
	return grew
}

func (p positions) any() bool { return slices.ContainsFunc(p, func(w uint64) bool { return w != 0 }) }

func (p positions) key() string {
	b := make([]byte, 0, len(p)*8)
	for _, w := range p {
		b = binary.LittleEndian.AppendUint64(b, w)
	}
	return string(b)
}

// buildTable compiles the four lists into the smallest automaton that answers Hidden: subset
// construction over every glob at once, then partition refinement, numbered from the start state.
func buildTable() (*table, error) {
	lists := [][]string{SecretGlobs, AllowGlobs, TemplateGlobs, HardGlobs}
	var globs [][]token
	var kinds []int
	bits := 0
	for kind, list := range lists {
		for _, glob := range list {
			t, err := tokens(glob)
			if err != nil {
				return nil, err
			}
			globs = append(globs, t)
			kinds = append(kinds, kind)
			bits += len(t) + 1
		}
	}
	words := (bits + 63) / 64
	set := func() positions { return make(positions, words) }
	stars, start := set(), set()
	var steps [columns]positions // positions whose token takes that column
	for col := range steps {
		steps[col] = set()
	}
	matched := [4]positions{set(), set(), set(), set()}
	at := 0
	for g, glob := range globs {
		start[at/64] |= 1 << (at % 64)
		for _, t := range glob {
			for col := range columns {
				if t.any || !t.star && int(t.literal) == col {
					steps[col][at/64] |= 1 << (at % 64)
				}
			}
			if t.star {
				stars[at/64] |= 1 << (at % 64)
			}
			at++
		}
		matched[kinds[g]][at/64] |= 1 << (at % 64)
		at++
	}
	// A star may match nothing, so a set also holds the place just past each star it holds.
	closed := func(p positions) positions {
		for p.absorb(p.and(stars).shifted()) {
		}
		return p
	}
	// A name ending in a set is hidden when a secret glob matched it and neither an allow glob nor a
	// template glob that no hard glob overrides did, as in Hidden.
	hiddenAt := func(p positions) bool {
		secret, allow, template, hard := p.and(matched[0]).any(), p.and(matched[1]).any(), p.and(matched[2]).any(), p.and(matched[3]).any()
		return secret && !(allow || template && !hard)
	}
	sets := []positions{closed(start)}
	ids := map[string]int32{sets[0].key(): 0}
	var next [][columns]int32
	for q := 0; q < len(sets); q++ {
		var row [columns]int32
		for col := range columns {
			moved := sets[q].and(stars)
			moved.absorb(sets[q].and(steps[col]).shifted())
			moved = closed(moved)
			id, ok := ids[moved.key()]
			if !ok {
				id = int32(len(sets))
				ids[moved.key()] = id
				sets = append(sets, moved)
			}
			row[col] = id
		}
		next = append(next, row)
	}
	hidden := make([]bool, len(sets))
	class := make([]int32, len(sets))
	for q, p := range sets {
		hidden[q] = hiddenAt(p)
		if hidden[q] {
			class[q] = 1
		}
	}
	// Merge the states no name can tell apart: split classes by where each column leads, until no
	// class splits.
	count := 0
	for _, verdict := range []bool{false, true} {
		if slices.Contains(hidden, verdict) {
			count++
		}
	}
	for {
		signatures := map[[columns + 1]int32]int32{}
		refined := make([]int32, len(sets))
		for q := range sets {
			var signature [columns + 1]int32
			signature[0] = class[q]
			for col := range columns {
				signature[col+1] = class[next[q][col]]
			}
			id, ok := signatures[signature]
			if !ok {
				id = int32(len(signatures))
				signatures[signature] = id
			}
			refined[q] = id
		}
		class = refined
		if len(signatures) == count {
			break
		}
		count = len(signatures)
	}
	// Number the merged states breadth first from the start, so the table comes out the same each time.
	number := map[int32]int{class[0]: 0}
	representative := []int32{0}
	t := &table{}
	for i := 0; i < len(representative); i++ {
		q := representative[i]
		var row [columns]uint16
		for col := range columns {
			n, ok := number[class[next[q][col]]]
			if !ok {
				n = len(representative)
				if n >= maxStates {
					return nil, fmt.Errorf("secret names need more than %d kernel table states", maxStates)
				}
				number[class[next[q][col]]] = n
				representative = append(representative, next[q][col])
			}
			row[col] = uint16(n)
		}
		t.next = append(t.next, row)
		t.hidden = append(t.hidden, hidden[q])
	}
	return t, nil
}

// CHeader is the table as the C header the helper image compiles into its eBPF normalizer.
func CHeader() ([]byte, error) {
	t, err := buildTable()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString("/* Generated by coop-fs name-table from internal/secretname. Do not edit. */\n")
	fmt.Fprintf(&out, "#define NAME_STATES %d\n#define NAME_COLUMNS %d\n#define NAME_OTHER %d\n", len(t.next), columns, otherColumn)
	out.WriteString("static const unsigned short name_next[NAME_STATES][NAME_COLUMNS] = {\n")
	for _, row := range t.next {
		cells := make([]string, len(row))
		for col, n := range row {
			cells[col] = strconv.Itoa(int(n))
		}
		out.WriteString("{" + strings.Join(cells, ",") + "},\n")
	}
	cells := make([]string, len(t.hidden))
	for q, hidden := range t.hidden {
		cells[q] = "0"
		if hidden {
			cells[q] = "1"
		}
	}
	out.WriteString("};\nstatic const unsigned char name_hidden[NAME_STATES] = {" + strings.Join(cells, ",") + "};\n")
	return out.Bytes(), nil
}
