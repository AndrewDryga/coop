package tasks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TaskCounts tallies a task queue by state (todo/in_progress/blocked/done).
type TaskCounts struct {
	Todo    int `json:"todo"`
	Doing   int `json:"in_progress"`
	Done    int `json:"done"`
	Blocked int `json:"blocked"`
}

// Total sums every state's count.
func (c TaskCounts) Total() int { return c.Todo + c.Doing + c.Done + c.Blocked }

// QueuedTask is a task item plus the queue root (Root) it was read from — the multi-queue
// callers (the loop assignment, the status/telemetry views) need to know which host a task
// came from, not just its parsed fields.
type QueuedTask struct {
	Root string
	Item Item
}

// QueueState reads the queue union once, tallying it and selecting the authoritative next task.
// An interrupted task wins globally, even when an earlier subproject queue still has todo work;
// ties preserve queue order and ReadTaskTree's stable ID order. Shared by the loop's own
// assignment (AssignLoopTaskOnly) and cli's banner printing, so the two can never disagree about
// what's next.
func QueueState(hosts []string) (TaskCounts, QueuedTask, bool, error) {
	var total TaskCounts
	var firstTodo, firstDoing QueuedTask
	haveTodo, haveDoing := false, false
	for _, h := range hosts {
		items, err := ReadTaskTree(h)
		if err != nil {
			return TaskCounts{}, QueuedTask{}, false, err
		}
		for _, t := range items {
			switch t.State {
			case StateTodo:
				total.Todo++
				if !haveTodo {
					firstTodo, haveTodo = QueuedTask{Root: h, Item: t}, true
				}
			case StateInProgress:
				total.Doing++
				if !haveDoing {
					firstDoing, haveDoing = QueuedTask{Root: h, Item: t}, true
				}
			case StateBlocked:
				total.Blocked++
			case StateDone:
				total.Done++
			}
		}
	}
	if haveDoing {
		return total, firstDoing, true, nil
	}
	return total, firstTodo, haveTodo, nil
}

// QueueProgress sums task counts across the queue(s) and returns the authoritative next task's
// title, sharing QueueState with the loop assignment so cli's banner cannot disagree with the box.
func QueueProgress(hosts []string) (TaskCounts, string, error) {
	total, next, ok, err := QueueState(hosts)
	if err != nil {
		return TaskCounts{}, "", err
	}
	if !ok {
		return total, "", nil
	}
	return total, next.Item.Title, nil
}

// QueueStamps fingerprints the queues' state folders. A task added, moved between states or
// removed changes a state folder's time, so a poller can skip the whole-tree read (a few file calls
// per archived task) while the stamps hold. A change inside one task's folder does not show here.
func QueueStamps(hosts []string) string {
	var b strings.Builder
	for _, root := range hosts {
		for _, state := range []string{"", StateTodo, StateInProgress, StateBlocked, StateDone} {
			stampPath(&b, filepath.Join(root, state))
		}
	}
	return b.String()
}

func stampPath(b *strings.Builder, path string) {
	if info, err := os.Stat(path); err == nil {
		fmt.Fprintf(b, "%s %d %d\n", path, info.ModTime().UnixNano(), info.Size())
	} else {
		fmt.Fprintf(b, "%s -\n", path)
	}
}
