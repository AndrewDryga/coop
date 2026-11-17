package ui

import (
	"sort"
	"unicode"
)

// DisplayWidth is the number of terminal columns s takes, ANSI escapes not included: East Asian
// Wide and Fullwidth characters and emoji take two, combining marks and zero-width joiners none,
// everything else one. A live region clips its lines to the terminal by this, so a CJK task title
// or an emoji cannot make a line wrap and desync the region's erase arithmetic.
//
// It is a table, not a full wcwidth: where terminals disagree about a character it counts the
// wider reading, since clipping one column early is harmless and wrapping is not.
func DisplayWidth(s string) int {
	n := 0
	for _, r := range s {
		n += RuneWidth(r)
	}
	return n
}

// RuneWidth is the number of terminal columns r takes; see DisplayWidth.
func RuneWidth(r rune) int {
	switch {
	case r < 0x300: // ASCII and Latin: the common case, no table lookup
		return 1
	case unicode.In(r, unicode.Mn, unicode.Me) || r == 0x200B || r == 0x200C || r == 0x200D || r == 0x2060 || r == 0xFEFF:
		return 0
	case inWide(r):
		return 2
	}
	return 1
}

// wideRanges are the East Asian Wide and Fullwidth blocks and the emoji with a default emoji
// presentation, sorted and disjoint.
var wideRanges = [][2]rune{
	{0x1100, 0x115F}, // Hangul Jamo initials
	{0x231A, 0x231B}, {0x2329, 0x232A}, {0x23E9, 0x23EC}, {0x23F0, 0x23F0}, {0x23F3, 0x23F3},
	{0x25FD, 0x25FE}, {0x2614, 0x2615}, {0x2648, 0x2653}, {0x267F, 0x267F}, {0x2693, 0x2693},
	{0x26A1, 0x26A1}, {0x26AA, 0x26AB}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5}, {0x26CE, 0x26CE},
	{0x26D4, 0x26D4}, {0x26EA, 0x26EA}, {0x26F2, 0x26F3}, {0x26F5, 0x26F5}, {0x26FA, 0x26FA},
	{0x26FD, 0x26FD}, {0x2705, 0x2705}, {0x270A, 0x270B}, {0x2728, 0x2728}, {0x274C, 0x274C},
	{0x274E, 0x274E}, {0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797}, {0x27B0, 0x27B0},
	{0x27BF, 0x27BF}, {0x2B1B, 0x2B1C}, {0x2B50, 0x2B50}, {0x2B55, 0x2B55},
	{0x2E80, 0x303E},   // CJK radicals, Kangxi, ideographic description, CJK symbols and punctuation
	{0x3041, 0x33FF},   // Hiragana, Katakana, Bopomofo, Hangul compatibility, CJK compatibility
	{0x3400, 0x4DBF},   // CJK Extension A
	{0x4E00, 0x9FFF},   // CJK Unified Ideographs
	{0xA000, 0xA4CF},   // Yi
	{0xA960, 0xA97F},   // Hangul Jamo Extended-A
	{0xAC00, 0xD7A3},   // Hangul syllables
	{0xF900, 0xFAFF},   // CJK compatibility ideographs
	{0xFE10, 0xFE19},   // vertical forms
	{0xFE30, 0xFE6F},   // CJK compatibility forms, small form variants
	{0xFF00, 0xFF60},   // fullwidth forms
	{0xFFE0, 0xFFE6},   // fullwidth signs
	{0x16FE0, 0x16FE4}, // ideographic symbols
	{0x17000, 0x18CFF}, // Tangut, Khitan
	{0x1AFF0, 0x1B2FF}, // Kana supplement and extensions, Nushu
	{0x1F004, 0x1F004}, {0x1F0CF, 0x1F0CF}, {0x1F18E, 0x1F18E}, {0x1F191, 0x1F19A},
	{0x1F1E6, 0x1F1FF}, // regional indicators (flags)
	{0x1F200, 0x1F2FF}, // enclosed ideographic supplement
	{0x1F300, 0x1F64F}, // pictographs, emoticons
	{0x1F680, 0x1F6FF}, // transport and map symbols
	{0x1F7E0, 0x1F7EB}, // colored circles and squares
	{0x1F90C, 0x1F9FF}, // supplemental symbols and pictographs
	{0x1FA70, 0x1FAFF}, // symbols and pictographs extended-A
	{0x20000, 0x2FFFD}, // CJK Extensions B-F
	{0x30000, 0x3FFFD}, // CJK Extension G and beyond
}

func inWide(r rune) bool {
	i := sort.Search(len(wideRanges), func(i int) bool { return wideRanges[i][1] >= r })
	return i < len(wideRanges) && wideRanges[i][0] <= r
}
