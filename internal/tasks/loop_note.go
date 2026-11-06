package tasks

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/processidentity"
)

const loopNoteVersion = 1

// loopNoteRecord is what a loop run says about itself on a queue it works, for coop tasks watch:
// that a person stopped it, or that it waits out a usage limit and until when. It sits in the
// host-only lease registry beside the queue's leases and, like a holder record, is display
// evidence only: no authority check reads it.
type loopNoteRecord struct {
	Version int    `json:"version"`
	RunID   string `json:"run_id"`
	PID     int    `json:"pid"`
	// Start is the run's process start token, so a later process that reused the pid is not the run.
	Start      string    `json:"start"`
	StoppedAt  time.Time `json:"stopped_at,omitzero"`
	LimitUntil time.Time `json:"limit_until,omitzero"`
}

// LoopNote is one loop run's note on every queue it works.
type LoopNote struct {
	mu     sync.Mutex
	roots  []string
	record loopNoteRecord
}

// NoteLoopRun starts runID's note on each queue in roots, replacing what the last run there left,
// so a stop a person made never explains what a later run did, a crash included. A note that
// cannot be written only leaves the board saying less.
func NoteLoopRun(roots []string, runID string) *LoopNote {
	n := &LoopNote{roots: roots, record: loopNoteRecord{
		Version: loopNoteVersion, RunID: runID, PID: os.Getpid(), Start: processidentity.StartToken(os.Getpid()),
	}}
	n.update(func(*loopNoteRecord) {})
	return n
}

// Stopped notes that a person stopped the run (Ctrl-C, or coop fork stop's signal). It is noted
// when the stop is asked for: coop fork stop kills a worker that does not exit within seconds.
func (n *LoopNote) Stopped() {
	n.update(func(r *loopNoteRecord) {
		if r.StoppedAt.IsZero() {
			r.StoppedAt = time.Now().UTC()
		}
	})
}

// WaitingForLimit notes that the run waits out a usage limit until until, and returns the call that
// notes the wait is over.
func (n *LoopNote) WaitingForLimit(until time.Time) func() {
	n.update(func(r *loopNoteRecord) { r.LimitUntil = until.UTC() })
	return func() { n.update(func(r *loopNoteRecord) { r.LimitUntil = time.Time{} }) }
}

func (n *LoopNote) update(change func(*loopNoteRecord)) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	change(&n.record)
	data, err := json.Marshal(n.record)
	if err != nil {
		return
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return
	}
	defer registry.Close()
	for _, root := range n.roots {
		if name, err := loopNoteName(root); err == nil {
			_ = AtomicWriteTaskFile(registry, name, append(data, '\n'))
		}
	}
}

// loopNoteName keys a queue's note like its leases; no task has the empty id.
func loopNoteName(root string) (string, error) {
	key, err := LeaseAuthorityKey(root, "")
	return key + ".loop.json", err
}

// ProjectTaskLoop is what the loop run that last held a task says about it once nothing holds it.
type ProjectTaskLoop struct {
	StoppedAt       time.Time `json:"stopped_at,omitzero"`        // a person stopped that run
	UsageLimitUntil time.Time `json:"usage_limit_until,omitzero"` // the run waits out a usage limit until then
}

// taskLoopNote is what the run that last held task id of the queue at root says about it, or nil:
// the run says nothing, a later run has started there, or the run waiting out a limit is gone. The
// caller has seen that nothing holds the task's lease now.
func taskLoopNote(root, id string, now time.Time) *ProjectTaskLoop {
	name, err := loopNoteName(root)
	if err != nil {
		return nil
	}
	var note loopNoteRecord
	if ok, err := readLeaseRecord(name, &note); err != nil || !ok || note.Version != loopNoteVersion || note.RunID == "" {
		return nil
	}
	if holder, ok := lastLeaseHolder(root, id); !ok || holder.RunID != note.RunID {
		return nil
	}
	switch {
	case !note.StoppedAt.IsZero():
		return &ProjectTaskLoop{StoppedAt: note.StoppedAt}
	case note.LimitUntil.After(now) && processidentity.Inspect(note.PID, note.Start) == processidentity.Match:
		return &ProjectTaskLoop{UsageLimitUntil: note.LimitUntil}
	}
	return nil
}
