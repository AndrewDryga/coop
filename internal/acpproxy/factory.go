package acpproxy

import "context"

// A factory can wait for quota before starting any child. Give that wait the
// selection's lifetime, so a later provider change, reload, or disconnect can
// interrupt it even when there is no process for triggerRestart to stop yet.
func (p *proxy) startChild(ctx context.Context, factory Factory, epoch uint64) (*Child, error) {
	attempt, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	if p.restartEpoch != epoch || p.reloading.Load() || p.shuttingDown {
		p.mu.Unlock()
		cancel()
		return nil, errReplaySuperseded
	}
	p.factoryCancel = cancel
	p.mu.Unlock()
	child, err := factory(attempt)
	p.mu.Lock()
	p.factoryCancel = nil // the run loop admits only one factory at a time
	superseded := p.restartEpoch != epoch || p.reloading.Load() || p.shuttingDown
	p.mu.Unlock()
	if superseded || err != nil || attempt.Err() != nil {
		cancel()
		if child != nil {
			child.Stop()
		}
		if superseded {
			return nil, errReplaySuperseded
		}
		if err != nil {
			return nil, err
		}
		return nil, attempt.Err()
	}
	// Factory callers may bind child resources to this context. Keep it alive
	// through successful publication and cancel only when the child retires.
	stop := child.Stop
	child.Stop = func() {
		cancel()
		stop()
	}
	return child, nil
}
