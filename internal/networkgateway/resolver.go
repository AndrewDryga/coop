package networkgateway

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/AndrewDryga/coop/internal/egress"
)

const (
	MaxDNSMessage    = 4096
	MaxDNSAnswers    = 32
	MaxCNAMEs        = 8
	MaxResolverNames = 4096
	MaxDNSInFlight   = 32
	MaxDNSTTL        = 5 * time.Minute
	DNSQueryTimeout  = 3 * time.Second
	// A sidecar address is fixed for the run, so the TTL only bounds how long a
	// client caches a fact that cannot change under it.
	ServiceAnswerTTL = 60
)

// Exchange is trusted resolver plumbing, never an agent-selected URL or socket.
// The production transport must authenticate its pinned upstream and bound I/O.
type Exchange func(context.Context, []byte) ([]byte, error)

type Resolution struct {
	Name       string
	Addresses  []netip.Addr
	Expires    BootInstant
	Cached     bool
	generation *resolutionGeneration
}

// Nonzero-sized pointer identity distinguishes refreshes even when a resolver
// returns decreasing TTLs that produce identical absolute expiry instants.
type resolutionGeneration [1]byte

type cacheEntry struct {
	resolution Resolution
	err        error
}

type Resolver struct {
	policy           egress.Snapshot
	protected        []netip.Prefix
	services         map[string]netip.Addr
	exchange         Exchange
	domain           ClockDomain
	now              func() BootInstant
	mu               sync.Mutex
	cache            map[string]cacheEntry
	active           map[string]chan struct{}
	lastOutcome      string
	slots            chan struct{}
	queries          atomic.Uint64
	failures         atomic.Uint64
	counterSaturated atomic.Bool
}

func NewResolver(policy egress.Snapshot, protected []netip.Prefix, services []ServiceBinding, clock *BootClock, exchange Exchange) (*Resolver, error) {
	if err := policy.RequireSupported(); err != nil {
		return nil, err
	}
	if exchange == nil || !clock.instant().Valid() {
		return nil, errors.New("network resolver needs a trusted exchange")
	}
	if _, err := addressGrants(policy, services); err != nil {
		return nil, err
	}
	// A service answer is a fixed fact from the host, not DNS authority: this
	// table is the ONLY name this resolver answers without an upstream query,
	// and it can only return the address the launch already granted.
	static := map[string]netip.Addr{}
	for _, binding := range services {
		static[binding.Name] = binding.Address
	}
	return &Resolver{policy: policy.Clone(), protected: slices.Clone(protected), services: static, exchange: exchange, domain: clock.Domain(), now: clock.instant,
		cache: map[string]cacheEntry{}, active: map[string]chan struct{}{}, slots: make(chan struct{}, MaxDNSInFlight)}, nil
}

// serviceAnswer reports the pinned address for an approved Compose sidecar
// name. The query name is matched exactly, case-insensitively, with at most one
// terminal dot: no search-domain guessing and no partial match.
func (r *Resolver) serviceAnswer(name string) (netip.Addr, bool) {
	address, ok := r.services[strings.ToLower(strings.TrimSuffix(name, "."))]
	return address, ok
}

// MaintenanceCounts exclude cache hits and agent DNS queries. A CNAME lookup
// can require multiple actual upstream exchanges; each is counted separately.
func (r *Resolver) MaintenanceCounts() (queries, failures uint64) {
	return r.queries.Load(), r.failures.Load()
}

// This is the last completed admitted lookup, not an active health probe or a
// promise that every name works. Cache hits and caller cancellation cannot
// manufacture recovery or turn an agent's denied name into a resolver outage.
func (r *Resolver) health() (status, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.lastOutcome {
	case "":
		return "ready", "resolver_initialized"
	case "success":
		return "ready", "last_lookup_succeeded"
	default:
		return "degraded", r.lastOutcome
	}
}

// refresh replaces only the generation whose remaining TTL was too short for
// kernel admission. Concurrent callers share Resolve's in-flight lookup and do
// not invalidate a newer answer another caller has already installed.
func (r *Resolver) refresh(ctx context.Context, previous Resolution) (Resolution, error) {
	r.mu.Lock()
	if entry, found := r.cache[previous.Name]; found && previous.generation != nil && entry.resolution.generation == previous.generation {
		delete(r.cache, previous.Name)
	}
	r.mu.Unlock()
	return r.Resolve(ctx, previous.Name)
}

// Resolve admits the original name before any query, validates every answer and
// never returns stale addresses. Consumers dial the returned binary address;
// using the name in a later dial would discard this security boundary.
func (r *Resolver) Resolve(ctx context.Context, name string) (Resolution, error) {
	name, err := egress.NormalizeDomain(name, false)
	if err != nil {
		return Resolution{}, Failure("dns_name_invalid")
	}
	// DNS admission is the NAME, not a port: a client has to resolve a name
	// before the kernel can record which granted port it is dialing.
	if !r.policy.AdmitsName(name) {
		return Resolution{}, Failure("unapproved_name")
	}
	for {
		if err := ctx.Err(); err != nil {
			return Resolution{}, Failure("dns_unavailable")
		}
		r.mu.Lock()
		now := r.now()
		if !now.Valid() {
			r.mu.Unlock()
			return Resolution{}, Failure("clock_unavailable")
		}
		if entry, found := r.cache[name]; found && now.Before(entry.resolution.Expires) {
			result := entry.resolution
			result.Addresses = slices.Clone(result.Addresses)
			result.Cached = true
			r.mu.Unlock()
			return result, entry.err
		}
		if done, busy := r.active[name]; busy {
			r.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return Resolution{}, Failure("dns_unavailable")
			}
		}
		for key, entry := range r.cache {
			if !now.Before(entry.resolution.Expires) {
				delete(r.cache, key)
			}
		}
		if len(r.cache)+len(r.active) >= MaxResolverNames {
			r.mu.Unlock()
			return Resolution{}, Failure("dns_capacity_exceeded")
		}
		select {
		case r.slots <- struct{}{}:
		default:
			r.mu.Unlock()
			return Resolution{}, Failure("dns_capacity_exceeded")
		}
		done := make(chan struct{})
		r.active[name] = done
		r.mu.Unlock()

		result, err := r.lookup(ctx, name)
		result.Name = name
		result.generation = new(resolutionGeneration)
		r.mu.Lock()
		completed := r.now()
		if !completed.Valid() {
			err = Failure("clock_unavailable")
		}
		if err != nil {
			// Bounded short negative caching prevents a failing approved name
			// from becoming an unbounded upstream query loop. Never cache IPs
			// from an unsuccessful lookup, even if part of its chain was valid.
			result.Addresses = nil
			result.Expires = completed.Add(time.Second)
		}
		// A caller cancellation is not a DNS failure for another tool waiting
		// on the same name. Wake surviving waiters so one can retry normally.
		if ctx.Err() == nil && completed.Valid() {
			r.cache[name] = cacheEntry{resolution: result, err: err}
			r.lastOutcome = "success"
			if err != nil {
				r.lastOutcome = "last_lookup_answer_rejected"
				if err == Failure("dns_unavailable") {
					r.lastOutcome = "last_lookup_exchange_failed"
				}
			}
		}
		delete(r.active, name)
		close(done)
		<-r.slots
		r.mu.Unlock()
		result.Addresses = slices.Clone(result.Addresses)
		return result, err
	}
}

// resolverRolloverPause is how long a lookup waits after an answer that arrived already expired
// before asking again.
var resolverRolloverPause = time.Second

func (r *Resolver) lookup(ctx context.Context, name string) (Resolution, error) {
	ctx, cancel := context.WithTimeout(ctx, DNSQueryTimeout)
	defer cancel()
	resolution, err := r.lookupChain(ctx, name)
	if err != Failure("dns_ttl_expired") {
		return resolution, err
	}
	// An upstream cache hands out a record in its last second with TTL 0 and refetches it a moment
	// later. Without the pause a CDN name whose records live seconds (Akamai's: 20 s) fails a flow
	// at every expiry — and the failure is cached for a second, so an immediate refresh fails too.
	select {
	case <-ctx.Done():
		return Resolution{}, Failure("dns_unavailable")
	case <-time.After(resolverRolloverPause):
	}
	return r.lookupChain(ctx, name)
}

func (r *Resolver) lookupChain(ctx context.Context, name string) (Resolution, error) {
	started := r.now()
	if !started.Valid() {
		return Resolution{}, Failure("clock_unavailable")
	}
	expires := started.Add(MaxDNSTTL)
	current := name
	seen := map[string]bool{name: true}
	for hops := 0; hops <= MaxCNAMEs; {
		query, err := makeQuery(current)
		if err != nil {
			return Resolution{}, err
		}
		wire, _ := query.Pack()
		addAtomic(&r.queries, 1, &r.counterSaturated)
		answer, err := r.exchange(ctx, wire)
		if err != nil || ctx.Err() != nil {
			addAtomic(&r.failures, 1, &r.counterSaturated)
			return Resolution{}, Failure("dns_unavailable")
		}
		parsed, err := parseAnswer(query, answer)
		if err != nil {
			addAtomic(&r.failures, 1, &r.counterSaturated)
			return Resolution{}, err
		}
		received := r.now()
		if !received.Valid() {
			return Resolution{}, Failure("clock_unavailable")
		}
		if received.Before(started) || received.Sub(started) > DNSQueryTimeout {
			return Resolution{}, Failure("dns_unavailable")
		}
		used := map[string]bool{}
		for {
			record, found := parsed[current]
			if !found {
				if len(used) != len(parsed) || len(used) == 0 {
					return Resolution{}, Failure("dns_answer_invalid")
				}
				break // A validated CNAME needs another bounded upstream lookup.
			}
			used[current] = true
			expiry := received.Add(time.Duration(record.ttl) * time.Second)
			if expiry.Before(expires) {
				expires = expiry
			}
			if !r.now().Before(expires) {
				return Resolution{}, Failure("dns_ttl_expired")
			}
			if record.cname != "" {
				hops++
				if hops > MaxCNAMEs || seen[record.cname] {
					return Resolution{}, Failure("dns_cname_limit")
				}
				seen[record.cname] = true
				current = record.cname
				continue
			}
			if len(used) != len(parsed) || len(record.addresses) == 0 {
				return Resolution{}, Failure("dns_answer_invalid")
			}
			for _, ip := range record.addresses {
				if !egress.PublicAnswer(ip, r.protected) {
					return Resolution{}, Failure("unsafe_dns_answer")
				}
			}
			slices.SortFunc(record.addresses, func(a, b netip.Addr) int { return a.Compare(b) })
			return Resolution{Addresses: slices.Compact(record.addresses), Expires: expires}, nil
		}
	}
	return Resolution{}, Failure("dns_cname_limit")
}

type answerRecord struct {
	addresses []netip.Addr
	cname     string
	ttl       uint32
}

func makeQuery(name string) (dnsmessage.Message, error) {
	var id [2]byte
	if _, err := rand.Read(id[:]); err != nil {
		return dnsmessage.Message{}, Failure("dns_unavailable")
	}
	dnsName, err := dnsmessage.NewName(name + ".")
	if err != nil {
		return dnsmessage.Message{}, Failure("dns_name_invalid")
	}
	return dnsmessage.Message{Header: dnsmessage.Header{ID: binary.BigEndian.Uint16(id[:]), RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsName, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}, nil
}

func parseAnswer(query dnsmessage.Message, wire []byte) (map[string]answerRecord, error) {
	if len(wire) > MaxDNSMessage || len(wire) < 12 {
		return nil, Failure("dns_answer_invalid")
	}
	// Bound header counts before a library decoder can allocate from them.
	if binary.BigEndian.Uint16(wire[4:6]) != 1 || binary.BigEndian.Uint16(wire[6:8]) > MaxDNSAnswers ||
		binary.BigEndian.Uint16(wire[8:10]) > MaxDNSAnswers || binary.BigEndian.Uint16(wire[10:12]) > MaxDNSAnswers {
		return nil, Failure("dns_answer_limit")
	}
	var response dnsmessage.Message
	if err := response.Unpack(wire); err != nil || !response.Response || response.ID != query.ID || response.OpCode != 0 || response.Truncated ||
		len(response.Questions) != 1 || response.Questions[0].Type != dnsmessage.TypeA || response.Questions[0].Class != dnsmessage.ClassINET {
		return nil, Failure("dns_answer_invalid")
	}
	question, err := egress.NormalizeDomain(response.Questions[0].Name.String(), false)
	if err != nil || question+"." != query.Questions[0].Name.String() {
		return nil, Failure("dns_answer_invalid")
	}
	if response.RCode != dnsmessage.RCodeSuccess || len(response.Answers) == 0 {
		return nil, Failure("dns_no_address")
	}
	if len(response.Answers) > MaxDNSAnswers {
		return nil, Failure("dns_answer_limit")
	}
	records := map[string]answerRecord{}
	for _, answer := range response.Answers {
		owner, err := egress.NormalizeDomain(answer.Header.Name.String(), false)
		if err != nil || answer.Header.Class != dnsmessage.ClassINET {
			return nil, Failure("dns_answer_invalid")
		}
		record, exists := records[owner]
		if !exists || answer.Header.TTL < record.ttl {
			record.ttl = answer.Header.TTL
		}
		switch body := answer.Body.(type) {
		case *dnsmessage.AResource:
			if record.cname != "" {
				return nil, Failure("dns_answer_invalid")
			}
			record.addresses = append(record.addresses, netip.AddrFrom4(body.A))
		case *dnsmessage.CNAMEResource:
			name, err := egress.NormalizeDomain(body.CNAME.String(), false)
			if err != nil || len(record.addresses) != 0 || (record.cname != "" && record.cname != name) {
				return nil, Failure("dns_answer_invalid")
			}
			record.cname = name
		default:
			return nil, Failure("dns_answer_invalid")
		}
		records[owner] = record
	}
	return records, nil
}

// parseQuery bounds header counts before Unpack allocates slices. Diagnostics
// must use this same parser: rejected packets are still hostile input.
func parseQuery(wire []byte) (dnsmessage.Message, error) {
	if len(wire) < 12 || len(wire) > MaxDNSMessage {
		return dnsmessage.Message{}, Failure("dns_query_invalid")
	}
	if binary.BigEndian.Uint16(wire[4:6]) != 1 || binary.BigEndian.Uint16(wire[6:8]) != 0 ||
		binary.BigEndian.Uint16(wire[8:10]) != 0 || binary.BigEndian.Uint16(wire[10:12]) > 1 {
		return dnsmessage.Message{}, Failure("dns_query_invalid")
	}
	var query dnsmessage.Message
	if err := query.Unpack(wire); err != nil || query.Response || query.OpCode != 0 || len(query.Questions) != 1 || len(query.Answers) != 0 || len(query.Authorities) != 0 {
		return dnsmessage.Message{}, Failure("dns_query_invalid")
	}
	for _, additional := range query.Additionals {
		if additional.Header.Type != dnsmessage.TypeOPT {
			return dnsmessage.Message{}, Failure("dns_query_invalid")
		}
	}
	return query, nil
}

// Answer resolves admitted A questions only. An admitted AAAA question gets
// local NODATA: refusing it makes dual-stack musl clients discard the valid A
// answer. This grants no IPv6 address, lookup or transport. Other query types
// and all client options remain outside upstream authority.
func (r *Resolver) Answer(ctx context.Context, wire []byte) ([]byte, string) {
	query, err := parseQuery(wire)
	if err != nil {
		return nil, "dns_query_invalid"
	}
	question := query.Questions[0]
	response := dnsmessage.Message{Header: dnsmessage.Header{ID: query.ID, Response: true, RecursionDesired: query.RecursionDesired, RecursionAvailable: true}, Questions: query.Questions}
	reason := ""
	service, isService := r.serviceAnswer(question.Name.String())
	switch {
	case question.Class != dnsmessage.ClassINET:
	case !isService:
	case question.Type == dnsmessage.TypeA:
		response.Answers = append(response.Answers, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: ServiceAnswerTTL},
			Body:   &dnsmessage.AResource{A: service.As4()},
		})
		fallthrough
	case question.Type == dnsmessage.TypeAAAA:
		// NODATA for AAAA, exactly as for an admitted public name: the grant is
		// IPv4 and a dual-stack client must not discard the A answer.
		data, err := response.Pack()
		if err != nil || len(data) > MaxDNSMessage {
			return nil, "dns_answer_limit"
		}
		return data, ""
	}
	if question.Class != dnsmessage.ClassINET {
		response.RCode, reason = dnsmessage.RCodeRefused, "dns_type_unsupported"
	} else if question.Type == dnsmessage.TypeAAAA {
		name, err := egress.NormalizeDomain(question.Name.String(), false)
		if err != nil {
			response.RCode, reason = dnsmessage.RCodeRefused, "dns_name_invalid"
		} else if !r.policy.AdmitsName(name) {
			response.RCode, reason = dnsmessage.RCodeRefused, "unapproved_name"
		}
	} else if question.Type != dnsmessage.TypeA {
		response.RCode, reason = dnsmessage.RCodeRefused, "dns_type_unsupported"
	} else {
		result, err := r.Resolve(ctx, question.Name.String())
		if err != nil {
			response.RCode, reason = dnsmessage.RCodeServerFailure, err.Error()
			if reason == "unapproved_name" || reason == "dns_name_invalid" {
				response.RCode = dnsmessage.RCodeRefused
			}
		} else {
			now := r.now()
			if !now.Before(result.Expires) {
				response.RCode, reason = dnsmessage.RCodeServerFailure, "dns_ttl_expired"
				if !now.Valid() {
					reason = "clock_unavailable"
				}
			} else {
				ttl := uint32(result.Expires.Sub(now) / time.Second)
				for _, ip := range result.Addresses {
					response.Answers = append(response.Answers, dnsmessage.Resource{
						Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: ttl},
						Body:   &dnsmessage.AResource{A: ip.As4()},
					})
				}
			}
		}
	}
	data, err := response.Pack()
	if err != nil || len(data) > MaxDNSMessage {
		return nil, "dns_answer_limit"
	}
	return data, reason
}
