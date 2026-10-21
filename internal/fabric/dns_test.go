package fabric

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

type resolverFunc func(context.Context, string, *dns.Msg) (*dns.Msg, error)

func (fn resolverFunc) Exchange(ctx context.Context, target string, request *dns.Msg) (*dns.Msg, error) {
	return fn(ctx, target, request)
}

func TestDNSServerClassifiesRealAnswersForUDPAndTCP(t *testing.T) {
	classifier, err := NewClassifier([]Rule{
		{ID: "ai", Kind: RuleDomainSuffix, Value: "ai.example", PoolID: "pool-x", Enabled: true, Priority: 500},
	}, "pool-general", []string{"pool-general", "pool-x"})
	if err != nil {
		t.Fatal(err)
	}
	sets := &MemorySetWriter{}
	server, err := NewDNSServer(DNSConfig{
		Listen: "127.0.0.1:0", RejectHTTPSRecords: true, Classifier: classifier, SetWriter: sets,
		Resolver: resolverFunc(func(_ context.Context, target string, request *dns.Msg) (*dns.Msg, error) {
			response := new(dns.Msg)
			response.SetReply(request)
			if target == "pool-x" {
				response.Answer = []dns.RR{
					&dns.A{Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: net.ParseIP("203.0.113.9")},
					&dns.AAAA{Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 45}, AAAA: net.ParseIP("2001:db8::9")},
				}
			}
			return response, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Shutdown(context.Background()) }()

	for _, network := range []string{"udp", "tcp"} {
		request := new(dns.Msg)
		request.SetQuestion("chat.ai.example.", dns.TypeA)
		response, _, err := (&dns.Client{Net: network, Timeout: time.Second}).Exchange(request, server.Addr())
		if err != nil {
			t.Fatalf("%s query: %v", network, err)
		}
		if response.Rcode != dns.RcodeSuccess || len(response.Answer) != 2 {
			t.Fatalf("%s response: %+v", network, response)
		}
	}
	entries := sets.Snapshot()
	if names := server.hosts.lookup("127.0.0.1", "203.0.113.9", time.Now()); len(names) != 1 || names[0] != "chat.ai.example" {
		t.Fatalf("real DNS answer not retained as client-specific display hint: %v", names)
	}
	if len(entries) != 4 {
		t.Fatalf("set writes=%d want 4: %+v", len(entries), entries)
	}
	if entries[0].PoolID != "pool-x" || entries[0].Address != netip.MustParseAddr("203.0.113.9") || entries[0].TTL != 30*time.Second {
		t.Fatalf("unexpected first set entry: %+v", entries[0])
	}
	metrics := server.Metrics()
	if metrics.Queries != 2 || metrics.Classified != 2 || metrics.SetWrites != 4 || metrics.ByTarget["pool-x"] != 2 {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
}

func TestDNSServerListensOnIPv6AndClassifiesAAAA(t *testing.T) {
	classifier, err := NewClassifier([]Rule{
		{ID: "ipv6", Kind: RuleDomainSuffix, Value: "v6.example", PoolID: "pool-v6", Enabled: true, Priority: 500},
	}, "pool-general", []string{"pool-general", "pool-v6"})
	if err != nil {
		t.Fatal(err)
	}
	sets := &MemorySetWriter{}
	server, err := NewDNSServer(DNSConfig{
		Listen: "[::1]:0", Classifier: classifier, SetWriter: sets,
		Resolver: resolverFunc(func(_ context.Context, target string, request *dns.Msg) (*dns.Msg, error) {
			response := new(dns.Msg)
			response.SetReply(request)
			if target == "pool-v6" {
				response.Answer = []dns.RR{&dns.AAAA{
					Hdr:  dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 90},
					AAAA: net.ParseIP("2001:db8::42"),
				}}
			}
			return response, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Shutdown(context.Background()) }()

	for _, network := range []string{"udp6", "tcp6"} {
		request := new(dns.Msg)
		request.SetQuestion("chat.v6.example.", dns.TypeAAAA)
		response, _, err := (&dns.Client{Net: network, Timeout: time.Second}).Exchange(request, server.Addr())
		if err != nil {
			t.Fatalf("%s query: %v", network, err)
		}
		if response.Rcode != dns.RcodeSuccess || len(response.Answer) != 1 {
			t.Fatalf("%s response: %+v", network, response)
		}
	}
	entries := sets.Snapshot()
	if len(entries) != 2 {
		t.Fatalf("IPv6 set writes=%d want 2: %+v", len(entries), entries)
	}
	for _, entry := range entries {
		if entry.PoolID != "pool-v6" || entry.Address != netip.MustParseAddr("2001:db8::42") || entry.TTL != 90*time.Second {
			t.Fatalf("unexpected IPv6 set entry: %+v", entry)
		}
	}
}

func TestDNSServerClassificationHitRate(t *testing.T) {
	classifier, err := NewClassifier([]Rule{
		{ID: "classified", Kind: RuleDomainSuffix, Value: "classified.example", PoolID: "pool-classified", Enabled: true, Priority: 500},
	}, "pool-general", []string{"pool-general", "pool-classified"})
	if err != nil {
		t.Fatal(err)
	}
	sets := &countingSetWriter{}
	server, err := NewDNSServer(DNSConfig{
		Listen: "127.0.0.1:0", Classifier: classifier, SetWriter: sets,
		Resolver: resolverFunc(func(_ context.Context, _ string, request *dns.Msg) (*dns.Msg, error) {
			response := new(dns.Msg)
			response.SetReply(request)
			response.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.ParseIP("198.51.100.77"),
			}}
			return response, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Shutdown(context.Background()) }()

	const queries = 256
	client := &dns.Client{Net: "udp", Timeout: time.Second}
	for index := 0; index < queries; index++ {
		request := new(dns.Msg)
		request.SetQuestion("chat.classified.example.", dns.TypeA)
		response, _, err := client.Exchange(request, server.Addr())
		if err != nil {
			t.Fatalf("query %d: %v", index, err)
		}
		if response.Rcode != dns.RcodeSuccess {
			t.Fatalf("query %d rcode=%d", index, response.Rcode)
		}
	}
	metrics := server.Metrics()
	if metrics.Queries != queries || metrics.Classified != queries || metrics.Fallback != 0 || metrics.Failures != 0 || metrics.SetWrites != queries {
		t.Fatalf("classification hit rate was not 100%%: %+v", metrics)
	}
	if sets.Count.Load() != queries || metrics.ByTarget["pool-classified"] != queries {
		t.Fatalf("classification target or set writes mismatch: metrics=%+v writes=%d", metrics, sets.Count.Load())
	}
}

func BenchmarkDNSServerUDPClassification(benchmark *testing.B) {
	classifier, err := NewClassifier([]Rule{
		{ID: "classified", Kind: RuleDomainSuffix, Value: "classified.example", PoolID: "pool-classified", Enabled: true, Priority: 500},
	}, "pool-general", []string{"pool-general", "pool-classified"})
	if err != nil {
		benchmark.Fatal(err)
	}
	server, err := NewDNSServer(DNSConfig{
		Listen: "127.0.0.1:0", Classifier: classifier, SetWriter: &countingSetWriter{},
		Resolver: resolverFunc(func(_ context.Context, _ string, request *dns.Msg) (*dns.Msg, error) {
			response := new(dns.Msg)
			response.SetReply(request)
			response.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.ParseIP("198.51.100.77"),
			}}
			return response, nil
		}),
	})
	if err != nil {
		benchmark.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.Start(ctx); err != nil {
		cancel()
		benchmark.Fatal(err)
	}
	benchmark.Cleanup(func() {
		cancel()
		_ = server.Shutdown(context.Background())
	})
	client := &dns.Client{Net: "udp", Timeout: time.Second}
	request := new(dns.Msg)
	request.SetQuestion("chat.classified.example.", dns.TypeA)
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for benchmark.Loop() {
		response, _, err := client.Exchange(request, server.Addr())
		if err != nil {
			benchmark.Fatal(err)
		}
		if response.Rcode != dns.RcodeSuccess {
			benchmark.Fatalf("rcode=%d", response.Rcode)
		}
	}
}

func TestDNSServerRejectsHTTPSBeforeUpstream(t *testing.T) {
	classifier, err := NewClassifier(nil, "general", []string{"general"})
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	server, err := NewDNSServer(DNSConfig{
		Listen: "127.0.0.1:0", RejectHTTPSRecords: true, Classifier: classifier, SetWriter: &MemorySetWriter{},
		Resolver: resolverFunc(func(context.Context, string, *dns.Msg) (*dns.Msg, error) {
			calls++
			return nil, errors.New("must not be called")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Shutdown(context.Background()) }()
	request := new(dns.Msg)
	request.SetQuestion("example.com.", dns.TypeHTTPS)
	response, _, err := (&dns.Client{Net: "udp", Timeout: time.Second}).Exchange(request, server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if response.Rcode != dns.RcodeRefused || calls != 0 {
		t.Fatalf("response=%+v upstream calls=%d", response, calls)
	}
}

func TestDNSServerFailsClosedWhenSetWriteFails(t *testing.T) {
	classifier, err := NewClassifier(nil, "general", []string{"general"})
	if err != nil {
		t.Fatal(err)
	}
	sets := &MemorySetWriter{Err: errors.New("nft unavailable")}
	server, err := NewDNSServer(DNSConfig{
		Listen: "127.0.0.1:0", Classifier: classifier, SetWriter: sets,
		Resolver: resolverFunc(func(_ context.Context, _ string, request *dns.Msg) (*dns.Msg, error) {
			response := new(dns.Msg)
			response.SetReply(request)
			response.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.ParseIP("198.51.100.8"),
			}}
			return response, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Shutdown(context.Background()) }()
	request := new(dns.Msg)
	request.SetQuestion("example.com.", dns.TypeA)
	response, _, err := (&dns.Client{Net: "udp", Timeout: time.Second}).Exchange(request, server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if response.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode=%d want SERVFAIL", response.Rcode)
	}
	if metrics := server.Metrics(); metrics.SetWriteFailures != 1 || metrics.Failures != 1 {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
}

func TestDNSUpstreamRouterRoundRobinAndFallback(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	upstream := func(name string, err error) DNSUpstream {
		return upstreamFunc(func(_ context.Context, request *dns.Msg) (*dns.Msg, error) {
			mu.Lock()
			calls = append(calls, name)
			mu.Unlock()
			if err != nil {
				return nil, err
			}
			response := new(dns.Msg)
			response.SetReply(request)
			return response, nil
		})
	}
	router, err := NewDNSUpstreamRouter(map[string][]DNSUpstream{
		"general": {upstream("first", errors.New("down")), upstream("second", nil)},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := new(dns.Msg)
	request.SetQuestion("example.com.", dns.TypeA)
	if _, err := router.Exchange(context.Background(), "general", request); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0] != "first" || calls[1] != "second" {
		t.Fatalf("unexpected calls: %v", calls)
	}
}

type upstreamFunc func(context.Context, *dns.Msg) (*dns.Msg, error)

func (fn upstreamFunc) Exchange(ctx context.Context, request *dns.Msg) (*dns.Msg, error) {
	return fn(ctx, request)
}

type countingSetWriter struct {
	Count atomic.Uint64
}

func (writer *countingSetWriter) Add(context.Context, string, netip.Addr, time.Duration) error {
	writer.Count.Add(1)
	return nil
}
