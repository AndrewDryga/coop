package workerconnector

import (
	"errors"
	"time"
)

// A command whose job source is a repository still downloading its whole history for the first
// time (errJobSourceDownloading) has no result yet, so the controller delivers it again on every
// poll, and staging it again only finds the download still running. blitz-core's 45-minute first
// download was tried, and logged, at every poll (2026-10-03). A held command is tried again
// after five seconds, then twice as long each time, up to two minutes; every other command keeps
// the ordinary pace.
const (
	firstDownloadRetry    = 5 * time.Second
	firstDownloadRetryMax = 2 * time.Minute
)

type firstDownloadWaits struct {
	waits map[string]firstDownloadWait
}

type firstDownloadWait struct {
	notBefore time.Time
	delay     time.Duration
}

// held reports whether a delivered command is still waiting out its delay.
func (w *firstDownloadWaits) held(commandID string, now time.Time) bool {
	wait, ok := w.waits[commandID]
	return ok && now.Before(wait.notBefore)
}

// settled records what a command's try came to. A source still downloading holds the command
// for twice its last delay; any other outcome ends its wait. A wait whose command was not
// delivered again well after its delay ended belongs to a command the controller stopped
// delivering, and is dropped.
func (w *firstDownloadWaits) settled(commandID string, err error, now time.Time) {
	for id, wait := range w.waits {
		if now.Sub(wait.notBefore) > 2*firstDownloadRetryMax {
			delete(w.waits, id)
		}
	}
	if !errors.Is(err, errJobSourceDownloading) {
		delete(w.waits, commandID)
		return
	}
	delay := firstDownloadRetry
	if wait, ok := w.waits[commandID]; ok {
		delay = min(2*wait.delay, firstDownloadRetryMax)
	}
	if w.waits == nil {
		w.waits = make(map[string]firstDownloadWait)
	}
	w.waits[commandID] = firstDownloadWait{notBefore: now.Add(delay), delay: delay}
}
