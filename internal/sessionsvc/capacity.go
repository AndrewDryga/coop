package sessionsvc

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// Active and warm children share one bound. Create's Git concurrency is separate.
const sessionRuntimeSlots = 4

func (s *Service) measureRuntimeMachine(ctx context.Context) {
	if _, production := s.runner.(*sessionTurnRunner); !production || !s.rt.IsDocker() || s.runtimeMachine() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	daemon, err := runtime.InspectDocker(ctx, s.rt)
	if err != nil {
		return
	}
	defer daemon.Close()
	info := daemon.Info()
	if info.NCPU <= 0 || info.NCPU > 10000 || info.MemTotal < 64<<20 {
		return
	}
	// Leave one quarter for the operating system, image builds and the worker.
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	s.machine = &workerproto.MachineCapacity{Daemon: fmt.Sprintf("%x", sha256.Sum256([]byte(info.ID))), CPUMillis: int64(info.NCPU) * 750, MemoryBytes: info.MemTotal - info.MemTotal/4}
	s.runtimeCapacityChangedLocked()
}

func (s *Service) runtimeMachine() *workerproto.MachineCapacity {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	return s.machine
}

func (s *Service) machineFreeLocked() *workerproto.MachineCapacity {
	if s.machine == nil {
		return nil
	}
	m := *s.machine
	m.FreeCPUMillis = m.CPUMillis
	m.FreeMemoryBytes = m.MemoryBytes
	for id := range s.runtimeSlots {
		r, ok := s.runtimeRequests[id]
		if !ok {
			m.FreeCPUMillis = 0
			m.FreeMemoryBytes = 0
			break
		}
		m.FreeCPUMillis = max(0, m.FreeCPUMillis-int64(r.CPUMillis))
		m.FreeMemoryBytes = max(0, m.FreeMemoryBytes-r.MemoryBytes)
	}
	return &m
}

func (s *Service) slotLimitLocked() int {
	if s.machine == nil {
		return sessionRuntimeSlots
	}
	return int(min(10000, max(1, s.machine.CPUMillis/250)))
}

// RuntimeCapacity measures the same permits admission uses. The three advertised
// dimensions share this pool; they are not three independent allocations.
func (s *Service) RuntimeCapacity() workerproto.Capacity {
	s.runtimeMu.Lock()
	s.historicalMu.Lock()
	unknown := len(s.historicalPending) != 0
	s.historicalMu.Unlock()
	total := s.slotLimitLocked()
	free := total - len(s.runtimeSlots)
	machine := s.machineFreeLocked()
	s.runtimeMu.Unlock()
	if unknown || free < 0 || machine != nil && (machine.FreeCPUMillis < 10 || machine.FreeMemoryBytes < 6<<20) {
		free = 0
	}
	state := "eligible"
	if free == 0 {
		state = "busy"
	}
	return workerproto.Capacity{
		SessionSlotsTotal: total, SessionSlotsFree: free, Machine: machine,
		TurnSlotsTotal: total, TurnSlotsFree: free,
		WorkspaceSlotsTotal: total, WorkspaceSlotsFree: free, State: state,
	}
}

// A permit stays with a session across active/warm reuse. The bool records an
// uncertain teardown: only a later exact cleanup proof may release that permit.
func (s *Service) reserveRuntimeResources(id string, request workerproto.JobResources) (bool, <-chan struct{}) {
	if s.testBeforeRuntimeReservation != nil {
		s.testBeforeRuntimeReservation()
	}
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	// Quarantine may discover pending custody after startup. Read it inside
	// the same admission lock that publishes that new hold.
	s.historicalMu.Lock()
	unknown := len(s.historicalPending) != 0
	s.historicalMu.Unlock()
	if s.runtimeChanged == nil {
		s.runtimeChanged = make(chan struct{})
	}
	if poisoned, held := s.runtimeSlots[id]; held {
		return !poisoned && !unknown, s.runtimeChanged
	}
	if unknown || len(s.runtimeSlots) >= s.slotLimitLocked() {
		return false, s.runtimeChanged
	}
	if m := s.machineFreeLocked(); m != nil {
		if request.CPUMillis <= 0 || request.MemoryBytes <= 0 || int64(request.CPUMillis) > m.FreeCPUMillis || request.MemoryBytes > m.FreeMemoryBytes {
			return false, s.runtimeChanged
		}
		if s.runtimeRequests == nil {
			s.runtimeRequests = make(map[string]workerproto.JobResources)
		}
		s.runtimeRequests[id] = request
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
	delete(s.runtimeRequests, id)
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

// runtimeCleanupPending is set by the runtime owner even when a completed answer
// is retained after teardown fails. Only proven cleanup releases this slot.
func (s *Service) runtimeCleanupPending(id string) bool {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	return s.runtimeSlots[id]
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
		delete(s.runtimeRequests, bound.ID)
		s.runtimeCapacityChangedLocked()
	}
	s.runtimeMu.Unlock()
}

// Wait outside both the session lock and TurnTimeout. Reacquire/recheck before
// leasing so cancellation, close, review and warm expiry remain able to progress.
// Only the session's own worker may also wake on its trigger: any other waiter would
// consume the token, and the worker, finding none, would retire with a turn queued.
func (s *Service) lockRuntimeCapacity(ctx context.Context, id string, worker bool, check func(session.Session) error) (func(), error) {
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
		var request workerproto.JobResources
		machine := s.runtimeMachine()
		if machine != nil {
			bound, err := s.store.GetSession(ctx, id)
			if err != nil {
				unlock()
				return nil, err
			}
			job, err := sessionJobSpec(bound)
			if err != nil {
				unlock()
				return nil, err
			}
			request = job.Resources
			if job.Runtime != nil && job.Runtime.Compose != "" {
				if job.Runtime.MaxServices > 0 {
					request.CPUMillis *= 1 + job.Runtime.MaxServices
					request.MemoryBytes *= int64(1 + job.Runtime.MaxServices)
				} else {
					// A previously accepted job has no portable sidecar count.
					request.CPUMillis = int(machine.CPUMillis)
					request.MemoryBytes = machine.MemoryBytes
				}
			}
			if int64(request.CPUMillis) > machine.CPUMillis || request.MemoryBytes > machine.MemoryBytes {
				unlock()
				return nil, fmt.Errorf("job exceeds worker machine budget")
			}
		}
		ok, changed := s.reserveRuntimeResources(id, request)
		if ok {
			return unlock, nil
		}
		unlock()
		if machine == nil && s.runtimeMachine() != nil {
			continue
		}
		s.mu.Lock()
		var trigger <-chan struct{}
		if current := s.workers[id]; worker && current != nil {
			trigger = current.trigger
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
