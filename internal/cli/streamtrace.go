package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

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
func openStreamTrace(repo, run string, seq int, agent string) (raw, rendered io.Writer, close func(), err error) {
	noop := func() {}
	if !safeRunID(run) {
		return nil, nil, noop, fmt.Errorf("invalid run id")
	}
	runsRoot, err := openRunsRoot(repo, true)
	if err != nil {
		return nil, nil, noop, err
	}
	defer runsRoot.Close()
	dir := run + ".streams"
	info, err := runsRoot.Lstat(dir)
	if os.IsNotExist(err) {
		if err := runsRoot.MkdirAll(dir, 0o700); err != nil {
			return nil, nil, noop, err
		}
		info, err = runsRoot.Lstat(dir)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, noop, fmt.Errorf("stream trace target is not a regular directory")
	}
	traceRoot, err := runsRoot.OpenRoot(dir)
	if err != nil {
		return nil, nil, noop, err
	}
	defer traceRoot.Close()
	opened, err := traceRoot.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, nil, noop, fmt.Errorf("stream trace directory changed while opening")
	}
	base := fmt.Sprintf("%02d-%s", seq, agent)
	rawName := base + ".jsonl"
	outName := base + ".out"
	rawFile, err := traceRoot.OpenFile(rawName, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, nil, noop, err
	}
	outFile, err := traceRoot.OpenFile(outName, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		_ = rawFile.Close()
		_ = traceRoot.Remove(rawName)
		return nil, nil, noop, err
	}
	for name, file := range map[string]*os.File{rawName: rawFile, outName: outFile} {
		info, statErr := file.Stat()
		if statErr != nil {
			_ = rawFile.Close()
			_ = outFile.Close()
			_ = traceRoot.Remove(rawName)
			_ = traceRoot.Remove(outName)
			return nil, nil, noop, fmt.Errorf("inspect stream trace file %s: %w", name, statErr)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
			_ = rawFile.Close()
			_ = outFile.Close()
			_ = traceRoot.Remove(rawName)
			_ = traceRoot.Remove(outName)
			return nil, nil, noop, fmt.Errorf("stream trace file %s is not a single-link regular file", name)
		}
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
	raw, rendered, close, err := openStreamTrace(repo, a.runID, a.streamSeq, agent)
	if err != nil {
		a.streamOff = true
		ui.Warn("stream trace disabled: could not create %s: %v", dir, err)
		return nil, nil, noop
	}
	return raw, rendered, close
}
