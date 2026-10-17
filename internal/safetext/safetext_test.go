package safetext

import (
	"testing"
	"unicode"
)

// A box or a repository chooses the text Coop shows, so every character that can act on the
// terminal, break the line, turn the text around or draw as nothing is hidden wherever it stands,
// and the characters ordinary names are made of are not.
func TestHidden(t *testing.T) {
	for _, tc := range []struct {
		name        string
		previous, r rune
		want        bool
	}{
		{"ASCII letter", -1, 'w', false},
		{"combining accent", 'e', '\u0301', false},
		{"CJK and Hangul", '日', '한', false},
		{"emoji", -1, '🙂', false},
		{"skin tone", '👍', '\U0001F3FD', false},
		{"flag half", '\U0001F1FA', '\U0001F1E6', false},
		{"keycap", '\uFE0F', '\u20E3', false},
		{"no-break space", 'a', '\u00A0', false},
		{"replacement character", -1, '\uFFFD', false},
		{"escape", 'a', '\x1b', true},
		{"new line", 'a', '\n', true},
		{"tab", 'a', '\t', true},
		{"DEL", 'a', '\x7f', true},
		{"C1 control sequence introducer", 'a', '\u009B', true},
		{"C1 next line", 'a', '\u0085', true},
		{"right-to-left override", 'a', '\u202E', true},
		{"left-to-right isolate", 'a', '\u2066', true},
		{"right-to-left mark", 'a', '\u200F', true},
		{"Arabic letter mark", 'a', '\u061C', true},
		{"zero-width space", 'w', '\u200B', true},
		{"zero-width joiner inside a family emoji", '👨', '\u200D', true},
		{"byte-order mark", -1, '\uFEFF', true},
		{"soft hyphen", 'e', '\u00AD', true},
		{"tag character", 'b', '\U000E0041', true},
		{"line separator", 'a', '\u2028', true},
		{"paragraph separator", 'a', '\u2029', true},
		{"Hangul filler", 'w', '\u1160', true},
		{"Hangul compatibility filler", -1, '\u3164', true},
		{"combining grapheme joiner", 'w', '\u034F', true},
		{"unassigned tag-block code point", 'b', '\U000E0000', true},
		{"variation selector at the start", -1, '\uFE0E', true},
		{"variation selector after an ASCII letter", 'w', '\uFE0F', true},
		{"variation selector after an emoji", '❤', '\uFE0F', false},
		{"variation selector after a digit, for a keycap", '1', '\uFE0F', false},
		{"ideographic variation selector after a kanji", '葛', '\U000E0100', false},
	} {
		if got := Hidden(tc.previous, tc.r); got != tc.want {
			t.Errorf("%s: Hidden(%U, %U) = %v, want %v", tc.name, tc.previous, tc.r, got, tc.want)
		}
	}
	// Every character of the hidden classes, not only the ones above, is hidden wherever it stands.
	for _, table := range []*unicode.RangeTable{unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Other_Default_Ignorable_Code_Point} {
		eachRune(table, func(r rune) {
			for _, previous := range []rune{-1, 'a', '🙂'} {
				if !Hidden(previous, r) {
					t.Errorf("Hidden(%U, %U) = false, want true", previous, r)
				}
			}
		})
	}
	// Every variation selector is hidden where it draws as nothing, and kept after an emoji.
	eachRune(unicode.Variation_Selector, func(r rune) {
		if !Hidden(-1, r) || !Hidden('a', r) || Hidden('🙂', r) {
			t.Errorf("variation selector %U: hidden at the start %v, after a letter %v, after an emoji %v; want true, true, false",
				r, Hidden(-1, r), Hidden('a', r), Hidden('🙂', r))
		}
	})
}

// eachRune calls f with every character of table.
func eachRune(table *unicode.RangeTable, f func(rune)) {
	for _, r16 := range table.R16 {
		for r := rune(r16.Lo); r <= rune(r16.Hi); r += rune(r16.Stride) {
			f(r)
		}
	}
	for _, r32 := range table.R32 {
		for r := rune(r32.Lo); r <= rune(r32.Hi); r += rune(r32.Stride) {
			f(r)
		}
	}
}
