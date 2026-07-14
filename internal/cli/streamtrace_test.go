package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestOpenStreamTrace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run.streams")
	raw, rendered, close, err := openStreamTrace(dir, 3, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(raw, "{\"type\":\"one\"}\nraw tail"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(rendered, "\x1b[2m· tool\x1b[0m\n"); err != nil {
		t.Fatal(err)
	}
	close()

	for name, want := range map[string]string{
		"03-codex.jsonl": "{\"type\":\"one\"}\nraw tail",
		"03-codex.out":   "\x1b[2m· tool\x1b[0m\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, got)
		}
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("trace dir mode = %o, want 700", got)
	}
}

type failedTraceWriter struct{}

func (failedTraceWriter) Write([]byte) (int, error) { return 0, errors.New("disk failed") }

func TestBestEffortWriterSwallowsFailure(t *testing.T) {
	p := []byte("provider bytes")
	n, err := (bestEffortWriter{failedTraceWriter{}}).Write(p)
	if err != nil || n != len(p) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(p))
	}
}

func TestIterationStreamTraceGatesAndWarnsOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		streaming bool
		cfg       bool
		runID     string
	}{
		{name: "plain", streaming: false, cfg: true, runID: "run"},
		{name: "disabled", streaming: true, cfg: false, runID: "run"},
		{name: "outside loop", streaming: true, cfg: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			a := &app{cfg: &config.Config{StreamTrace: tc.cfg}, runID: tc.runID}
			raw, rendered, close := a.iterationStreamTrace(repo, "claude", tc.streaming)
			close()
			if raw != nil || rendered != nil || a.streamSeq != 0 {
				t.Fatalf("trace = (%v, %v), seq=%d; want nil, nil, 0", raw, rendered, a.streamSeq)
			}
			if _, err := os.Stat(filepath.Join(repo, ".agent", "runs")); !os.IsNotExist(err) {
				t.Fatalf("gated trace created runs dir: %v", err)
			}
		})
	}

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".agent"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{StreamTrace: true}, runID: "run"}
	var warnings []string
	ui.SetLiveSink(func(s string) { warnings = append(warnings, s) })
	defer ui.SetLiveSink(nil)
	for i := 0; i < 2; i++ {
		raw, rendered, close := a.iterationStreamTrace(repo, "claude", true)
		close()
		if raw != nil || rendered != nil {
			t.Fatalf("failed open %d returned non-nil writers", i+1)
		}
	}
	if !a.streamOff || a.streamSeq != 1 {
		t.Errorf("failure state = off %v seq %d, want true/1", a.streamOff, a.streamSeq)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "stream trace disabled") {
		t.Errorf("warnings = %q, want one disable warning", warnings)
	}
}
