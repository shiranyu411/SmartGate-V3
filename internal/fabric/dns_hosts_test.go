package fabric

import (
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func hostAnswer(name, ip string, ttl uint32) *dns.Msg {
	return &dns.Msg{Answer: []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: dns.Fqdn(name), Rrtype: dns.TypeA, Ttl: ttl}, A: net.ParseIP(ip)}}}
}

func TestDNSHostHintsIsolateClientsAndRespectCNAMEAndTTL(t *testing.T) {
	index := newDNSHostIndex()
	now := time.Now()
	client := netip.MustParseAddr("192.0.2.10")
	response := hostAnswer("edge.example", "198.51.100.1", 120)
	response.Answer = append(response.Answer,
		&dns.CNAME{Hdr: dns.RR_Header{Name: "service.example.", Rrtype: dns.TypeCNAME, Ttl: 10}, Target: "edge.example."},
		&dns.A{Hdr: dns.RR_Header{Name: "unrelated.example.", Rrtype: dns.TypeA, Ttl: 120}, A: net.ParseIP("198.51.100.2")},
	)
	index.remember(client, "Service.Example.", response, now)
	if names := index.lookup(client.String(), "198.51.100.1", now); len(names) != 1 || names[0] != "service.example" {
		t.Fatalf("CNAME hint: %v", names)
	}
	if names := index.lookup("192.0.2.11", "198.51.100.1", now); len(names) != 0 {
		t.Fatalf("cross-client hint leaked: %v", names)
	}
	if names := index.lookup(client.String(), "198.51.100.2", now); len(names) != 0 {
		t.Fatalf("unrelated RR attributed: %v", names)
	}
	if names := index.lookup(client.String(), "198.51.100.1", now.Add(10*time.Second)); len(names) != 0 {
		t.Fatalf("CNAME TTL not respected: %v", names)
	}
	index.remember(client, "zero.example", hostAnswer("zero.example", "198.51.100.1", 0), now)
	if names := index.lookup(client.String(), "198.51.100.1", now); len(names) != 0 {
		t.Fatalf("zero-TTL hint retained: %v", names)
	}
}

func TestDNSHostHintsKeepSharedIPAmbiguityAndBoundMemory(t *testing.T) {
	index := newDNSHostIndex()
	now := time.Now()
	client := netip.MustParseAddr("192.0.2.10")
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("site%d.example", i)
		index.remember(client, name, hostAnswer(name, "198.51.100.1", 30), now)
	}
	if names := index.lookup(client.String(), "198.51.100.1", now); len(names) != 4 {
		t.Fatalf("shared-IP names: %v", names)
	}
	for i := 0; i < maxDNSHostKeys+10; i++ {
		ip := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		index.remember(client, "bounded.example", hostAnswer("bounded.example", ip.String(), 30), now)
	}
	if len(index.entries) != maxDNSHostKeys || index.lru.Len() != maxDNSHostKeys {
		t.Fatalf("index unbounded: %d / %d", len(index.entries), index.lru.Len())
	}
}
