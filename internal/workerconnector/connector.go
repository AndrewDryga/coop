package workerconnector

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

type Transport interface {
	Poll(context.Context, workerproto.Poll) (workerproto.Response, error)
}

const commandReadSlots = 4

func readOnlyCommand(command workerproto.Command) bool {
	request, err := workerproto.DecodeAPIRequest(command.Payload)
	return err == nil && (request.Method == http.MethodGet || request.Method == http.MethodHead)
}

type ConnectorConfig struct {
	Executor  *Executor
	Hello     func(context.Context, time.Time) workerproto.WorkerHello
	Now       func() time.Time
	Transport Transport
}

type Connector struct {
	executor     *Executor
	hello        func(context.Context, time.Time) workerproto.WorkerHello
	now          func() time.Time
	sequence     uint64
	transport    Transport
	workerID     string
	acknowledged acknowledgedResults
}

func NewConnector(config ConnectorConfig) (*Connector, error) {
	if config.Executor == nil || config.Hello == nil || config.Now == nil || config.Transport == nil {
		return nil, errors.New("worker connector configuration is incomplete")
	}
	hello := config.Hello(context.Background(), config.Now())
	poll := workerproto.Poll{Version: workerproto.Version, PollRef: pollReference(hello.ID, 0), Worker: hello}
	if err := poll.Validate(); err != nil {
		return nil, fmt.Errorf("validate worker connector hello: %w", err)
	}
	return &Connector{
		executor: config.Executor, hello: config.Hello, now: config.Now,
		transport: config.Transport, workerID: hello.ID, acknowledged: make(acknowledgedResults),
	}, nil
}

func pollReference(workerID string, sequence uint64) string {
	// Reserve all 20 uint64 digits up front so sequence growth cannot exceed the
	// protocol's 256-byte bound. Keep hashed IDs outside the legacy poll: namespace.
	if len(workerID) > 256-len("poll::")-20 {
		return fmt.Sprintf("poll-sha256:%x:%d", sha256.Sum256([]byte(workerID)), sequence)
	}
	return fmt.Sprintf("poll:%s:%d", workerID, sequence)
}

func (c *Connector) PollOnce(ctx context.Context) error {
	return c.pollOnce(ctx, func(command workerproto.Command) error {
		_, err := c.executor.Execute(ctx, command)
		return err
	})
}

func (c *Connector) pollOnce(ctx context.Context, dispatch func(workerproto.Command) error) error {
	// A receipt may release a controller reservation. Sample it before capacity so
	// a completion during Hello cannot accompany a pre-admission free-slot count.
	entries, err := c.executor.journal.pending()
	if err != nil {
		return err
	}
	c.sequence++
	pollRef := pollReference(c.workerID, c.sequence)
	poll := workerproto.Poll{
		Version: workerproto.Version, PollRef: pollRef, Worker: c.hello(ctx, c.now()),
		AcknowledgedCommandIDs: []string{}, CommandResults: []workerproto.CommandResult{},
		EventBatches: []workerproto.EventBatch{},
	}
	page, err := c.executor.journal.nextReceiptPage(poll, c.acknowledged.unsent(entries))
	if err != nil {
		return err
	}
	poll = page.poll
	var activityErr error
	// Never put best-effort narration on the critical path of command
	// settlement. Its durable cursor makes deferring the read lossless.
	if !page.settlementPending {
		poll.EventBatches, activityErr = c.executor.collectActivity(ctx, eventBatchBudget(poll))
	}
	if !pollFits(poll) {
		// Command settlement owns the wire budget. Activity is replayable from
		// its durable cursor on the next poll after those results are acknowledged.
		poll.EventBatches = []workerproto.EventBatch{}
	}
	// Capacity and activity reads may have aged Hello's clock. UTC seconds keep
	// the fresh timestamp within the wire space already budgeted for receipts.
	poll.Worker.ClockAt = c.now().UTC().Truncate(time.Second)
	if err := poll.Validate(); err != nil {
		return fmt.Errorf("validate outbound worker poll: %w", err)
	}
	response, err := c.transport.Poll(ctx, poll)
	if err != nil {
		return err
	}
	if err := response.Validate(); err != nil {
		return fmt.Errorf("validate controller worker response: %w", err)
	}
	if response.PollRef != pollRef {
		return errors.New("worker response poll identity does not match")
	}
	// Fairness is not custody authority: a cursor write failure must not prevent
	// acknowledged receipts from freeing space or discard newly delivered commands.
	pollErr := errors.Join(page.issue, activityErr, c.executor.journal.advanceReceiptScan(page.cursor))
	pollErr = errors.Join(pollErr, c.acceptAcknowledgements(poll, entries, response.AcknowledgedResultCommandIDs))
	// One command the worker cannot execute right now — an expired lease, a redelivery for
	// another worker, a conflicting receipt, an artifact fetch that failed for now — must not
	// starve the rest of the batch or the event acknowledgements behind it. Each failure is
	// reported; the command's receipt (if any) waits for the controller's redelivery.
	for _, command := range response.Commands {
		if err := dispatch(command); err != nil {
			if ctx.Err() != nil {
				return errors.Join(pollErr, err)
			}
			pollErr = errors.Join(pollErr, fmt.Errorf("command %s: %w", command.CommandID, err))
		}
	}
	// Event narration cannot delay commands or turn settlement. A failed
	// acknowledgement remains replayable from the previous durable cursor.
	pollErr = errors.Join(pollErr, c.executor.journal.acknowledgeEvents(response.EventAcknowledgements))
	return errors.Join(pollErr, c.repairAcknowledgement(ctx, entries))
}

func (c *Connector) Run(ctx context.Context, interval time.Duration, onError func(error)) error {
	if interval <= 0 || interval > time.Minute || onError == nil {
		return errors.New("worker connector run configuration is invalid")
	}
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	type pendingCommand struct {
		command  workerproto.Command
		digest   string
		readOnly bool
		expiry   atomic.Int64
		renewed  chan struct{}
	}
	type completion struct {
		job *pendingCommand
		err error
	}
	// Run waits for all executors on shutdown without consuming more completions.
	completed := make(chan completion, 1+commandReadSlots)
	pending := make(map[string]*pendingCommand)
	var waits firstDownloadWaits
	var queue []*pendingCommand
	var mutationActive bool
	var readsActive int
	dispatch := func(command workerproto.Command) error {
		digest, err := commandDigest(command)
		if err != nil {
			return err
		}
		if job := pending[command.CommandID]; job != nil {
			if job.digest != digest {
				return ErrCommandConflict
			}
			job.expiry.Store(command.LeaseExpiresAt.UnixNano())
			select {
			case job.renewed <- struct{}{}:
			default:
			}
			return nil
		}
		if waits.held(command.CommandID, c.now()) {
			return nil
		}
		if len(pending) >= workerproto.MaxBatchItems {
			return errors.New("worker command queue is full; awaiting redelivery")
		}
		job := &pendingCommand{command: command, digest: digest, readOnly: readOnlyCommand(command), renewed: make(chan struct{}, 1)}
		job.expiry.Store(command.LeaseExpiresAt.UnixNano())
		pending[command.CommandID] = job
		queue = append(queue, job)
		return nil
	}
	// Mutations keep FIFO order; validated reads use their own bounded lane so a
	// slow command cannot hold fleet inspection behind it. Run owns both lanes.
	startNext := func() {
		for index := 0; index < len(queue) && ctx.Err() == nil; {
			job := queue[index]
			if (job.readOnly && readsActive == commandReadSlots) || (!job.readOnly && mutationActive) {
				index++
				continue
			}
			queue = append(queue[:index], queue[index+1:]...)
			if job.readOnly {
				readsActive++
			} else {
				mutationActive = true
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				expiresAt := func() time.Time { return time.Unix(0, job.expiry.Load()) }
				commandCtx, stopLease := watchCommandLease(ctx, c.now, expiresAt, job.renewed)
				defer stopLease()
				_, err := c.executor.execute(commandCtx, job.command, expiresAt)
				completed <- completion{job, err}
			}()
		}
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case done := <-completed:
			delete(pending, done.job.command.CommandID)
			// A command still waiting on its first download was reported when the wait began;
			// every later try that finds the download running says nothing new.
			stillWaiting := waits.waiting(done.job.command.CommandID) && errors.Is(done.err, errJobSourceDownloading)
			waits.settled(done.job.command.CommandID, done.err, c.now())
			if done.job.readOnly {
				readsActive--
			} else {
				mutationActive = false
			}
			if done.err != nil && ctx.Err() == nil && !stillWaiting {
				onError(fmt.Errorf("command %s: %w", done.job.command.CommandID, done.err))
			}
			startNext()
			// The controller waits on this result, so it goes out on the next
			// poll now rather than after the rest of the interval: every
			// short command waited up to a second to report (get_session and
			// get_turn averaged ~1.8 s end to end, 2026-09-28). Completions
			// that land together share that one poll, and polling returns to
			// its interval after it. A transfer failure has no result yet, so
			// redelivery keeps the ordinary retry interval.
			if done.err == nil {
				timer.Reset(0)
			}
		case <-timer.C:
			if err := c.pollOnce(ctx, dispatch); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				onError(err)
			}
			startNext()
			timer.Reset(interval)
		}
	}
}

func watchCommandLease(parent context.Context, now, expiresAt func() time.Time, renewed <-chan struct{}) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			remaining := expiresAt().Sub(now())
			if remaining <= 0 {
				cancel()
				return
			}
			timer := time.NewTimer(remaining)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-renewed:
			case <-timer.C:
			}
			timer.Stop()
		}
	}()
	return ctx, func() { cancel(); <-stopped }
}
