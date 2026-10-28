package workerconnector

import (
	"context"
	"fmt"
	"time"
)

// Missing operation evidence is not terminal proof. Preserve the origin while
// bounding failed reads; this process-local wait never changes durable custody.
type createOriginRetry struct {
	origin      createOrigin
	nextAttempt time.Time
	delay       time.Duration
}

func (e *Executor) resolveCreateOriginWhenDue(ctx context.Context, origin createOrigin) error {
	now := e.now()
	retry, exists := e.originRetries[origin.SessionRef]
	same := exists && retry.origin == origin
	if same && now.Before(retry.nextAttempt) {
		return nil
	}
	if err := e.resolveCreateOrigin(ctx, origin); err != nil {
		delay := 5 * time.Second
		if same {
			delay = min(retry.delay*2, 2*time.Minute)
		}
		if e.originRetries == nil {
			e.originRetries = make(map[string]createOriginRetry)
		}
		e.originRetries[origin.SessionRef] = createOriginRetry{origin: origin, nextAttempt: e.now().Add(delay), delay: delay}
		return fmt.Errorf("%w (retry in %s)", err, delay)
	}
	delete(e.originRetries, origin.SessionRef)
	return nil
}

func (e *Executor) pruneCreateOriginRetries(targets []activityTarget) {
	pending := make(map[string]bool, len(targets))
	for _, target := range targets {
		if target.origin != nil {
			pending[target.sessionRef] = true
		}
	}
	for ref := range e.originRetries {
		if !pending[ref] {
			delete(e.originRetries, ref)
		}
	}
}
