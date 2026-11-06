package forkspace

import (
	"encoding/json"
	"io"
	"os"
	"slices"
	"syscall"

	"github.com/AndrewDryga/coop/internal/randid"
)

// forkLaunchWaitFolder has one note for each box that waits for a service launch lock now, in the
// project's fork records: the fork it works in, its process, and what it waits for in Coop's words,
// for `coop fork ls`. launchWaiterFolder tells `coop tasks watch` the same by execution record, but a
// filtered box waits while it prepares its services, before it has a record, so this note names its
// fork instead. A note whose process is gone says nothing, and a listing removes it.
const forkLaunchWaitFolder = "fork-launch-waits"

// launchWaitScan bounds how many notes a listing reads: each is one box waiting right now.
const launchWaitScan = 256

type launchWait struct {
	Fork  string   `json:"fork"`
	PID   int      `json:"pid"`
	Token string   `json:"token"`
	Boxes []string `json:"boxes,omitempty"`
}

// NoteLaunchWait says that a box of fork, run by this process, waits for boxes to let a service launch
// lock go, and returns the call that says the wait is over. It only tells `coop fork ls`, so a failure
// leaves the wait unshown and changes no lock.
func NoteLaunchWait(repo, fork string, boxes []string) func() {
	pid := os.Getpid()
	body, err := json.Marshal(launchWait{Fork: fork, PID: pid, Token: ProcStartToken(pid), Boxes: boxes})
	if !ValidExistingName(fork) || err != nil {
		return func() {}
	}
	root, err := OpenStateRoot(repo, forkLaunchWaitFolder, true)
	if err != nil {
		return func() {}
	}
	defer root.Close()
	name := randid.Hex(16) + ".json"
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return func() {}
	}
	_, writeErr := f.Write(body)
	if closeErr := f.Close(); writeErr != nil || closeErr != nil {
		_ = root.Remove(name)
		return func() {}
	}
	return func() {
		root, err := OpenStateRoot(repo, forkLaunchWaitFolder, false)
		if err != nil {
			return
		}
		defer root.Close()
		_ = root.Remove(name)
	}
}

// LaunchWaits reports whether a box of fork waits for a service launch lock now, and the boxes it waits
// for, from the notes of processes still running. A note whose process is provably gone is removed.
func LaunchWaits(repo, fork string) (boxes []string, waiting bool) {
	root, err := OpenStateRoot(repo, forkLaunchWaitFolder, false)
	if err != nil {
		return nil, false
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, false
	}
	names, _ := dir.Readdirnames(launchWaitScan)
	_ = dir.Close()
	for _, name := range names {
		f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, 64<<10))
		_ = f.Close()
		var note launchWait
		if err != nil || json.Unmarshal(data, &note) != nil {
			continue // one being written, or not Coop's
		}
		switch identity := ProcessIdentityOf(note.PID, note.Token); {
		case identity.ProvesGone():
			_ = root.Remove(name)
		case identity == ProcessIdentityMatch && note.Fork == fork:
			waiting = true
			for _, box := range note.Boxes {
				if !slices.Contains(boxes, box) {
					boxes = append(boxes, box)
				}
			}
		}
	}
	return boxes, waiting
}
