// Package safetext names the characters that cannot be shown as they are on a line a person reads.
// Coop prints and records text a box or a repository chose, such as file names, task titles,
// commit authors and network provenance, and each place keeps these characters out in its own way:
// internal/ui writes them as their code, the task views drop them, and network policy refuses them.
// One definition keeps those places from disagreeing. It imports nothing, so internal/egress, which
// may not print, shares it with internal/ui.
package safetext

import (
	"unicode"
	"unicode/utf8"
)

// Hidden reports whether r, coming after previous (-1 at the start of the text), would act on the
// terminal, break the line, change the direction of the text around it or draw as nothing, so that
// a folder named "w\u200Beb" could pass for "web". Those are the control characters (Unicode Cc:
// C0, DEL and C1), the format characters (Cf: direction marks, zero-width spaces and joiners, the
// byte-order mark, the soft hyphen, tag characters), the line and paragraph separators, and the
// other code points Unicode draws as nothing, such as the Hangul fillers, the grapheme joiner and
// the unassigned rest of the tag block. Joiners and tag characters count inside emoji too, since
// they hide just as well in a name. Variation selectors, which emoji and CJK ideographs need, count
// only at the start or after an ASCII letter, where no emoji or ideograph puts one and they draw
// as nothing.
func Hidden(previous, r rune) bool {
	return unicode.IsControl(r) ||
		unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Other_Default_Ignorable_Code_Point) ||
		unicode.Is(unicode.Variation_Selector, r) && (previous < 0 || previous < utf8.RuneSelf && unicode.IsLetter(previous))
}
