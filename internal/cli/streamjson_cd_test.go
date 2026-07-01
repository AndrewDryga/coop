package cli

import "testing"

// stripLeadingCD peels the `cd <dir>` an agent prefixes to reach a monorepo subdir, so a streamed
// Bash line shows the command that did the work — otherwise a whole run reads as identical `cd …`.
func TestStripLeadingCD(t *testing.T) {
	cases := []struct{ in, want string }{
		{"cd /repo/portal && mix test", "mix test"},
		{"cd /repo/portal\nmix compile", "mix compile"},
		{"cd a && cd b && grep x", "grep x"},   // peels every leading cd
		{"cd /x && cmd1\ncmd2", "cmd1\ncmd2"},  // && comes before the newline
		{"cd /repo/portal", "cd /repo/portal"}, // bare cd IS the command — keep it
		{"grep foo", "grep foo"},               // no cd prefix
		{"  cd /x && ls", "ls"},                // leading whitespace
	}
	for _, c := range cases {
		if got := stripLeadingCD(c.in); got != c.want {
			t.Errorf("stripLeadingCD(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The user-visible result: the Bash tool line shows the real command, not the chdir.
func TestBashDisplayStripsCD(t *testing.T) {
	if _, label, _ := toolDisplay("", "Bash", []byte(`{"command":"cd /Users/x/portal && mix test"}`)); label != "mix test" {
		t.Errorf("Bash display = %q, want %q", label, "mix test")
	}
}
