package tasks

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
