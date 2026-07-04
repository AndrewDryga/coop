package main

import (
	"strings"
	"testing"
)

// TestManPage: the man page carries a NAME line (for apropos/mandb) and the manual verbatim in a
// no-fill block, and roff-escapes the body so arbitrary help text can't be misread as roff.
func TestManPage(t *testing.T) {
	manual := "coop — the tagline.\nUsage: coop x\n.dotstart line\nback\\slash line\n'quote line\n"
	got := manPage(manual)

	for _, want := range []string{
		`.TH COOP 1 "" "coop" "coop Manual"`, // deterministic header — no date/version to break -check
		".SH NAME\ncoop \\- the tagline.",    // NAME derived from the tagline, sans "coop — "
		".SH DESCRIPTION",
		".nf\n",   // no-fill block opens
		"\n.fi\n", // ...and closes
	} {
		if !strings.Contains(got, want) {
			t.Errorf("man page missing %q:\n%s", want, got)
		}
	}

	// roff escaping (the failure path): a line starting with . or ' is shielded with \&, and a
	// literal backslash becomes \e — so a stray control char in help.go can't corrupt the page.
	if !strings.Contains(got, `\&.dotstart line`) {
		t.Errorf("a .-leading line must be shielded with \\&:\n%s", got)
	}
	if !strings.Contains(got, `\&'quote line`) {
		t.Errorf("a '-leading line must be shielded with \\&:\n%s", got)
	}
	if !strings.Contains(got, `back\eslash line`) || strings.Contains(got, `back\slash`) {
		t.Errorf("a literal backslash must become \\e:\n%s", got)
	}
}
