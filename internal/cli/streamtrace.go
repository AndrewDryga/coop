package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/ui"
)

// bestEffortWriter makes tracing observational: a full disk or failed file cannot break the
// MultiWriter ahead of the provider decoder or rendered output sinks.
type bestEffortWriter struct{ io.Writer }

func (w bestEffortWriter) Write(p []byte) (int, error) {
	_, _ = w.Writer.Write(p)
	return len(p), nil
}

// openStreamTrace creates one raw/rendered pair. The caller owns close; a partial open is cleaned
// up so a failed attempt never leaves a pair that looks complete.
func openStreamTrace(dir string, seq int, agent string) (raw, rendered io.Writer, close func(), err error) {
	noop := func() {}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, noop, err
	}
	base := fmt.Sprintf("%02d-%s", seq, agent)
	rawPath := filepath.Join(dir, base+".jsonl")
	outPath := filepath.Join(dir, base+".out")
	rawFile, err := os.OpenFile(rawPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, noop, err
	}
	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		_ = rawFile.Close()
		_ = os.Remove(rawPath)
		return nil, nil, noop, err
	}
	close = func() {
		_ = rawFile.Close()
		_ = outFile.Close()
	}
	return bestEffortWriter{rawFile}, bestEffortWriter{outFile}, close, nil
}

// iterationStreamTrace gates tracing to real streaming loop attempts and disables it after one
// open failure. The warning is emitted once because every later attempt returns at streamOff.
func (a *app) iterationStreamTrace(repo, agent string, streaming bool) (raw, rendered io.Writer, close func()) {
	noop := func() {}
	if !streaming || !a.cfg.StreamTrace || a.runID == "" || a.streamOff {
		return nil, nil, noop
	}
	a.streamSeq++
	dir := filepath.Join(repo, ".agent", "runs", a.runID+".streams")
	raw, rendered, close, err := openStreamTrace(dir, a.streamSeq, agent)
	if err != nil {
		a.streamOff = true
		ui.Warn("stream trace disabled: could not create %s: %v", dir, err)
		return nil, nil, noop
	}
	return raw, rendered, close
}
