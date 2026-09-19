package networkgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
)

const (
	MaxKernelCounterBytes = 64 << 10
	// One counter per address grant, bounded by the policy's own grant limit.
	MaxKernelGrantCounters = egress.MaxGrants
	KernelSampleTimeout    = 500 * time.Millisecond
)

// These are disjoint packet counters, not connection or application-byte
// counters. ProtectedAgent is added to DeniedAgent for total agent denials.
type KernelCounters struct {
	DeniedAgent    networkview.Count `json:"denied_agent"`
	ProtectedAgent networkview.Count `json:"protected_agent"`
	DeniedService  networkview.Count `json:"denied_service"`
	DeniedIngress  networkview.Count `json:"denied_ingress"`
	// Grants counts the packets each address grant actually passed, keyed by
	// its kernel counter name. Bytes are real here — unlike a denial, an
	// allowed raw flow is a measured volume, not a packet tally alone.
	Grants map[string]GrantCount `json:"grants,omitempty"`
}

type GrantCount struct {
	Packets networkview.Count `json:"packets"`
	Bytes   networkview.Count `json:"bytes"`
}

type KernelSample struct {
	Sequence      networkview.Count `json:"sequence"`
	At            time.Time         `json:"at"`
	BootAt        BootInstant       `json:"boot_at"`
	StartedBoot   BootInstant       `json:"started_boot"`
	Counters      *KernelCounters   `json:"counters"`
	Reason        string            `json:"reason,omitempty"`
	EnforcerReady bool              `json:"enforcer_ready"`
}

// Sampling has its own socket, subprocess deadline and cache. Observation
// callers cannot start a subprocess, renew liveness or compete for lease slots.
type kernelEvents struct {
	mu     sync.Mutex
	sample KernelSample
	read   func(context.Context) (KernelCounters, error)
	// wake asks the sampler for a sample now rather than at its next tick: a terminal barrier needs one
	// started after its cutoff, and every run's teardown waits for it. Made on first use.
	wake chan struct{}
}

func (k *kernelEvents) wakeup() chan struct{} {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.wake == nil {
		k.wake = make(chan struct{}, 1)
	}
	return k.wake
}

func (k *kernelEvents) snapshot() KernelSample {
	k.mu.Lock()
	defer k.mu.Unlock()
	s := k.sample
	if s.Counters != nil {
		value := *s.Counters
		value.Grants = maps.Clone(value.Grants)
		s.Counters = &value
	}
	if s.Sequence == 0 {
		s.Reason = "kernel_counters_unavailable"
	}
	return s
}

func (k *kernelEvents) run(ctx context.Context, clock *BootClock) {
	if k.read == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	wake := k.wakeup()
	for {
		started := clock.instant()
		bounded, cancel := context.WithTimeout(ctx, KernelSampleTimeout)
		counters, err := k.read(bounded)
		cancel()
		now := clock.instant()
		k.mu.Lock()
		sequence := k.sample.Sequence
		exact := networkview.Add(&sequence, 1)
		k.sample = KernelSample{Sequence: sequence, At: time.Now().UTC(), StartedBoot: started, BootAt: now, Reason: "kernel_counters_unavailable"}
		if exact && err == nil && now.Valid() && started.Valid() && !now.Before(started) && now.Sub(started) <= KernelSampleTimeout {
			k.sample.Counters, k.sample.Reason = &counters, ""
		}
		k.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		}
	}
}

func (k *kernelEvents) after(ctx context.Context, cutoff BootInstant) KernelSample {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	// Without this the barrier waits out up to a whole sampling tick — the better part of a second on
	// every filtered run's stop. A sample started now starts after the cutoff, which has passed.
	select {
	case k.wakeup() <- struct{}{}:
	default: // a wake is already pending
	}
	for {
		sample := k.snapshot()
		if sample.StartedBoot.Valid() && !sample.StartedBoot.Before(cutoff) {
			return sample
		}
		select {
		case <-ctx.Done():
			return KernelSample{Reason: "kernel_terminal_sample_unavailable"}
		case <-ticker.C:
		}
	}
}

func readKernelCounters(ctx context.Context) (KernelCounters, error) {
	// nft 1.0.2 does not accept a table argument for "list counters inet".
	// Filter the exact table and required names after the bounded fixed query.
	command := exec.CommandContext(ctx, "/usr/sbin/nft", "-j", "list", "counters", "inet")
	output := &boundedOutput{limit: MaxKernelCounterBytes}
	command.Stdout, command.Stderr = output, io.Discard
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	command.WaitDelay = 250 * time.Millisecond
	if command.Run() != nil || output.exceeded {
		return KernelCounters{}, Failure("kernel_counters_unavailable")
	}
	return parseKernelCounters(output.Bytes())
}

type boundedOutput struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

// Do not embed bytes.Buffer: its promoted ReadFrom would let io.Copy (used by
// os/exec's stdout pump) bypass Write's size limit entirely.
func (b *boundedOutput) Bytes() []byte { return b.buffer.Bytes() }
func (b *boundedOutput) Len() int      { return b.buffer.Len() }

func (b *boundedOutput) Write(data []byte) (int, error) {
	if len(data) > b.limit-b.Len() {
		b.exceeded = true
		return 0, Failure("observation_limit")
	}
	return b.buffer.Write(data)
}

func parseKernelCounters(data []byte) (KernelCounters, error) {
	invalid := Failure("kernel_counters_invalid")
	var envelope struct {
		Objects []struct {
			Counter *struct {
				Family  string          `json:"family"`
				Table   string          `json:"table"`
				Name    string          `json:"name"`
				Packets json.RawMessage `json:"packets"`
				Bytes   json.RawMessage `json:"bytes"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	if len(data) > MaxKernelCounterBytes || json.Unmarshal(data, &envelope) != nil {
		return KernelCounters{}, invalid
	}
	var result KernelCounters
	wanted := map[string]*networkview.Count{"denied_agent": &result.DeniedAgent, "protected_agent": &result.ProtectedAgent,
		"denied_service": &result.DeniedService, "denied_ingress": &result.DeniedIngress}
	seen := make(map[string]bool, len(wanted))
	for _, object := range envelope.Objects {
		c := object.Counter
		if c == nil || c.Family != "inet" || c.Table != "coop_net" {
			continue
		}
		target, ok := wanted[c.Name]
		// An unknown name still means a table that is not the one this gateway
		// installed. Per-grant counters are named from the frozen policy, so
		// their exact shape is checked here rather than accepted as a wildcard.
		if !ok && !grantCounterName(c.Name) || seen[c.Name] {
			return KernelCounters{}, invalid
		}
		packets, err := nftUnsigned(c.Packets)
		if err != nil {
			return KernelCounters{}, invalid
		}
		bytes, err := nftUnsigned(c.Bytes)
		if err != nil {
			return KernelCounters{}, invalid
		}
		seen[c.Name] = true
		if !ok {
			if len(result.Grants) >= MaxKernelGrantCounters {
				return KernelCounters{}, invalid
			}
			if result.Grants == nil {
				result.Grants = map[string]GrantCount{}
			}
			result.Grants[c.Name] = GrantCount{Packets: networkview.Count(packets), Bytes: networkview.Count(bytes)}
			continue
		}
		*target = networkview.Count(packets)
	}
	if len(seen) != len(wanted)+len(result.Grants) {
		return KernelCounters{}, invalid
	}
	return result, nil
}

// grantCounterName is the exact spelling the policy derives: "grant_" plus the
// first 24 lowercase hex characters of a stable rule ID.
func grantCounterName(name string) bool {
	rest, ok := strings.CutPrefix(name, "grant_")
	return ok && len(rest) == 24 && strings.Trim(rest, "0123456789abcdef") == ""
}

func nftUnsigned(raw []byte) (uint64, error) {
	text := string(raw)
	if len(text) > 0 && text[0] == '-' {
		// Pinned nft 1.0.2 formats unsigned 64-bit kernel counters through a
		// signed JSON integer: UINT64_MAX is -1. Reinterpret only this private
		// source's counter fields, never public API counters. Do not use float64.
		n, err := strconv.ParseInt(text, 10, 64)
		if err == nil && n < 0 && strconv.FormatInt(n, 10) == text {
			return uint64(n), nil
		}
	} else {
		n, err := strconv.ParseUint(text, 10, 64)
		if err == nil && strconv.FormatUint(n, 10) == text {
			return n, nil
		}
	}
	return 0, Failure("kernel_counters_invalid")
}
