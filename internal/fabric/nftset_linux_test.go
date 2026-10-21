package fabric

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/google/nftables"
	"github.com/miekg/dns"
)

func TestNativeNftSetWriterKernel(t *testing.T) {
	if os.Getenv("SMARTGATE_NFT_KERNEL_TEST") != "1" {
		t.Skip("requires an isolated Linux rehearsal host and SMARTGATE_NFT_KERNEL_TEST=1")
	}
	conn := &nftables.Conn{}
	table := conn.AddTable(&nftables.Table{
		Family: nftables.TableFamilyINet, Name: fmt.Sprintf("sg_dns_test_%d", os.Getpid()),
	})
	sets := []*nftables.Set{
		{Table: table, Name: "v4", KeyType: nftables.TypeIPAddr, HasTimeout: true},
		{Table: table, Name: "v6", KeyType: nftables.TypeIP6Addr, HasTimeout: true},
	}
	for _, set := range sets {
		if err := conn.AddSet(set, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.DelTable(table)
		if err := conn.Flush(); err != nil {
			t.Error(err)
		}
	})
	writer := NftSetWriter{
		Table: table.Name,
		Sets:  map[string]PoolSetNames{"general": {IPv4: "v4", IPv6: "v6"}},
	}
	for index, raw := range []string{"203.0.113.7", "2001:db8::7"} {
		address := netip.MustParseAddr(raw)
		if err := writer.Add(context.Background(), "general", address, 10*time.Second); err != nil {
			t.Fatalf("initial %s add: %v", raw, err)
		}
		if err := writer.Add(context.Background(), "general", address, time.Minute); err != nil {
			t.Fatalf("refresh %s: %v", raw, err)
		}
		elements, err := conn.GetSetElements(sets[index])
		if err != nil {
			t.Fatal(err)
		}
		if len(elements) != 1 || elements[0].Expires < 50*time.Second {
			t.Fatalf("%s TTL was not refreshed: %+v", raw, elements)
		}
		conn.FlushSet(sets[index])
		if err := conn.Flush(); err != nil {
			t.Fatal(err)
		}
		if err := writer.Add(context.Background(), "general", address, time.Minute); err != nil {
			t.Fatalf("restore %s after rule rebuild: %v", raw, err)
		}
	}
	start := time.Now()
	for i := 0; i < 128; i++ {
		address := netip.AddrFrom4([4]byte{198, 51, 100, byte(i)})
		if err := writer.Add(context.Background(), "general", address, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("128 classified addresses written in %s without spawning nft", time.Since(start))

	classifier, err := NewClassifier(nil, "general", []string{"general"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewDNSServer(DNSConfig{
		Listen: "127.0.0.1:0", Classifier: classifier, SetWriter: &ExclusiveSetWriter{Next: writer},
		Resolver: resolverFunc(func(_ context.Context, _ string, request *dns.Msg) (*dns.Msg, error) {
			reply := new(dns.Msg)
			reply.SetReply(request)
			for i := 1; i <= 8; i++ {
				reply.Answer = append(reply.Answer, &dns.A{
					Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
					A:   net.IPv4(203, 0, 113, byte(i)),
				})
			}
			return reply, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Shutdown(context.Background()) }()
	start = time.Now()
	results := make(chan error, 32)
	for i := 0; i < cap(results); i++ {
		go func(index int) {
			request := new(dns.Msg)
			request.SetQuestion("fixture.example.", dns.TypeA)
			network := []string{"udp", "tcp"}[index%2]
			reply, _, err := (&dns.Client{Net: network, Timeout: 3 * time.Second}).Exchange(request, server.Addr())
			if err == nil && (reply.Rcode != dns.RcodeSuccess || len(reply.Answer) != 8) {
				err = fmt.Errorf("unexpected DNS result: rcode=%d answers=%d", reply.Rcode, len(reply.Answer))
			}
			results <- err
		}(i)
	}
	for i := 0; i < cap(results); i++ {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	t.Logf("32 concurrent UDP/TCP DNS queries with 8 addresses each completed in %s", time.Since(start))
}
