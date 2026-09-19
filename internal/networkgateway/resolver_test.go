package networkgateway

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func aRecord(name, ip string, ttl uint32) dnsmessage.Resource {
	return dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name + "."), Class: dnsmessage.ClassINET, TTL: ttl},
		Body: &dnsmessage.AResource{A: netip.MustParseAddr(ip).As4()}}
}

func cnameRecord(name, target string, ttl uint32) dnsmessage.Resource {
	return dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name + "."), Class: dnsmessage.ClassINET, TTL: ttl},
		Body: &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName(target + ".")}}
}

func answerExchange(t *testing.T, answers func(string) []dnsmessage.Resource) Exchange {
	t.Helper()
	return func(_ context.Context, wire []byte) ([]byte, error) {
		var query dnsmessage.Message
		if err := query.Unpack(wire); err != nil || len(query.Questions) != 1 {
			return nil, errors.New("bad test query")
		}
		name := query.Questions[0].Name.String()
		response := dnsmessage.Message{Header: dnsmessage.Header{ID: query.ID, Response: true, RecursionAvailable: true}, Questions: query.Questions,
			Answers: answers(name[:len(name)-1])}
		return response.Pack()
	}
}

func newTestResolver(t *testing.T, exchange Exchange) *Resolver {
	t.Helper()
	r, err := NewResolver(testPolicy(t), nil, nil, testBootClock(), exchange)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResolverAdmitsBeforeAnyUpstreamQuery(t *testing.T) {
	var calls atomic.Int64
	r := newTestResolver(t, func(context.Context, []byte) ([]byte, error) { calls.Add(1); return nil, errors.New("must not query") })
	for _, name := range []string{"other.example.com", "services.example.com", "a.b.services.example.com", "api.example.com.attacker.test", "localhost", "127.0.0.1", "https://api.example.com"} {
		if _, err := r.Resolve(context.Background(), name); err == nil {
			t.Errorf("resolved unapproved %q", name)
		}
	}
	for _, kind := range []dnsmessage.Type{dnsmessage.TypeHTTPS, dnsmessage.TypeSVCB, dnsmessage.TypeTXT, dnsmessage.TypeALL} {
		query, _ := makeQuery("api.example.com")
		query.Questions[0].Type = kind
		wire, _ := query.Pack()
		got, reason := r.Answer(context.Background(), wire)
		var answer dnsmessage.Message
		if err := answer.Unpack(got); err != nil || reason != "dns_type_unsupported" || answer.RCode != dnsmessage.RCodeRefused {
			t.Fatalf("unsupported DNS type: %v %q", err, reason)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("denied name/type leaked an upstream query")
	}
	if status, reason := r.health(); status != "ready" || reason != "resolver_initialized" {
		t.Fatal("unapproved agent query became upstream health evidence")
	}
}

func TestResolverAAAAIsLocalNODATAOnlyForAdmittedNames(t *testing.T) {
	var calls atomic.Int64
	r := newTestResolver(t, func(context.Context, []byte) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("AAAA must not reach upstream")
	})
	for _, tc := range []struct {
		name   string
		class  dnsmessage.Class
		reason string
	}{
		{"api.example.com", dnsmessage.ClassINET, ""},
		{"API.EXAMPLE.COM", dnsmessage.ClassINET, ""},
		{"a.services.example.com", dnsmessage.ClassINET, ""},
		{"other.example.com", dnsmessage.ClassINET, "unapproved_name"},
		{"api.example.com.attacker.test", dnsmessage.ClassINET, "unapproved_name"},
		{"localhost", dnsmessage.ClassINET, "dns_name_invalid"},
		{"api.example.com", dnsmessage.ClassCHAOS, "dns_type_unsupported"},
	} {
		query, _ := makeQuery(tc.name)
		query.Questions[0].Type, query.Questions[0].Class = dnsmessage.TypeAAAA, tc.class
		wire, _ := query.Pack()
		got, reason := r.Answer(context.Background(), wire)
		var answer dnsmessage.Message
		if err := answer.Unpack(got); err != nil || reason != tc.reason || answer.ID != query.ID || !answer.Response || len(answer.Answers) != 0 || len(answer.Authorities) != 0 || len(answer.Additionals) != 0 || len(answer.Questions) != 1 || answer.Questions[0] != query.Questions[0] {
			t.Fatalf("AAAA response for %s: %q, %v", tc.name, reason, err)
		}
		want := dnsmessage.RCodeSuccess
		if tc.reason != "" {
			want = dnsmessage.RCodeRefused
		}
		if answer.RCode != want {
			t.Fatalf("AAAA response code %v, want %v", answer.RCode, want)
		}
	}
	if calls.Load() != 0 || len(r.cache) != 0 {
		t.Fatal("AAAA query acquired upstream or cache authority")
	}
	if status, reason := r.health(); status != "ready" || reason != "resolver_initialized" {
		t.Fatal("local NODATA manufactured upstream health")
	}
}

func TestResolverHealthReflectsCompletedLookupNotCacheOrCancellation(t *testing.T) {
	now := testBootNow()
	fail := true
	success := answerExchange(t, func(name string) []dnsmessage.Resource { return []dnsmessage.Resource{aRecord(name, "1.1.1.1", 10)} })
	r := newTestResolver(t, func(ctx context.Context, wire []byte) ([]byte, error) {
		if fail {
			return nil, errors.New("private upstream diagnostics must not escape")
		}
		return success(ctx, wire)
	})
	r.now = func() BootInstant { return now }
	if _, err := r.Resolve(context.Background(), "api.example.com"); err == nil {
		t.Fatal("fixture did not fail")
	}
	if status, reason := r.health(); status != "degraded" || reason != "last_lookup_exchange_failed" {
		t.Fatal("failed lookup hidden or raw error exposed")
	}
	fail = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = r.Resolve(ctx, "api.example.com")
	_, _ = r.Resolve(context.Background(), "api.example.com") // negative cache is not recovery
	if status, _ := r.health(); status != "degraded" {
		t.Fatal("cache or cancelled caller manufactured recovery")
	}
	now = now.Add(2 * time.Second)
	if _, err := r.Resolve(context.Background(), "api.example.com"); err != nil {
		t.Fatal(err)
	}
	if status, reason := r.health(); status != "ready" || reason != "last_lookup_succeeded" {
		t.Fatal("observed successful retry did not recover resolver health")
	}
}

func TestResolverPinsFreshAnswersAndNeverReturnsStaleOnRebinding(t *testing.T) {
	now := testBootNow()
	ip := "93.184.216.34"
	r := newTestResolver(t, answerExchange(t, func(name string) []dnsmessage.Resource {
		return []dnsmessage.Resource{aRecord(name, ip, 10)}
	}))
	r.now = func() BootInstant { return now }
	first, err := r.Resolve(context.Background(), "API.EXAMPLE.COM.")
	if err != nil || first.Name != "api.example.com" || first.Cached || first.Expires != now.Add(10*time.Second) {
		t.Fatalf("initial resolution: %#v %v", first, err)
	}
	first.Addresses[0] = netip.MustParseAddr("1.1.1.1")
	second, err := r.Resolve(context.Background(), "api.example.com")
	if err != nil || !second.Cached || second.Addresses[0].String() != ip {
		t.Fatalf("cache mutated through returned slice: %#v %v", second, err)
	}
	ip = "169.254.169.254"
	now = now.Add(10 * time.Second)
	got, err := r.Resolve(context.Background(), "api.example.com")
	if err == nil || err.Error() != "unsafe_dns_answer" || len(got.Addresses) != 0 {
		t.Fatalf("rebound lookup retained stale authority: %#v %v", got, err)
	}
	got, err = r.Resolve(context.Background(), "api.example.com")
	if err == nil || !got.Cached || len(got.Addresses) != 0 {
		t.Fatal("negative cache exposed old addresses")
	}
	queries, _ := r.MaintenanceCounts()
	if queries != 2 {
		t.Fatalf("cache hit became maintenance query: %d", queries)
	}
}

func TestResolverRefreshDoesNotInvalidateNewGenerationWithSameExpiry(t *testing.T) {
	now := testBootNow()
	r := newTestResolver(t, answerExchange(t, func(name string) []dnsmessage.Resource {
		return []dnsmessage.Resource{aRecord(name, "93.184.216.34", 1)}
	}))
	r.now = func() BootInstant { return now }
	first, err := r.Resolve(context.Background(), "api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.refresh(context.Background(), first)
	if err != nil || second.Expires != first.Expires || second.generation == first.generation {
		t.Fatalf("refresh generation: %v", err)
	}
	third, err := r.refresh(context.Background(), first)
	if err != nil || !third.Cached || third.generation != second.generation {
		t.Fatal("old caller invalidated newer answer with identical expiry")
	}
	queries, _ := r.MaintenanceCounts()
	if queries != 2 {
		t.Fatal("refresh did not coalesce callers")
	}
}

func TestResolverRejectsWholeMixedUnsafeAndProtectedAnswers(t *testing.T) {
	for _, ip := range []string{"10.0.0.1", "127.0.0.1", "0.0.0.0", "169.254.169.254", "100.64.0.1", "192.0.2.1", "224.0.0.1"} {
		t.Run(ip, func(t *testing.T) {
			r := newTestResolver(t, answerExchange(t, func(name string) []dnsmessage.Resource {
				return []dnsmessage.Resource{aRecord(name, "1.1.1.1", 30), aRecord(name, ip, 30)}
			}))
			got, err := r.Resolve(context.Background(), "api.example.com")
			if err == nil || len(got.Addresses) != 0 {
				t.Fatalf("partially admitted mixed answer: %#v %v", got, err)
			}
		})
	}
	r, err := NewResolver(testPolicy(t), []netip.Prefix{netip.MustParsePrefix("93.184.216.0/24")}, nil, testBootClock(), answerExchange(t, func(name string) []dnsmessage.Resource {
		return []dnsmessage.Resource{aRecord(name, "93.184.216.34", 30)}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), "api.example.com"); err == nil {
		t.Fatal("resolved runtime-protected public address")
	}
}

func TestResolverCNAMEChainIsPlumbingNotAnotherAgentGrant(t *testing.T) {
	var queried []string
	r := newTestResolver(t, answerExchange(t, func(name string) []dnsmessage.Resource {
		queried = append(queried, name)
		if name == "api.example.com" {
			return []dnsmessage.Resource{cnameRecord(name, "edge.cdn.example.net", 20)}
		}
		return []dnsmessage.Resource{aRecord(name, "1.1.1.1", 100), aRecord(name, "1.0.0.1", 100), aRecord(name, "1.1.1.1", 100)}
	}))
	now := testBootNow()
	r.now = func() BootInstant { return now }
	got, err := r.Resolve(context.Background(), "api.example.com")
	if err != nil || got.Expires != now.Add(20*time.Second) || len(got.Addresses) != 2 || got.Addresses[0].String() != "1.0.0.1" {
		t.Fatalf("CNAME resolution: %#v %v", got, err)
	}
	if !slices.Equal(queried, []string{"api.example.com", "edge.cdn.example.net"}) {
		t.Fatalf("unexpected queries: %v", queried)
	}
	if _, err := r.Resolve(context.Background(), "edge.cdn.example.net"); err == nil || len(queried) != 2 {
		t.Fatal("CNAME became directly approved")
	}
}

// quickRollover shortens the pause before the resolver asks again for an answer that arrived
// already expired, so a test of that path does not wait the real second.
func quickRollover(t *testing.T) {
	pause := resolverRolloverPause
	resolverRolloverPause = time.Millisecond
	t.Cleanup(func() { resolverRolloverPause = pause })
}

func TestResolverMalformedChainsAndExpiredTTLFailClosed(t *testing.T) {
	quickRollover(t)
	tests := map[string][]dnsmessage.Resource{
		"cycle":                 {cnameRecord("api.example.com", "other.example.net", 10), cnameRecord("other.example.net", "api.example.com", 10)},
		"cname and address":     {cnameRecord("api.example.com", "other.example.net", 10), aRecord("api.example.com", "1.1.1.1", 10)},
		"conflicting aliases":   {cnameRecord("api.example.com", "one.example.net", 10), cnameRecord("api.example.com", "two.example.net", 10)},
		"unrelated address":     {aRecord("other.example.com", "1.1.1.1", 10)},
		"extra poisoned answer": {aRecord("api.example.com", "1.1.1.1", 10), aRecord("other.example.net", "127.0.0.1", 10)},
		"TTL zero":              {aRecord("api.example.com", "1.1.1.1", 0)},
		"no answer":             nil,
	}
	for name, records := range tests {
		t.Run(name, func(t *testing.T) {
			r := newTestResolver(t, answerExchange(t, func(string) []dnsmessage.Resource { return records }))
			if _, err := r.Resolve(context.Background(), "api.example.com"); err == nil {
				t.Fatal("admitted invalid DNS response")
			}
		})
	}
	var calls int
	r := newTestResolver(t, answerExchange(t, func(name string) []dnsmessage.Resource {
		calls++
		return []dnsmessage.Resource{cnameRecord(name, fmt.Sprintf("hop-%d.example.net", calls), 100)}
	}))
	if _, err := r.Resolve(context.Background(), "api.example.com"); err == nil || calls > MaxCNAMEs+1 {
		t.Fatalf("unbounded CNAME chain: calls=%d err=%v", calls, err)
	}
}

// An upstream cache answers a record in its last second with TTL 0, then refetches it: one pause and
// a second question get the fresh record instead of failing the flow — and a name that keeps
// answering expired still fails closed.
func TestResolverAsksAgainAfterAnAnswerThatArrivedExpired(t *testing.T) {
	quickRollover(t)
	var calls atomic.Int64
	r := newTestResolver(t, answerExchange(t, func(name string) []dnsmessage.Resource {
		ttl := uint32(20)
		if calls.Add(1) == 1 {
			ttl = 0
		}
		return []dnsmessage.Resource{cnameRecord(name, "edge.example.net", 3600), aRecord("edge.example.net", "1.1.1.1", ttl)}
	}))
	if got, err := r.Resolve(context.Background(), "api.example.com"); err != nil || len(got.Addresses) != 1 || calls.Load() != 2 {
		t.Fatalf("resolution after a rollover = %#v, %v (exchanges %d)", got, err, calls.Load())
	}
}

func TestResolverValidChainCanBeInOneReorderedAnswer(t *testing.T) {
	r := newTestResolver(t, answerExchange(t, func(name string) []dnsmessage.Resource {
		return []dnsmessage.Resource{aRecord("edge.example.net", "1.1.1.1", 100), cnameRecord(name, "edge.example.net", 40)}
	}))
	if got, err := r.Resolve(context.Background(), "api.example.com"); err != nil || len(got.Addresses) != 1 {
		t.Fatalf("valid reordered chain: %#v %v", got, err)
	}
}

func TestResolverConcurrentLookupHasOneExchangeAndBoundedCardinality(t *testing.T) {
	var calls atomic.Int64
	exchange := answerExchange(t, func(name string) []dnsmessage.Resource { return []dnsmessage.Resource{aRecord(name, "1.1.1.1", 100)} })
	r := newTestResolver(t, func(ctx context.Context, wire []byte) ([]byte, error) {
		calls.Add(1)
		return exchange(ctx, wire)
	})
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			if _, err := r.Resolve(context.Background(), "api.example.com"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent cache misses queried %d times", calls.Load())
	}
	for i := 0; i < MaxResolverNames-1; i++ {
		r.cache[fmt.Sprintf("%d.services.example.com", i)] = cacheEntry{resolution: Resolution{Expires: testBootNow().Add(time.Hour)}}
	}
	if _, err := r.Resolve(context.Background(), "new.services.example.com"); err == nil || err.Error() != "dns_capacity_exceeded" {
		t.Fatalf("cardinality limit: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("over-capacity query reached upstream")
	}
}

func TestResolverClientQueryDoesNotForwardOptionsOrWrongQuestions(t *testing.T) {
	r := newTestResolver(t, answerExchange(t, func(name string) []dnsmessage.Resource { return []dnsmessage.Resource{aRecord(name, "1.1.1.1", 100)} }))
	query, _ := makeQuery("api.example.com")
	wire, _ := query.Pack()
	answer, reason := r.Answer(context.Background(), wire)
	var parsed dnsmessage.Message
	if err := parsed.Unpack(answer); err != nil || reason != "" || len(parsed.Answers) != 1 || parsed.ID != query.ID {
		t.Fatalf("ordinary DNS query: %v %q", err, reason)
	}
	query.Questions = append(query.Questions, query.Questions[0])
	wire, _ = query.Pack()
	if _, reason := r.Answer(context.Background(), wire); reason != "dns_query_invalid" {
		t.Fatal("accepted multiple questions")
	}
	forged := make([]byte, 12)
	binary.BigEndian.PutUint16(forged[4:], 65535)
	if _, reason := r.Answer(context.Background(), forged); reason != "dns_query_invalid" {
		t.Fatal("accepted oversized header counts")
	}
}

func TestResolverLeaderCancellationDoesNotPoisonHealthyWaiter(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int64
	exchange := answerExchange(t, func(name string) []dnsmessage.Resource { return []dnsmessage.Resource{aRecord(name, "1.1.1.1", 100)} })
	r := newTestResolver(t, func(ctx context.Context, wire []byte) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return exchange(ctx, wire)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leader := make(chan error, 1)
	go func() { _, err := r.Resolve(ctx, "api.example.com"); leader <- err }()
	receiveFixture(t, "leader lookup started", started)
	waiter := make(chan error, 1)
	go func() { _, err := r.Resolve(context.Background(), "api.example.com"); waiter <- err }()
	cancel()
	if err := receiveFixture(t, "cancelled leader returned", leader); err == nil {
		t.Fatal("cancelled lookup succeeded")
	}
	if err := receiveFixture(t, "healthy waiter returned", waiter); err != nil {
		t.Fatal("healthy waiter inherited cancelled lookup:", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("healthy waiter did not retry exactly once: %d", calls.Load())
	}
}

func receiveFixture[T any](t *testing.T, what string, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(wait.Deadline):
		t.Fatalf("%s did not happen within %s", what, wait.Deadline)
		var zero T
		return zero
	}
}

func TestResolverTTLStartsAtEachResponseReceipt(t *testing.T) {
	now := testBootNow()
	start := now
	r := newTestResolver(t, answerExchange(t, func(name string) []dnsmessage.Resource {
		if name == "api.example.com" {
			return []dnsmessage.Resource{cnameRecord(name, "edge.example.net", 100)}
		}
		now = now.Add(2 * time.Second)
		return []dnsmessage.Resource{aRecord(name, "1.1.1.1", 1)}
	}))
	r.now = func() BootInstant { return now }
	got, err := r.Resolve(context.Background(), "api.example.com")
	if err != nil || got.Expires != start.Add(3*time.Second) {
		t.Fatalf("fresh low-TTL final CNAME answer treated as expired: %#v %v", got, err)
	}
}

func FuzzDNSQueriesDoNotResolveUnadmittedNames(f *testing.F) {
	f.Add([]byte("not DNS"))
	f.Add(make([]byte, 12))
	f.Fuzz(func(t *testing.T, wire []byte) {
		r := newTestResolver(t, func(_ context.Context, query []byte) ([]byte, error) {
			var message dnsmessage.Message
			if err := message.Unpack(query); err != nil || len(message.Questions) != 1 || !runnableName(t, message.Questions[0].Name.String()) {
				t.Fatal("unadmitted upstream question")
			}
			return nil, errors.New("fixture unavailable")
		})
		answer, _ := r.Answer(context.Background(), wire)
		if len(answer) > MaxDNSMessage {
			t.Fatal("unbounded answer")
		}
	})
}

func runnableName(t *testing.T, name string) bool {
	t.Helper()
	return testPolicy(t).AdmitsName(name)
}
