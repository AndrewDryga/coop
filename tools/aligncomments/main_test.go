package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProcess(t *testing.T) {
	for _, tc := range []struct {
		name, file, text string
		offenders        int
	}{
		// GitHub shows <id> inside a fence, so it takes four columns there.
		{"a fence counts a placeholder as text", "a.md",
			"```bash\ncoop context --task <id>  # one\ncoop context --changed    # two\n```\n", 0},
		{"a line aligned as if <id> were a tag is off by four", "b.md",
			"```bash\ncoop context --task <id> # one\ncoop context --changed    # two\n```\n", 2},
		// In HTML, <span> is markup and &lt;id&gt; shows as <id>: both measure by what the reader sees.
		{"html measures what the reader sees", "a.html", `<pre class="code">
<span class="k">coop</span> context --task &lt;id&gt;  <span class="t"># one</span>
coop context --changed    <span class="t"># two</span>
</pre>
`, 0},
		{"html drops a short tag and resolves an entity", "b.html",
			"<pre>\n<b>coop</b> run &lt;id&gt; <span class=\"t\"># one</span>\ncoop ls --all <span class=\"t\"># two</span>\n</pre>\n", 0},
		{"a comment after a closed fence is prose", "c.md", "```\ncoop x # one\n```\ncoop a # one\ncoop bbb # two\n", 0},
		{"a '#' inside a quoted string is not a comment", "d.md",
			"```bash\ntmux set -g status '#{s} # x'\ncoop a # one\n```\n", 0},
		{"a lone over-long line stands on its own", "e.md",
			"```\ncoop a  # one\ncoop bb # two\ncoop run claude -- -p \"a very long prompt that runs on and on\" # three\n```\n", 0},
		{"blank-separated stanzas of disparate width align separately", "f.md",
			"```\ncoop fork new feature-with-a-long-name --tasks 2026-10-15-a-long-task-id # one\n" +
				"coop fork new another-long-feature-name --tasks 2026-10-15-another-task  # two\n\n" +
				"coop ls # three\ncoop up # four\n```\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.file)
			if err := os.WriteFile(path, []byte(tc.text), 0o644); err != nil {
				t.Fatal(err)
			}
			if offenders, err := process(path, false); err != nil || len(offenders) != tc.offenders {
				t.Errorf("got %v (%v), want %d offenders", offenders, err, tc.offenders)
			}
		})
	}
}

// -write moves every comment of a jagged group to one space past its widest line, and leaves an
// aligned group's own gap alone.
func TestProcessWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.md")
	jagged := "```\ncoop a # one\ncoop bbb # two\n```\n\n```\ncoop a    # kept\ncoop bbb  # kept\n```\n"
	if err := os.WriteFile(path, []byte(jagged), 0o644); err != nil {
		t.Fatal(err)
	}
	if offenders, err := process(path, true); err != nil || len(offenders) != 2 {
		t.Fatalf("got %v (%v), want the 2 jagged lines", offenders, err)
	}
	got, err := os.ReadFile(path)
	want := "```\ncoop a   # one\ncoop bbb # two\n```\n\n```\ncoop a    # kept\ncoop bbb  # kept\n```\n"
	if err != nil || string(got) != want {
		t.Fatalf("rewrote to %q (%v), want %q", got, err, want)
	}
	if offenders, err := process(path, false); err != nil || len(offenders) != 0 {
		t.Errorf("the rewritten file still has offenders: %v (%v)", offenders, err)
	}
}
