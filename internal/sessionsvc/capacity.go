package sessionsvc

import (
	"context"
	"fmt"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// Active and warm children share one bound. Create's Git concurrency is separate.
const sessionRuntimeSlots = 4

// RuntimeCapacity measures the same permits admission uses. The three advertised
// dimensions share this pool; they are not three independent allocations.
func (s *Service) RuntimeCapacity() workerproto.Capacity {
	s.historicalMu.Lock()
	unknown := len(s.historicalPending) != 0
	s.historicalMu.Unlock()
	s.mu.Lock()
	unknown = unknown || len(s.quarantined) != 0
	s.mu.Unlock()
	s.runtimeMu.Lock()
	free := sessionRuntimeSlots - len(s.runtimeSlots)
	s.runtimeMu.Unlock()
	if unknown || free < 0 {
		free = 0
	}
	state := "eligible"
	if free == 0 {
		state = "busy"
	}
	return workerproto.Capacity{
		SessionSlotsTotal: sessionRuntimeSlots, SessionSlotsFree: free,
		TurnSlotsTotal: sessionRuntimeSlots, TurnSlotsFree: free,
		WorkspaceSlotsTotal: sessionRuntimeSlots, WorkspaceSlotsFree: free, State: state,
	}
}

// A permit stays with a session across active/warm reuse. The bool records an
// uncertain teardown: only a later exact cleanup proof may release that permit.
func (s *Service) reserveRuntimeSlot(id string) (bool, <-chan struct{}) {
	s.historicalMu.Lock()
	unknown := len(s.historicalPending) != 0
	s.historicalMu.Unlock()
	s.mu.Lock()
	unknown = unknown || len(s.quarantined) != 0
	s.mu.Unlock()
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if s.runtimeChanged == nil {
		s.runtimeChanged = make(chan struct{})
	}
	if poisoned, held := s.runtimeSlots[id]; held {
		return !poisoned && !unknown, s.runtimeChanged
	}
	if unknown || len(s.runtimeSlots) >= sessionRuntimeSlots {
		return false, s.runtimeChanged
	}
	if s.runtimeSlots == nil {
		s.runtimeSlots = make(map[string]bool)
	}
	s.runtimeSlots[id] = false
	return true, s.runtimeChanged
}

func (s *Service) runtimeCapacityChangedLocked() {
	if s.runtimeChanged != nil {
		close(s.runtimeChanged)
	}
	s.runtimeChanged = make(chan struct{})
}

func (s *Service) runtimeCleaned(id string) {
	s.runtimeMu.Lock()
	delete(s.runtimeSlots, id)
	s.runtimeCapacityChangedLocked()
	s.runtimeMu.Unlock()
}

func (s *Service) runtimeCleanupFailed(id string) {
	s.runtimeMu.Lock()
	if s.runtimeSlots == nil {
		s.runtimeSlots = make(map[string]bool)
	}
	s.runtimeSlots[id] = true
	s.runtimeMu.Unlock()
}

func (s *Service) finishRuntimeSlot(bound session.Session, err error) {
	if sessionRunnerCleanupFailed(err) {
		s.runtimeCleanupFailed(bound.ID)
	}
	if runner, ok := s.runner.(*sessionTurnRunner); ok {
		runner.warmMu.Lock()
		held := runner.warm[bound.ID] != nil
		runner.warmMu.Unlock()
		if held {
			return
		}
	} else if inspector, ok := s.runner.(sessionRunnerWarmInspector); ok && inspector.WarmSessionReady(bound) {
		return
	}
	s.runtimeMu.Lock()
	if !s.runtimeSlots[bound.ID] {
		delete(s.runtimeSlots, bound.ID)
		s.runtimeCapacityChangedLocked()
	}
	s.runtimeMu.Unlock()
}

// Wait outside both the session lock and TurnTimeout. Reacquire/recheck before
// leasing so cancellation, close, review and warm expiry remain able to progress.
func (s *Service) lockRuntimeCapacity(ctx context.Context, id string, check func(session.Session) error) (func(), error) {
	for ctx.Err() == nil {
		unlock := s.lockSessionRuntime(id)
		if check != nil {
			bound, err := s.store.GetSession(ctx, id)
			if err == nil {
				err = check(bound)
			}
			if err != nil {
				unlock()
				return nil, err
			}
		}
		if s.historicalRuntimeNeedsCleanup(id) {
			bound, err := s.store.GetSession(ctx, id)
			if err == nil {
				err = s.validateSessionForkAuthority(ctx, bound)
			}
			if err == nil {
				if cleaner, ok := s.runner.(sessionRunnerRuntimeCleaner); ok {
					err = cleaner.CleanupSession(ctx, bound)
				}
			}
			if err != nil {
				unlock()
				return nil, fmt.Errorf("clean historical session runtime: %w", err)
			}
			s.markHistoricalRuntimeClean(id)
		}
		ok, changed := s.reserveRuntimeSlot(id)
		if ok {
			return unlock, nil
		}
		unlock()
		s.mu.Lock()
		var trigger <-chan struct{}
		if worker := s.workers[id]; worker != nil {
			trigger = worker.trigger
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		case <-trigger:
		}
	}
	return nil, ctx.Err()
}
