package workerconnector

import (
	"context"
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
	poll := workerproto.Poll{Version: workerproto.Version, PollRef: "poll:" + hello.ID + ":0", Worker: hello}
	if err := poll.Validate(); err != nil {
		return nil, fmt.Errorf("validate worker connector hello: %w", err)
	}
	return &Connector{
		executor: config.Executor, hello: config.Hello, now: config.Now,
		transport: config.Transport, workerID: hello.ID,
	}, nil
}

func (c *Connector) PollOnce(ctx context.Context) error {
	acknowledgements, results, err := c.executor.journal.pending()
	if err != nil {
		return err
	}
	c.sequence++
	pollRef := fmt.Sprintf("poll:%s:%d", c.workerID, c.sequence)
	poll := workerproto.Poll{
		Version: workerproto.Version, PollRef: pollRef, Worker: c.hello(ctx, c.now()),
		AcknowledgedCommandIDs: acknowledgements, CommandResults: results,
		EventBatches: []workerproto.EventBatch{},
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
	if err := c.executor.journal.acknowledgeResults(response.AcknowledgedResultCommandIDs); err != nil {
		return err
	}
	for _, command := range response.Commands {
		if _, err := c.executor.Execute(ctx, command); err != nil {
			return err
		}
	}
	return nil
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
