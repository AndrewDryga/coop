package networkgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestPinnedNFTUnsignedJSONKeepsEveryBit(t *testing.T) {
	for text, wanted := range map[string]uint64{"0": 0, "9007199254740993": 1<<53 + 1,
		"9223372036854775807": 1<<63 - 1, "-9223372036854775808": 1 << 63, "-1": ^uint64(0), "18446744073709551615": ^uint64(0)} {
		value, err := nftUnsigned([]byte(text))
		if err != nil || value != wanted {
			t.Fatalf("%s => %d, %v; wanted %d", text, value, err, wanted)
		}
	}
	for _, text := range []string{"", "null", "1.0", "1e2", "\"1\"", "+1", "01", "-0", "-01", "18446744073709551616", "-9223372036854775809"} {
		if _, err := nftUnsigned([]byte(text)); err == nil {
			t.Errorf("accepted invalid private integer %q", text)
		}
	}
}

func kernelFixture() string {
	var objects []string
	for _, name := range []string{"denied_agent", "protected_agent", "denied_ingress", "denied_service"} {
		objects = append(objects, fmt.Sprintf(`{"counter":{"family":"inet","table":"coop_net","name":%q,"handle":1,"packets":9007199254740993,"bytes":-1}}`, name))
	}
	return `{"nftables":[{"metainfo":{"version":"1.0.2"}},` + strings.Join(objects, ",") + `]}`
}

func TestKernelParserRequiresExactCompleteDisjointCounters(t *testing.T) {
	valid := kernelFixture()
	got, err := parseKernelCounters([]byte(valid))
	if err != nil || uint64(got.ProtectedAgent) != 1<<53+1 || got.DeniedAgent != got.DeniedService || got.DeniedIngress != got.DeniedService {
		t.Fatalf("lost packet counter precision: %#v %v", got, err)
	}
	for _, text := range []string{
		strings.Replace(valid, `"denied_agent"`, `"protected_agent"`, 1),
		strings.Replace(valid, `"coop_net"`, `"other"`, 1),
		strings.Replace(valid, `"inet"`, `"ip"`, 1),
		strings.Replace(valid, `"packets":9007199254740993`, `"packets":null`, 1),
		strings.Replace(valid, `"bytes":-1`, `"bytes":1.0`, 1),
		valid + `{}`, strings.Repeat(" ", MaxKernelCounterBytes+1), `{}`,
	} {
		if _, err := parseKernelCounters([]byte(text)); err == nil {
			t.Fatal("accepted malformed, duplicate, missing, or oversized kernel evidence")
		}
	}
}

func TestKernelSamplingIsCachedAndIndependentOfControl(t *testing.T) {
	c := testController(t, func(context.Context, string) error { return nil })
	if err := c.Initialize(context.Background(), netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	entered := make(chan struct{})
	c.kernel.read = func(ctx context.Context) (KernelCounters, error) {
		if reads.Add(1) == 1 {
			close(entered)
		}
		<-ctx.Done()
		return KernelCounters{}, ctx.Err()
	}
	client, _, _ := startControlFixture(t, c, func(*net.UnixConn) bool { return true })
	select {
	case <-entered:
	case <-time.After(wait.Deadline):
		t.Fatal("sampler did not start")
	}
	for range 20 {
		sample, err := client.countersAfter(context.Background(), 0)
		if err != nil || sample.Counters != nil || sample.Reason != "kernel_counters_unavailable" {
			t.Fatalf("unmeasured kernel counter claimed available: %#v %v", sample, err)
		}
		if err := client.Ready(context.Background()); err != nil {
			t.Fatal("slow sampler blocked controller health", err)
		}
	}
	// Successful observation does not turn its endpoint into a control API.
	for _, operation := range []string{"heartbeat", "ready", "lease"} {
		if _, err := client.exchange(context.Background(), controlRequest{Version: PrivateProtocolVersion, Operation: operation}, client.Path+".observe"); err == nil {
			t.Fatalf("observation endpoint accepted %s", operation)
		}
	}
	wrong := client
	wrong.Identity.Epoch = strings.Repeat("f", 32)
	if _, err := wrong.countersAfter(context.Background(), 0); err == nil {
		t.Fatal("observation crossed gateway epoch")
	}
	if !c.Ready() {
		t.Fatal("counter failure closed admission")
	}
}

func TestKernelSnapshotIsDetachedAndBounded(t *testing.T) {
	refused := RefusedAttempt{Peer: netip.MustParseAddrPort("203.0.113.9:8443"), SourcePort: 52396}
	k := kernelEvents{sample: KernelSample{Sequence: 1, Counters: &KernelCounters{DeniedAgent: 3, Refused: []RefusedAttempt{refused}}}}
	s := k.snapshot()
	s.Counters.DeniedAgent = 99
	s.Counters.Refused[0].SourcePort = 1
	if k.snapshot().Counters.DeniedAgent != 3 || k.snapshot().Counters.Refused[0] != refused {
		t.Fatal("caller mutated sampling cache")
	}
	data, err := json.Marshal(controlReply{Kernel: &s})
	if err != nil || len(data) >= maxControlBytes {
		t.Fatal("counter reply exceeds private frame")
	}
	// The widest reply the controller can send — every grant counter and a full refused set at
	// their largest values — fits the reply frame and reads back whole.
	most := networkview.Count(^uint64(0))
	widest := KernelCounters{DeniedAgent: most, ProtectedAgent: most, DeniedService: most, DeniedIngress: most, Grants: map[string]GrantCount{}}
	for i := range MaxKernelGrantCounters {
		widest.Grants[fmt.Sprintf("grant_%024x", i)] = GrantCount{Packets: most, Bytes: most}
	}
	for i := range MaxRefusedAttempts {
		widest.Refused = append(widest.Refused, RefusedAttempt{Peer: netip.AddrPortFrom(netip.AddrFrom4([4]byte{255, 255, 255, byte(i)}), 65535), SourcePort: 65535})
	}
	reply := controlReply{Identity: Identity{Clock: ClockDomain{BootID: strings.Repeat("f", 36), TimeNamespace: "18446744073709551615", BootOffset: strings.Repeat("9", 40)},
		RunID: strings.Repeat("a", 32), Epoch: strings.Repeat("b", 32), PolicyFingerprint: strings.Repeat("c", 64)}, Version: PrivateProtocolVersion, Ready: true,
		Kernel: &KernelSample{Sequence: most, At: time.Now(), BootAt: BootInstant(1<<62 + 1), StartedBoot: BootInstant(1<<62 + 1), Counters: &widest, EnforcerReady: true}}
	frame, err := json.Marshal(reply)
	if err != nil || len(frame)+1 > maxControlReplyBytes {
		t.Fatalf("the widest counters reply (%d bytes) exceeds the reply frame (%d)", len(frame)+1, maxControlReplyBytes)
	}
	var back controlReply
	if err := readFrame(strings.NewReader(string(frame)+"\n"), &back, maxControlReplyBytes); err != nil || len(back.Kernel.Counters.Refused) != MaxRefusedAttempts ||
		len(back.Kernel.Counters.Grants) != MaxKernelGrantCounters {
		t.Fatalf("the widest counters reply did not read back whole: %v", err)
	}
	out := &boundedOutput{limit: 5}
	if _, err := out.Write([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write([]byte("6")); err == nil || !out.exceeded || out.Len() != 5 {
		t.Fatal("unbounded subprocess output")
	}
	pumped := &boundedOutput{limit: 5}
	if _, err := io.Copy(pumped, struct{ io.Reader }{strings.NewReader("123456")}); err == nil || !pumped.exceeded || pumped.Len() > 5 {
		t.Fatal("io.Copy optimized around the subprocess output limit")
	}
}

func TestKernelTerminalBarrierRequiresSampleStartedAfterCutoff(t *testing.T) {
	cutoff := BootInstant(time.Hour)
	k := kernelEvents{sample: KernelSample{Sequence: 1, StartedBoot: cutoff.Add(-time.Millisecond), BootAt: cutoff.Add(time.Millisecond), Counters: &KernelCounters{DeniedAgent: 3}}}
	probe, stopProbe := context.WithCancel(context.Background())
	stopProbe()
	if got := k.after(probe, cutoff); got.Counters != nil {
		t.Fatal("read completion after cutoff accepted a pre-cutoff sample")
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
	defer cancel()
	done := make(chan KernelSample, 1)
	go func() { done <- k.after(ctx, cutoff) }()
	// A cached read completed after cutoff but began before it: it cannot
	// prove inclusion of the terminal tail. Publish a qualifying generation.
	k.mu.Lock()
	k.sample = KernelSample{Sequence: 2, StartedBoot: cutoff, BootAt: cutoff.Add(time.Millisecond), Counters: &KernelCounters{DeniedAgent: 4}}
	k.mu.Unlock()
	select {
	case got := <-done:
		if got.Sequence != 2 || got.Counters.DeniedAgent != 4 {
			t.Fatal("terminal barrier accepted pre-cutoff sample")
		}
	case <-ctx.Done():
		t.Fatal("terminal kernel barrier did not complete")
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if got := k.after(canceled, cutoff.Add(time.Second)); got.Counters != nil {
		t.Fatal("canceled terminal barrier fabricated measured counters")
	}
}

// A terminal barrier does not wait out the sampler's one-second tick: it wakes the sampler, whose next
// sample starts after the cutoff — so a run's teardown pays one counter read, not up to a second.
func TestKernelTerminalBarrierWakesTheSampler(t *testing.T) {
	clock := testBootClock()
	first := make(chan struct{})
	var reads atomic.Int32
	k := &kernelEvents{read: func(context.Context) (KernelCounters, error) {
		if reads.Add(1) == 1 {
			close(first)
		}
		return KernelCounters{DeniedAgent: 1}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); k.run(ctx, clock) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(wait.Deadline):
			t.Error("kernel sampler did not drain")
		}
	})
	select {
	case <-first:
	case <-time.After(wait.Deadline):
		t.Fatal("first kernel sample did not start")
	}
	wait.For(t, "first kernel sample published", func() bool { return k.snapshot().Sequence > 0 })
	cutoff := clock.instant()
	began := time.Now()
	barrier, stop := context.WithTimeout(ctx, wait.Deadline)
	defer stop()
	got := k.after(barrier, cutoff)
	if got.Counters == nil || got.StartedBoot.Before(cutoff) {
		t.Fatalf("terminal sample = %#v, want one started after the cutoff", got)
	}
	if took := time.Since(began); took > 500*time.Millisecond {
		t.Fatalf("the terminal barrier waited %s for the sampler's tick", took)
	}
}

// refusedSetFixture is `nft -j list set inet coop_net refused4` as pinned nft 1.0.2 printed it in a
// box (2026-11-06) after two refused connections; an empty set prints no "elem" at all.
func refusedSetFixture(elements string) string {
	set := `{"family": "inet", "name": "refused4", "table": "coop_net", "type": ["ipv4_addr", "inet_service", "inet_service"], "handle": 5, "size": 128, "flags": ["timeout"], "timeout": 10`
	if elements != "" {
		set += `, "elem": [` + elements + `]`
	}
	return `{"nftables": [{"metainfo": {"version": "1.0.2", "release_name": "Lester Gooch", "json_schema_version": 1}}, {"set": ` + set + `}}]}`
}

func TestRefusedSetParserReadsOnlyThePinnedShape(t *testing.T) {
	const two = `{"elem": {"val": {"concat": ["203.0.113.10", 80, 44032]}, "expires": 6}}, {"elem": {"val": {"concat": ["203.0.113.9", 8443, 52396]}, "expires": 5}}`
	got, err := parseRefusedAttempts([]byte(refusedSetFixture(two)))
	want := []RefusedAttempt{{Peer: netip.MustParseAddrPort("203.0.113.10:80"), SourcePort: 44032}, {Peer: netip.MustParseAddrPort("203.0.113.9:8443"), SourcePort: 52396}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("refused set = %v, %v; want %v", got, err, want)
	}
	if got, err := parseRefusedAttempts([]byte(refusedSetFixture(""))); err != nil || len(got) != 0 {
		t.Fatalf("an empty set = %v, %v", got, err)
	}
	// The agent can dial port 0: it is read, never a reason to lose the sample.
	if got, err := parseRefusedAttempts([]byte(refusedSetFixture(`{"elem": {"val": {"concat": ["203.0.113.10", 0, 44032]}, "expires": 6}}`))); err != nil || len(got) != 1 || got[0].Peer.Port() != 0 {
		t.Fatalf("port 0 = %v, %v", got, err)
	}
	element := func(i int) string {
		return fmt.Sprintf(`{"elem": {"val": {"concat": ["192.0.2.1", %d, 40000]}, "expires": 9}}`, i+1)
	}
	var full []string
	for i := range MaxRefusedAttempts + 1 {
		full = append(full, element(i))
	}
	if got, err := parseRefusedAttempts([]byte(refusedSetFixture(strings.Join(full[:MaxRefusedAttempts], ", ")))); err != nil || len(got) != MaxRefusedAttempts {
		t.Fatalf("a full set = %d, %v", len(got), err)
	}
	valid := refusedSetFixture(two)
	for _, text := range []string{
		strings.Replace(valid, `"coop_net"`, `"other"`, 1),
		strings.Replace(valid, `"refused4"`, `"leases4"`, 1),
		strings.Replace(valid, `"inet"`, `"ip"`, 1),
		strings.Replace(valid, `"203.0.113.10"`, `"2001:db8::1"`, 1),
		strings.Replace(valid, `"203.0.113.10"`, `"203.0.113.010"`, 1),
		strings.Replace(valid, `80, 44032`, `65536, 44032`, 1),
		strings.Replace(valid, `80, 44032`, `"80", 44032`, 1),
		strings.Replace(valid, `80, 44032`, `80.0, 44032`, 1),
		strings.Replace(valid, `80, 44032`, `80`, 1),
		strings.Replace(valid, `{"elem": {"val": {"concat": ["203.0.113.10", 80, 44032]}, "expires": 6}}`, `{"concat": ["203.0.113.10", 80, 44032]}`, 1),
		strings.Replace(valid, `]}}]}`, `]}}, {"set": {"family": "inet", "name": "refused4", "table": "coop_net"}}]}`, 1),
		refusedSetFixture(strings.Join(full, ", ")),
		`{"nftables": [{"metainfo": {"version": "1.0.2"}}]}`,
		valid + `{}`, `{}`,
	} {
		if got, err := parseRefusedAttempts([]byte(text)); err == nil {
			t.Errorf("accepted a refused set that is not the one this gateway installed (%v):\n%s", got, text[:min(len(text), 200)])
		}
	}
}

func TestKernelGrantCountersValidateNamesPrecisionAndCapacity(t *testing.T) {
	withGrants := func(names []string) string {
		var extra strings.Builder
		for _, name := range names {
			fmt.Fprintf(&extra, `,{"counter":{"family":"inet","table":"coop_net","name":%q,"packets":0,"bytes":0}}`, name)
		}
		return strings.TrimSuffix(kernelFixture(), "]}") + extra.String() + "]}"
	}
	name := "grant_" + strings.Repeat("a", 24)
	valid := strings.Replace(withGrants([]string{name}), `"name":"`+name+`","packets":0,"bytes":0`, `"name":"`+name+`","packets":9007199254740993,"bytes":-1`, 1)
	got, err := parseKernelCounters([]byte(valid))
	if err != nil || uint64(got.Grants[name].Packets) != 1<<53+1 || uint64(got.Grants[name].Bytes) != ^uint64(0) {
		t.Fatalf("grant precision: %+v %v", got, err)
	}
	for _, names := range [][]string{{name, name}, {"grant_" + strings.Repeat("A", 24)}, {"grant_" + strings.Repeat("a", 23)}, {"grant_" + strings.Repeat("a", 25)}, {"grant_" + strings.Repeat("g", 24)}, {"other_" + strings.Repeat("a", 24)}} {
		if _, err := parseKernelCounters([]byte(withGrants(names))); err == nil {
			t.Fatalf("accepted counters: %q", names)
		}
	}
	for _, count := range []int{MaxKernelGrantCounters, MaxKernelGrantCounters + 1} {
		names := make([]string, count)
		for i := range names {
			names[i] = fmt.Sprintf("grant_%024x", i)
		}
		raw := withGrants(names)
		if len(raw) > MaxKernelCounterBytes {
			t.Fatal("count fixture exceeded byte bound")
		}
		got, err := parseKernelCounters([]byte(raw))
		if count == MaxKernelGrantCounters {
			if err != nil || len(got.Grants) != count {
				t.Fatalf("exact grant limit: %d %v", len(got.Grants), err)
			}
		} else if err == nil {
			t.Fatal("excess grant count accepted")
		}
	}
}
