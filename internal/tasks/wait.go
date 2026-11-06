package tasks

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// A todo task can wait for another task: the `waits_on:` line of its task.md frontmatter names the
// task it must not start before. The folder stays in 00_todo/, so a wait is an ordering rather than
// another state, and the loop skips the task until the named one is in 99_done/. A wait nothing
// will ever meet (the named task is missing, in two queues, blocked for a person, the task itself,
// or waits for this one in turn) becomes that person's decision instead of a task skipped forever.
const waitsOnField = "waits_on"

type waitStatus int

const (
	waitMet     waitStatus = iota // no wait, or the task it names is done
	waitPending                   // the named task is still todo or in progress
	waitBroken                    // nothing will meet it: a person decides
)

// queueIndex is every task of a loop's queues by id; an id found in two queues has two entries.
type queueIndex map[string][]Item

func (q queueIndex) add(item Item) { q[item.ID] = append(q[item.ID], item) }

// indexOf is the index of one queue's tasks.
func indexOf(items []Item) queueIndex {
	index := queueIndex{}
	for _, item := range items {
		index.add(item)
	}
	return index
}

// wait says whether item may start and, for a broken wait, why not, in words for its decision.
func (q queueIndex) wait(item Item) (waitStatus, string) {
	on := item.WaitsOn
	if on == "" {
		return waitMet, ""
	}
	if on == item.ID {
		return waitBroken, "it names itself"
	}
	found := q[on]
	switch {
	case len(found) == 0:
		return waitBroken, on + " is not in the task queues this loop works"
	case len(found) > 1:
		return waitBroken, on + " is in more than one task queue"
	case found[0].State == StateDone:
		return waitMet, ""
	case found[0].State == StateBlocked:
		return waitBroken, on + " is blocked, waiting for a person's decision"
	}
	// Follow the todo tasks' own waits: a chain that comes back to this task never ends by itself.
	// A circle further on is reported by the tasks in it.
	seen := map[string]bool{item.ID: true, on: true}
	for next := found[0]; next.State == StateTodo && next.WaitsOn != ""; next = q[next.WaitsOn][0] {
		if next.WaitsOn == item.ID {
			return waitBroken, on + " waits for this task in turn"
		}
		if seen[next.WaitsOn] || len(q[next.WaitsOn]) != 1 {
			break
		}
		seen[next.WaitsOn] = true
	}
	return waitPending, ""
}

// waiting reports whether the loop must skip item for now.
func (q queueIndex) waiting(item Item) bool {
	status, _ := q.wait(item)
	return status != waitMet
}

// WaitsForAnotherTask reports whether task went back to 00_todo/ to wait (tasks_wait). Handing a task
// back is progress, not a stalled attempt: the loop takes other work next. A queue it cannot read is
// an error, not "no wait": the box can write the queue, so it must not be able to hide a wait.
func WaitsForAnotherTask(task QueuedTask) (bool, error) {
	current, ok, err := CurrentTask(task.Root, task.Item.ID)
	if err != nil {
		return false, fmt.Errorf("read task %s after its attempt: %w", task.Item.ID, err)
	}
	return ok && current.State == StateTodo && current.WaitsOn != "", nil
}

// RestoreRejectedWait returns a task whose wait the loop refused to in progress, with its work and
// the resume step its worker left, such as applying a patch saved in tmp/. The caller holds the
// iteration lease until the restoration and completion-window audit finish.
func RestoreRejectedWait(task QueuedTask, reason string) error {
	current, ok, err := CurrentTask(task.Root, task.Item.ID)
	if err != nil {
		return err
	}
	if !ok || current.State != StateTodo || current.WaitsOn == "" {
		return fmt.Errorf("restore task %s: its waiting folder is no longer present", task.Item.ID)
	}
	if err := MoveTaskDir(task.Root, current, StateInProgress); err != nil {
		return err
	}
	dir := filepath.Join(task.Root, StateInProgress, current.ID)
	guidance := "finish this task, or use tasks_block if it must not land before " + current.WaitsOn
	next := resumeStep(dir, guidance)
	if next == waitResumeFallback(current.WaitsOn) { // the wait's own step, not the worker's
		next = guidance
	}
	return errors.Join(
		setTaskWaitsOn(dir, ""),
		AppendTaskLogStrict(dir, "wait for "+current.WaitsOn+" rejected: "+reason),
		NormalizeTaskState(current.ID, dir, "in progress — wait rejected", next,
			"the wait was rejected; committed work is preserved", "request a wait before committing; never add a second Coop-Task binding"),
	)
}

func readQueueIndex(hosts []string) (queueIndex, []QueuedTask, error) {
	index := queueIndex{}
	var waits []QueuedTask
	for _, root := range hosts {
		items, err := ReadTaskTree(root)
		if err != nil {
			return nil, nil, err
		}
		for _, item := range items {
			index.add(item)
			if item.State == StateTodo && item.WaitsOn != "" {
				waits = append(waits, QueuedTask{Root: root, Item: item})
			}
		}
	}
	return index, waits, nil
}

// WaitForTask is tasks_wait for the in-progress task a loop iteration holds: it records the wait,
// says why in log.md and state.md, and returns the folder to 00_todo/. A refusal changes nothing. A
// wait for a task that is blocked for a person is recorded, and the loop's next SettleTaskWaits
// hands it to that person too.
func WaitForTask(hosts []string, task QueuedTask, on, reason string) error {
	if task.Item.State != StateInProgress {
		return fmt.Errorf("%s is %s; only the task you are working can wait", task.Item.ID, StateLabel(task.Item.State))
	}
	if on == task.Item.ID {
		return errors.New("a task cannot wait for itself")
	}
	index, _, err := readQueueIndex(hosts)
	if err != nil {
		return err
	}
	found := index[on]
	if len(found) == 0 {
		return fmt.Errorf("no task %s is in this loop's queues: check its id with tasks_list, or file that work first with tasks_propose", on)
	}
	waiting := task.Item
	waiting.WaitsOn = on
	switch status, why := index.wait(waiting); {
	case status == waitMet:
		return fmt.Errorf("%s is already done, so there is nothing to wait for: continue this task", on)
	case status == waitBroken && (len(found) != 1 || found[0].State != StateBlocked):
		return fmt.Errorf("%s cannot wait for %s: %s", task.Item.ID, on, why)
	}
	metadata, err := snapshotTaskMetadata(task.Item.Dir, "task.md", "log.md", "state.md")
	if err != nil {
		return err
	}
	if err := MoveTaskDir(task.Root, task.Item, StateTodo); err != nil {
		return err
	}
	dir := filepath.Join(task.Root, StateTodo, task.Item.ID)
	err = setTaskWaitsOn(dir, on)
	if err == nil {
		err = errors.Join(
			AppendTaskLogStrict(dir, "waits for "+on+" before it can go on: "+strings.Join(strings.Fields(reason), " ")),
			NormalizeTaskState(task.Item.ID, dir, "waiting — must not start before "+on+" is done",
				resumeStep(dir, waitResumeFallback(on)), "—", "—"),
		)
	}
	if err != nil {
		moved := task.Item
		moved.State, moved.Dir = StateTodo, dir
		return errors.Join(err, restoreTaskMetadata(dir, metadata), MoveTaskDir(task.Root, moved, StateInProgress))
	}
	return nil
}

// SettledWait is one wait SettleTaskWaits ended. Reason is empty when the named task finished and
// the task is ordinary work again; otherwise it says why the task was blocked for a person.
type SettledWait struct {
	Task    QueuedTask
	WaitsOn string
	Reason  string
}

// SettleTaskWaits runs on the host before the loop picks a task. A todo task whose named task is done
// drops its wait; one whose wait can never be met is blocked with a decision.md that says why. A task
// someone else holds or claims is left for a later pass. Blocking a task breaks the wait of a task
// that waits for it, so passes repeat until one blocks nothing.
func SettleTaskWaits(hosts []string, owner TaskLeaseOwner) ([]SettledWait, error) {
	var settled []SettledWait
	for blocked := true; blocked; {
		blocked = false
		index, waits, err := readQueueIndex(hosts)
		if err != nil {
			return settled, err
		}
		for _, task := range waits {
			status, why := index.wait(task.Item)
			if status == waitPending {
				continue
			}
			done, reason, err := settleWait(task, status, why, owner)
			if err != nil {
				return settled, err
			}
			if !done {
				continue
			}
			blocked = blocked || status != waitMet
			settled = append(settled, SettledWait{Task: task, WaitsOn: task.Item.WaitsOn, Reason: reason})
		}
	}
	return settled, nil
}

// settleWaitBeforeWrite lets a test act in the window between settlement's checks and its write.
var settleWaitBeforeWrite func(QueuedTask)

// settleWait settles one waiting task while holding its owner lock, the lock a claim takes to record
// itself before it moves the task. A task claimed first, or moved or changed since the queue was read,
// is left for a later pass; a claim that comes later waits until this settlement is written, so the
// task never moves under it. done reports whether the task was settled, and reason why it was blocked.
func settleWait(task QueuedTask, status waitStatus, why string, owner TaskLeaseOwner) (done bool, reason string, err error) {
	ownerLock, err := lockTaskOwner(task.Root, task.Item.ID)
	if err != nil {
		return false, "", err
	}
	defer ownerLock.Close()
	if _, owned, err := ownerLock.Read(); err != nil || owned {
		return false, "", err
	}
	current, exists, err := CurrentTask(task.Root, task.Item.ID)
	if err != nil {
		return false, "", err
	}
	if !exists || current.State != StateTodo || filepath.Clean(current.Dir) != filepath.Clean(task.Item.Dir) || current.WaitsOn != task.Item.WaitsOn {
		return false, "", nil
	}
	lease, _, err := TryTaskLease(task.Root, task.Item, owner)
	if errors.Is(err, errLeaseCandidateGone) || err == nil && lease == nil {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if settleWaitBeforeWrite != nil {
		settleWaitBeforeWrite(task)
	}
	if status == waitMet {
		why, err = "", releaseWait(task.Item)
	} else {
		err = parkBrokenWait(task, why)
	}
	if err = errors.Join(err, lease.Release()); err != nil {
		return false, "", err
	}
	return true, why, nil
}

// releaseWait makes a waiting task ordinary work again and keeps the worker's resume step.
func releaseWait(item Item) error {
	if err := setTaskWaitsOn(item.Dir, ""); err != nil {
		return err
	}
	return errors.Join(AppendTaskLogStrict(item.Dir, item.WaitsOn+" is done, so this task no longer waits for it"),
		NormalizeTaskState(item.ID, item.Dir, "ready — "+item.WaitsOn+", the task it waited for, is done",
			resumeStep(item.Dir, "continue this task"), "—", "—"))
}

// waitResumeFallback is the Next action a wait writes for a worker that left none.
func waitResumeFallback(on string) string { return "continue this task once " + on + " is done" }

// resumeStep is the Next action the worker left in state.md, or fallback when it left none.
func resumeStep(dir, fallback string) string {
	state, err := ReadTaskState(dir)
	if next := strings.TrimSpace(state.NextAction); err == nil && next != "" && next != "none" && next != "—" && !strings.HasPrefix(next, "<") {
		return next
	}
	return fallback
}

// parkBrokenWait blocks a waiting todo task for a person. Its waits_on line stays, so a task sent
// back without a change waits again; the decision says how to settle or drop the wait.
func parkBrokenWait(task QueuedTask, why string) error {
	id, on := task.Item.ID, task.Item.WaitsOn
	metadata, err := snapshotTaskMetadata(task.Item.Dir, "decision.md", "log.md", "state.md")
	if err != nil {
		return err
	}
	if err := MoveTaskDir(task.Root, task.Item, StateBlocked); err != nil {
		return err
	}
	dir := filepath.Join(task.Root, StateBlocked, id)
	_, err = ReplaceDecision(dir, id, task.Item.Title, Decision{
		Question: "This task waits for " + on + ", which will not finish on its own. What should it do?",
		Why: "The task must not start before " + on + " is done, but " + why +
			". The loop skips a waiting task, so without a change nobody would ever work it.",
		Options: []string{
			"Keep the wait: settle " + on + " first (answer its decision, or add it to the queue), then send this task back.",
			"Drop the wait: delete the waits_on line in this task's task.md, then send it back to be worked now.",
		},
		Recommendation: "Keep the wait when the order still matters, so this task does not land before the work it depends on.",
	})
	if err == nil {
		err = errors.Join(AppendTaskLogStrict(dir, "blocked: it waits for "+on+", but "+why),
			NormalizeTaskState(id, dir, "blocked — its wait for "+on+" cannot be met", "resolve decision.md, then send this task back", "—", "—"))
	}
	if err != nil {
		moved := task.Item
		moved.State, moved.Dir = StateBlocked, dir
		return errors.Join(err, restoreTaskMetadata(dir, metadata), MoveTaskDir(task.Root, moved, StateTodo))
	}
	return nil
}

// setTaskWaitsOn writes (on != "") or removes (on == "") the waits_on line of dir's task.md, adding
// a frontmatter block when the file has none. Every other line is kept as it was.
func setTaskWaitsOn(dir, on string) error {
	root, err := OpenTaskMetadataRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	data, err := ReadTaskMetadataFile(root, "task.md")
	if err != nil {
		return err
	}
	updated := withFrontmatterField(string(data), waitsOnField, on)
	if updated == string(data) {
		return nil
	}
	return AtomicWriteTaskFile(root, "task.md", []byte(updated))
}

func withFrontmatterField(content, key, value string) string {
	all := strings.Split(content, "\n")
	start, end, ok := frontmatterFences(all)
	if !ok {
		if value == "" {
			return content
		}
		return "---\n" + key + ": " + value + "\n---\n\n" + content
	}
	out := append([]string{}, all[:start+1]...)
	written := false
	for _, line := range all[start+1 : end] {
		if k, _, found := strings.Cut(strings.TrimSpace(line), ":"); found && strings.TrimSpace(k) == key {
			if value != "" && !written {
				out, written = append(out, key+": "+value), true
			}
			continue
		}
		out = append(out, line)
	}
	if value != "" && !written {
		out = append(out, key+": "+value)
	}
	return strings.Join(append(out, all[end:]...), "\n")
}
