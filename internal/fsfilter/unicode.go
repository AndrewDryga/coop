package fsfilter

import (
	"fmt"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// RuneMapping is shared with the BPF normalizer. Tables contain only nonidentity
// mappings and nonzero combining classes; absence means the rune is unchanged.
type RuneMapping struct {
	Decomposition              [4]uint32
	Fold                       [4]uint32
	NDecomposition, NFold, CCC uint32
}

func UnicodeTable() (map[uint32]RuneMapping, error) {
	fold := cases.Fold()
	table := make(map[uint32]RuneMapping)
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		s := string(r)
		decomposition := []rune(norm.NFD.String(s))
		folded := []rune(fold.String(s))
		// Canonical ordering changes positions, not length. Proving this per
		// scalar bounds every intermediate stage for any 255-byte component.
		stages := []string{string(decomposition), string(folded), fold.String(string(decomposition))}
		stages = append(stages, norm.NFD.String(stages[2]))
		for _, stage := range stages {
			if len([]rune(stage)) > 2*len(s) {
				return nil, fmt.Errorf("filesystem Unicode expansion exceeds BPF scratch bound: U+%04X", r)
			}
		}
		var value RuneMapping
		value.CCC = uint32(norm.NFD.PropertiesString(s).CCC())
		if len(decomposition) != 1 || decomposition[0] != r {
			if len(decomposition) > len(value.Decomposition) {
				return nil, fmt.Errorf("filesystem Unicode decomposition exceeds BPF bound: U+%04X", r)
			}
			value.NDecomposition = uint32(len(decomposition))
			for i, c := range decomposition {
				value.Decomposition[i] = uint32(c)
			}
		}
		if len(folded) != 1 || folded[0] != r {
			if len(folded) > len(value.Fold) {
				return nil, fmt.Errorf("filesystem Unicode folding exceeds BPF bound: U+%04X", r)
			}
			value.NFold = uint32(len(folded))
			for i, c := range folded {
				value.Fold[i] = uint32(c)
			}
		}
		if value != (RuneMapping{}) {
			table[uint32(r)] = value
		}
	}
	return table, nil
}
