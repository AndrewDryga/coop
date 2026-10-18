package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// UsageSpec keeps native quota, history and tariff differences in their owning adapter.
// Native is supplied lazily by the host; most quota reads need no container runtime.
type UsageSpec struct {
	Quota                 func(context.Context, UsageQuotaInput) (UsageQuota, error)
	HostHomes             func() []string
	Reports               usageReportSpec
	HistoryDirs           []string
	HistoryFile           func(string) bool
	HistoryFilter         func(*os.Root, string) (bool, error)
	HistoryCacheID        string
	StreamHistory         bool // The decoder bounds individual records rather than whole files.
	ParseHistory          func(io.Reader) (UsageHistory, error)
	Price                 func(UsageEvent) (float64, bool)
	NativeCredentialLease bool
	NativeCredentialCheck func(string) error
	NativeEnv             []string
}

// Native host history has no historical account binding. Only the owning CLI's declared
// home is scanned; a current login is never evidence about older requests.
func nativeUsageHomes(dir, env string, parent bool) []string {
	var homes []string
	if home, err := os.UserHomeDir(); err == nil {
		homes = append(homes, filepath.Join(home, dir))
	}
	if home := os.Getenv(env); home != "" {
		if parent {
			home = filepath.Join(home, dir)
		}
		home = filepath.Clean(home)
		if !slices.Contains(homes, home) {
			homes = append(homes, home)
		}
	}
	return homes
}

type UsageQuotaInput struct {
	ProfileDir string
	APIKey     bool
	EnvKey     string
	Native     func(context.Context, []string) ([]byte, error)
	Current    func(context.Context, time.Time) (NativeCredentialState, map[string][]byte, error)
}

type UsageQuota struct {
	Plan    string
	Buckets []UsageBucket
	Resets  *UsageResetCredits
	Note    string
	// Auth names a sign-in kind that has no provider limits to report, such as "API key"; the
	// summary shows it beside the account instead of explaining the missing limits.
	Auth string
}

// Available is authoritative even when the provider caps its expiry detail list. A nil count
// is unknown, not zero; expirations describe only returned available credits, never all credits.
type UsageResetCredits struct {
	Available   *int64
	Expirations []time.Time
}

type UsageBucket struct {
	Name      string
	Used      *float64 // percent; nil is unknown, not zero
	Remaining string   // provider-reported amount, not inferred from a percentage
	Reset     time.Time
	Available *bool
	Note      string
}

// UsageEvent is one native request, not a session/stage aggregate. Tokens are normalized to
// disjoint categories by the adapter. ID must survive transcript copies and repeated snapshots.
type UsageEvent struct {
	ID, Model                                         string
	Time                                              time.Time
	Input, Read, Write, Write1h, UnknownWrite, Output int64
	Partial, Approximate                              bool
	WriteKnown                                        bool
	ContextKnown                                      bool
}

type UsageHistory struct {
	Events    []UsageEvent
	Partial   bool
	Available bool
}

// Rates are USD per million tokens at standard public list prices, never native reported cost.
type usageTariff struct {
	Input, Read, Write, Write1h, Output float64
	LongAt                              int64
	LongInput, LongOutput               float64
	RequireWrites                       bool
}

func (rate usageTariff) value(event UsageEvent) (float64, bool) {
	if !event.valid() || event.UnknownWrite != 0 || (rate.RequireWrites && !event.WriteKnown) {
		return 0, false
	}
	inputScale, outputScale := 1.0, 1.0
	input := event.Input + event.Read + event.Write + event.Write1h
	if rate.LongAt != 0 && input >= rate.LongAt {
		if !event.ContextKnown {
			return 0, false
		}
		inputScale, outputScale = rate.LongInput, rate.LongOutput
	}
	return (inputScale*(float64(event.Input)*rate.Input+float64(event.Read)*rate.Read+float64(event.Write)*rate.Write+float64(event.Write1h)*rate.Write1h) + outputScale*float64(event.Output)*rate.Output) / 1e6, true
}

type UsageValue struct {
	USD                             float64
	Priced, Unpriced                int
	Partial, Approximate, Available bool
}

func ValueUsageHistory(history UsageHistory, now time.Time, price func(UsageEvent) (float64, bool)) UsageValue {
	out := UsageValue{Partial: history.Partial, Available: history.Available}
	cutoff := now.Add(-30 * 24 * time.Hour)
	events := make(map[string]UsageEvent)
	for _, event := range history.Events {
		if event.Time.Before(cutoff) || event.Time.After(now) {
			continue
		}
		prior, exists := events[event.ID]
		if !exists || (prior.Partial && !event.Partial) || (prior.Partial == event.Partial && event.Time.After(prior.Time)) {
			events[event.ID] = event
		}
	}
	ids := make([]string, 0, len(events))
	for id := range events {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		event := events[id]
		value, ok := price(event)
		if ok {
			out.USD += value
			out.Priced++
		} else {
			out.Unpriced++
		}
		out.Partial = out.Partial || event.Partial
		out.Approximate = out.Approximate || event.Approximate
	}
	return out
}

var ErrUsageSignIn = errors.New("sign-in required")

// Quota responses and errors never expose provider bodies, URLs from stored credentials, or
// reusable tokens. Redirects are refused rather than forwarding authority to another endpoint.
func readUsageQuota(ctx context.Context, method, endpoint string, headers http.Header, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return errors.New("cannot construct quota request")
	}
	req.Header = headers
	client := http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("quota lookup timed out")
		}
		return errors.New("quota request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUsageSignIn
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("quota lookup returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("quota response exceeds its read limit or is incomplete")
	}
	if json.Unmarshal(data, out) != nil {
		return errors.New("quota response has an unsupported format")
	}
	return nil
}

func usagePercent(value *float64) *float64 {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		return nil
	}
	return value
}

func usageReset(value string) time.Time {
	reset, _ := time.Parse(time.RFC3339Nano, value)
	return reset
}

func readUsageLines(reader io.Reader, visit func([]byte) bool) bool {
	lines := bufio.NewReaderSize(reader, 64<<10)
	partial := false
	var line []byte
	oversized := false
	for {
		part, err := lines.ReadSlice('\n')
		if !oversized {
			if len(line)+len(part) > 8<<20 {
				oversized, partial = true, true
				line = line[:0]
			} else {
				line = append(line, part...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if len(line) != 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}
		if len(line) != 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if !oversized && len(line) != 0 && !visit(line) {
			partial = true
		}
		line = line[:0]
		oversized = false
		if err != nil {
			return partial || err != io.EOF
		}
	}
}

func usageTokens(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func (event UsageEvent) valid() bool {
	return event.ID != "" && len(event.ID) <= 1024 && len(event.Model) <= 256 && !event.Time.IsZero() &&
		event.Input >= 0 && event.Read >= 0 && event.Write >= 0 && event.Write1h >= 0 && event.UnknownWrite >= 0 && event.Output >= 0 &&
		max(event.Input, event.Read, event.Write, event.Write1h, event.UnknownWrite, event.Output) <= 1e12
}

func readRootUsageArtifact(root *os.Root, path string, limit int64) ([]byte, error) {
	before, err := root.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > limit {
		return nil, errors.New("history is not a bounded regular file")
	}
	file, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Size() > limit {
		return nil, errors.New("history changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("history exceeds its read limit")
	}
	return data, nil
}

// ReadUsageHistory scans every adapter-declared history file, retaining old cumulative baselines.
// Unchanged files reuse normalized token records; prices and the rolling window stay uncached.
func ReadUsageHistory(ctx context.Context, profile string, spec UsageSpec, cache *UsageCache) UsageHistory {
	var out UsageHistory
	root, err := openSessionRoot(profile)
	if err != nil {
		out.Partial = !errors.Is(err, os.ErrNotExist)
		if out.Partial {
			cache.skip(profile, spec.HistoryCacheID)
		}
		return out
	}
	defer root.Close()
	entries := cache.open(profile, spec.HistoryCacheID)
	// The walk is complete when every history file reached its cache lookup; only then may the
	// root's other entries be pruned. A record the decoder rejects leaves it complete.
	complete := true
	defer func() { entries.finish(complete) }()
	missed := func() { out.Partial, complete = true, false }
	var walk func(string, int)
	walk = func(path string, depth int) {
		if ctx.Err() != nil || depth > 64 {
			missed()
			return
		}
		info, err := root.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			missed()
			return
		}
		if info.IsDir() {
			dir, err := root.Open(path)
			if err != nil {
				missed()
				return
			}
			defer dir.Close()
			for {
				batch, err := dir.ReadDir(128)
				for _, entry := range batch {
					walk(filepath.Join(path, entry.Name()), depth+1)
				}
				if err != nil {
					if err != io.EOF {
						missed()
					}
					break
				}
				if ctx.Err() != nil {
					missed()
					break
				}
			}
			return
		}
		if !spec.HistoryFile(path) {
			return
		}
		if !info.Mode().IsRegular() || !spec.StreamHistory && info.Size() > 128<<20 {
			missed()
			return
		}
		if spec.HistoryFilter != nil {
			keep, err := spec.HistoryFilter(root, path)
			if err != nil {
				missed()
				return
			}
			if !keep {
				return
			}
		}
		if history, ok := entries.read(path, info); ok {
			current, err := root.Lstat(path)
			if err != nil || !current.Mode().IsRegular() || usageFingerprint(info) != usageFingerprint(current) {
				out.Partial = true
				return
			}
			out.Partial = out.Partial || history.Partial
			out.Events = append(out.Events, history.Events...)
			out.Available = out.Available || len(history.Events) > 0
			return
		}
		file, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			out.Partial = true
			return
		}
		defer file.Close()
		after, err := file.Stat()
		if err != nil || !os.SameFile(info, after) || !after.Mode().IsRegular() || !spec.StreamHistory && after.Size() > 128<<20 {
			out.Partial = true
			return
		}
		reader := &usageContextReader{ctx: ctx, reader: io.LimitReader(file, after.Size())}
		history, err := spec.ParseHistory(reader)
		final, statErr := file.Stat()
		current, pathErr := root.Lstat(path)
		stable := statErr == nil && pathErr == nil && current.Mode().IsRegular() &&
			usageFingerprint(after) == usageFingerprint(final) && usageFingerprint(after) == usageFingerprint(current)
		if !stable || ctx.Err() != nil || reader.err != nil {
			history.Partial = true
		} else if err == nil {
			entries.write(path, after, history)
		}
		out.Partial = out.Partial || history.Partial || err != nil
		out.Events = append(out.Events, history.Events...)
		out.Available = out.Available || len(history.Events) > 0
	}
	for _, path := range spec.HistoryDirs {
		walk(path, 0)
	}
	return out
}

type usageContextReader struct {
	ctx    context.Context
	reader io.Reader
	err    error
}

func (r *usageContextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(data)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}
