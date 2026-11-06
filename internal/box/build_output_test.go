package box

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/runtime"
)

// What Docker 29 prints after a build whose Dockerfile has a bare `ARG COOP_BASE_IMAGE`, even with
// NO_COLOR set and its output redirected (QA, 2026-11-06).
const dockerWarning = " \x1b[33m1 warning found (use docker --debug to expand):\n\x1b[0m - InvalidDefaultArgInFrom: Default value for ARG ${COOP_BASE_IMAGE} results in empty or invalid base image name (line 2)\n"

const plainDockerWarning = " 1 warning found (use docker --debug to expand):\n - InvalidDefaultArgInFrom: Default value for ARG ${COOP_BASE_IMAGE} results in empty or invalid base image name (line 2)\n"

func TestPlainWriterRemovesEscapeSequencesAndKeepsTheText(t *testing.T) {
	for name, test := range map[string]struct{ in, want string }{
		"docker warning":          {dockerWarning, plainDockerWarning},
		"colors and cursor moves": {"\x1b[1;31mred\x1b[0m \x1b[2K\x1b[1Adone\n", "red done\n"},
		"hyperlink":               {"see \x1b]8;;https://example.com\x1b\\the docs\x1b]8;;\x07 now\n", "see the docs now\n"},
		"short escapes":           {"\x1b(Bplain\x1b7 text\x1b=\n", "plain text\n"},
		"plain text":              {"#5 DONE 0.6s\r\nλ ünïcode\n", "#5 DONE 0.6s\r\nλ ünïcode\n"},
		// A broken sequence must not take the text after it with it.
		"unterminated string": {"\x1b]0;title\nnext line\n", "\nnext line\n"},
		"broken csi":          {"\x1b[12\nkept\n", "\nkept\n"},
	} {
		t.Run(name, func(t *testing.T) {
			var whole bytes.Buffer
			if n, err := (&plainWriter{w: &whole}).Write([]byte(test.in)); err != nil || n != len(test.in) {
				t.Fatalf("write = %d, %v", n, err)
			}
			if whole.String() != test.want {
				t.Fatalf("plain output = %q, want %q", whole.String(), test.want)
			}
			// Docker writes in pieces, so a sequence can be split anywhere.
			var pieces bytes.Buffer
			w := &plainWriter{w: &pieces}
			for i := range len(test.in) {
				if _, err := w.Write([]byte{test.in[i]}); err != nil {
					t.Fatal(err)
				}
			}
			if pieces.String() != test.want {
				t.Fatalf("byte-at-a-time output = %q, want %q", pieces.String(), test.want)
			}
		})
	}
}

// A terminal that shows color gets Docker's output untouched; a file, a pipe or a log does not.
func TestBuildOutputIsPlainAwayFromAColorTerminal(t *testing.T) {
	if buildOutput(nil) != nil || buildOutput(io.Discard) != io.Discard {
		t.Fatal("no destination gained a writer")
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "build.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for name, w := range map[string]io.Writer{"a file": file, "a log": &bytes.Buffer{}} {
		if _, plain := buildOutput(w).(*plainWriter); !plain {
			t.Errorf("%s gets Docker's escape sequences", name)
		}
	}
}

// Both build paths hand Docker's output to buildOutput: the ordinary build and the filtered one.
func TestProjectBuildsShowDockerWarningsWithoutColor(t *testing.T) {
	script := filepath.Join(t.TempDir(), "docker")
	writeRepoFile(t, script, "#!/bin/sh\nprintf '"+strings.NewReplacer("\x1b", `\033`, "\n", `\n`, "%", "%%").Replace(dockerWarning)+"' >&2\n"+
		"while [ $# -gt 0 ]; do\n  if [ \"$1\" = --iidfile ]; then echo "+fixtureBuiltImage+" > \"$2\"; fi\n  shift\ndone\necho '#9 DONE 0.1s'\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	var ordinary bytes.Buffer
	if err := runBuild(runtime.Runtime{Name: script}, nil, io.Discard, &ordinary, "build", "."); err != nil {
		t.Fatal(err)
	}
	if ordinary.String() != plainDockerWarning {
		t.Errorf("ordinary build output = %q, want %q", ordinary.String(), plainDockerWarning)
	}
	// A daemon's build log takes both streams: it stays one writer, in the order Docker wrote it.
	var log bytes.Buffer
	if err := runBuild(runtime.Runtime{Name: script}, nil, &log, &log, "build", "."); err != nil {
		t.Fatal(err)
	}
	if want := plainDockerWarning + "#9 DONE 0.1s\n"; log.String() != want {
		t.Errorf("shared build log = %q, want %q", log.String(), want)
	}

	pinHome(t)
	repo := t.TempDir()
	writeRepoFile(t, filepath.Join(repo, ".agent", "Dockerfile"), "ARG COOP_BASE_IMAGE\nFROM ${COOP_BASE_IMAGE}\n")
	entries, err := buildContextSelection(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	dir, _, cleanup, err := stageContextEntries(t.Context(), repo, entries)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	projectBuildEndpoint(t)
	var filtered bytes.Buffer
	if _, err := buildStagedProjectOnBase(t.Context(), frozenProjectRuntime(t, script), dir, entries, ".agent/Dockerfile",
		filteredProjectTag(repo, fixtureLockedImage), "coop-clients:"+strings.Repeat("f", 32), false, nil, &filtered); err != nil {
		t.Fatal(err)
	}
	if filtered.String() != plainDockerWarning {
		t.Errorf("filtered build output = %q, want %q", filtered.String(), plainDockerWarning)
	}
}
