package workerconnector

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

type Transport interface {
	Poll(context.Context, workerproto.Poll) (workerproto.Response, error)
}

type ConnectorConfig struct {
	Executor  *Executor
	Hello     func(context.Context, time.Time) workerproto.WorkerHello
	Now       func() time.Time
	Transport Transport
}

type Connector struct {
	executor  *Executor
	hello     func(context.Context, time.Time) workerproto.WorkerHello
	now       func() time.Time
	sequence  uint64
	transport Transport
	workerID  string
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
		transport: config.Transport, workerID: hello.ID,
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
	c.sequence++
	pollRef := pollReference(c.workerID, c.sequence)
	poll := workerproto.Poll{
		Version: workerproto.Version, PollRef: pollRef, Worker: c.hello(ctx, c.now()),
		AcknowledgedCommandIDs: []string{}, CommandResults: []workerproto.CommandResult{},
		EventBatches: []workerproto.EventBatch{},
	}
	page, err := c.executor.journal.nextReceiptPage(poll)
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
	if err := poll.Validate(); err != nil {
		return fmt.Errorf("validate outbound worker poll: %w", err)
	}
	response, err := c.transport.Poll(ctx, poll)
	if err != nil {
		return err
	}
	if err := response.Validate(); err != nil {
		return fmt.Errorf("validate responder worker response: %w", err)
	}
	if response.PollRef != pollRef {
		return errors.New("worker response poll identity does not match")
	}
	// Fairness is not custody authority: a cursor write failure must not prevent
	// acknowledged receipts from freeing space or discard newly delivered commands.
	pollErr := errors.Join(page.issue, activityErr, c.executor.journal.advanceReceiptScan(page.cursor))
	if err := c.executor.journal.acknowledgeResults(response.AcknowledgedResultCommandIDs); err != nil {
		pollErr = errors.Join(pollErr, err)
	}
	// One command the worker cannot execute right now — an expired lease, a redelivery for
	// another worker, a conflicting receipt, an artifact fetch that failed for now — must not
	// starve the rest of the batch or the event acknowledgements behind it. Each failure is
	// reported; the command's receipt (if any) waits for the controller's redelivery.
	for _, command := range response.Commands {
		if _, err := c.executor.Execute(ctx, command); err != nil {
			if ctx.Err() != nil {
				return errors.Join(pollErr, err)
			}
			pollErr = errors.Join(pollErr, fmt.Errorf("command %s: %w", command.CommandID, err))
		}
	}
	// Event narration cannot delay commands or turn settlement. A failed
	// acknowledgement remains replayable from the previous durable cursor.
	return errors.Join(pollErr, c.executor.journal.acknowledgeEvents(response.EventAcknowledgements))
}

func (c *Connector) Run(ctx context.Context, interval time.Duration, onError func(error)) error {
	if interval <= 0 || interval > time.Minute || onError == nil {
		return errors.New("worker connector run configuration is invalid")
	}
	for {
		if err := c.PollOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			onError(err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}
