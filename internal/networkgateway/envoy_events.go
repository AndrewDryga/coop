package networkgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const MaxEnvoyEventBytes = 1024

// EnvoyEvent is private proxy evidence, not a request or a peer-delivery receipt.
// A zero meter in a start event is not proof a byte meter exists. The collector
// must interpret lifecycle phases before treating these counters as measured.
type EnvoyEvent struct {
	Sequence                       uint64
	At                             time.Time
	BootAt                         BootInstant
	FlowID                         string
	ConnectionID                   string
	Phase                          string
	Peer, Local                    netip.AddrPort
	Sent, Received, DurationMillis *uint64
	ConnectMillis                  *uint64
	Flags                          []string
	CloseType                      string
}

type EnvoyTotals struct {
	Sequence, Lost, Malformed uint64
	Saturated, Stopped        bool
	UnknownLoss               bool
	StopReason                string
}

// EnvoyEvents continuously drains process output even when retained detail is
// full or malformed. Backpressure/lifetime limits must not stop valid streams.
type EnvoyEvents struct {
	mu         sync.Mutex
	clock      *BootClock
	queue      chan EnvoyEvent
	pending    []byte
	discarding bool
	totals     EnvoyTotals
}

func NewEnvoyEvents(clock *BootClock) *EnvoyEvents {
	return &EnvoyEvents{clock: clock, queue: make(chan EnvoyEvent, MaxGuardEvents)}
}

func (e *EnvoyEvents) Write(data []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	length := len(data)
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		part := data
		if end >= 0 {
			part = data[:end]
		}
		if !e.discarding {
			if len(e.pending)+len(part) > MaxEnvoyEventBytes {
				e.pending, e.discarding = nil, true
			} else {
				e.pending = append(e.pending, part...)
			}
		}
		if end < 0 {
			break
		}
		e.line()
		e.pending, e.discarding = nil, false
		data = data[end+1:]
	}
	return length, nil
}

func (e *EnvoyEvents) increment(value *uint64) {
	if *value == ^uint64(0) {
		e.totals.Saturated = true
	} else {
		*value++
	}
}

func (e *EnvoyEvents) line() {
	if e.totals.Sequence == ^uint64(0) || e.totals.Stopped {
		e.totals.Saturated = true
		e.increment(&e.totals.Lost)
		return
	}
	e.increment(&e.totals.Sequence)
	event, err := parseEnvoyEvent(e.pending)
	if err != nil || e.discarding {
		e.increment(&e.totals.Malformed)
		e.increment(&e.totals.Lost)
		return
	}
	event.Sequence, event.At, event.BootAt = e.totals.Sequence, time.Now().UTC(), e.clock.instant()
	select {
	case e.queue <- event:
	default:
		e.increment(&e.totals.Lost)
	}
}

// Finish preserves exec.Wait's drain result. WaitDelay may forcibly close pipes,
// even with no partial tail in this parser; absence of bytes is not clean EOF.
func (e *EnvoyEvents) Finish(waitErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.totals.Stopped {
		return
	}
	e.totals.StopReason = "drained"
	if waitErr != nil {
		e.totals.UnknownLoss, e.totals.StopReason = true, "process_exit_error"
		if errors.Is(waitErr, exec.ErrWaitDelay) {
			e.totals.StopReason = "drain_incomplete"
		}
	}
	if len(e.pending) != 0 || e.discarding {
		e.increment(&e.totals.Lost)
		e.increment(&e.totals.Malformed)
	}
	e.pending, e.discarding, e.totals.Stopped = nil, false, true
}

func (e *EnvoyEvents) Drain(limit int) ([]EnvoyEvent, EnvoyTotals) {
	e.mu.Lock()
	defer e.mu.Unlock()
	limit = max(0, min(limit, MaxGuardEvents))
	events := make([]EnvoyEvent, 0, min(limit, len(e.queue)))
	for range limit {
		select {
		case event := <-e.queue:
			events = append(events, event)
		default:
			return events, e.totals
		}
	}
	return events, e.totals
}

func parseEnvoyEvent(data []byte) (EnvoyEvent, error) {
	var raw struct {
		FlowID       string `json:"flow_id"`
		ConnectionID string `json:"connection_id"`
		Phase        string `json:"phase"`
		Peer         string `json:"peer"`
		Local        string `json:"local"`
		Sent         string `json:"sent"`
		Received     string `json:"received"`
		Duration     string `json:"duration_ms"`
		Connect      string `json:"connect_ms"`
		Flags        string `json:"flags"`
		CloseType    string `json:"close_type"`
	}
	if len(data) > MaxEnvoyEventBytes {
		return EnvoyEvent{}, Failure("observation_invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&raw) != nil || decoder.Decode(new(any)) != io.EOF || !lowerHex(raw.FlowID, 32) {
		return EnvoyEvent{}, Failure("observation_invalid")
	}
	if _, err := decimal(raw.ConnectionID); err != nil || raw.ConnectionID == "-" {
		return EnvoyEvent{}, Failure("observation_invalid")
	}
	switch raw.Phase {
	case "TcpConnectionStart", "TcpUpstreamConnected", "TcpPeriodic", "TcpConnectionEnd":
	default:
		return EnvoyEvent{}, Failure("observation_invalid")
	}
	result := EnvoyEvent{FlowID: raw.FlowID, ConnectionID: raw.ConnectionID, Phase: raw.Phase}
	for _, pair := range []struct {
		value  string
		target *netip.AddrPort
	}{{raw.Peer, &result.Peer}, {raw.Local, &result.Local}} {
		value, target := pair.value, pair.target
		if value == "-" {
			continue
		}
		address, err := netip.ParseAddrPort(value)
		if err != nil || !address.Addr().Is4() || address.Port() == 0 {
			return EnvoyEvent{}, Failure("observation_invalid")
		}
		*target = address
	}
	var err error
	if result.Sent, err = decimal(raw.Sent); err != nil {
		return EnvoyEvent{}, err
	}
	if result.Received, err = decimal(raw.Received); err != nil {
		return EnvoyEvent{}, err
	}
	if result.DurationMillis, err = decimal(raw.Duration); err != nil {
		return EnvoyEvent{}, err
	}
	if result.ConnectMillis, err = decimal(raw.Connect); err != nil {
		return EnvoyEvent{}, err
	}
	if len(raw.Flags) > 128 || raw.Flags == "" {
		return EnvoyEvent{}, Failure("observation_invalid")
	}
	if raw.Flags != "-" {
		seen := make(map[string]bool)
		for _, flag := range strings.Split(raw.Flags, ",") {
			switch flag {
			case "DC", "LH", "UH", "UT", "LR", "UR", "UF", "UC", "UO", "URX", "NR", "DI", "FI", "RL", "UAEX", "RLSE", "SI", "IH", "DPE", "UMSDR", "RFCF", "NFCF", "DT", "UPE", "NC", "OM", "DF", "DO", "DR", "UDO":
			default:
				return EnvoyEvent{}, Failure("observation_invalid")
			}
			if seen[flag] {
				return EnvoyEvent{}, Failure("observation_invalid")
			}
			seen[flag] = true
			result.Flags = append(result.Flags, flag)
		}
	}
	switch raw.CloseType {
	case "-":
	case "Normal", "LocalReset", "RemoteReset":
		result.CloseType = raw.CloseType
	default:
		return EnvoyEvent{}, Failure("observation_invalid")
	}
	return result, nil
}

func decimal(value string) (*uint64, error) {
	if value == "-" {
		return nil, nil
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != value {
		return nil, Failure("observation_invalid")
	}
	return &n, nil
}
