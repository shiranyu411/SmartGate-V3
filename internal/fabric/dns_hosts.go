package fabric

import (
	"container/list"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// DNS associations are display hints, never routing or hostname evidence.
const maxDNSHostKeys = 4096

type dnsHostKey struct{ client, address netip.Addr }
type dnsHostEntry struct {
	key   dnsHostKey
	names map[string]time.Time
}
type dnsHostIndex struct {
	mu      sync.Mutex
	entries map[dnsHostKey]*list.Element
	lru     list.List
}

func newDNSHostIndex() *dnsHostIndex {
	return &dnsHostIndex{entries: make(map[dnsHostKey]*list.Element)}
}

func (index *dnsHostIndex) remember(client netip.Addr, name string, response *dns.Msg, now time.Time) {
	if index == nil || !client.IsValid() || response.Rcode != dns.RcodeSuccess {
		return
	}
	name = strings.ToLower(dns.Fqdn(name))
	if _, ok := dns.IsDomainName(name); !ok || len(name) > 254 {
		return
	}
	// Follow only this question's CNAME chain; unrelated answer RRs are ignored.
	allowed := map[string]uint32{name: ^uint32(0)}
	for range response.Answer {
		changed := false
		for _, rr := range response.Answer {
			cname, ok := rr.(*dns.CNAME)
			if !ok {
				continue
			}
			parent, ok := allowed[strings.ToLower(cname.Hdr.Name)]
			if !ok {
				continue
			}
			target := strings.ToLower(dns.Fqdn(cname.Target))
			ttl := min(parent, cname.Hdr.Ttl)
			if old, exists := allowed[target]; !exists || ttl < old {
				allowed[target] = ttl
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	for _, rr := range response.Answer {
		parentTTL, ok := allowed[strings.ToLower(rr.Header().Name)]
		if !ok {
			continue
		}
		var ip net.IP
		switch record := rr.(type) {
		case *dns.A:
			ip = record.A
		case *dns.AAAA:
			ip = record.AAAA
		default:
			continue
		}
		address, ok := netip.AddrFromSlice(ip)
		ttl := min(parentTTL, rr.Header().Ttl, uint32(3600))
		if !ok || ttl == 0 {
			continue
		}
		key := dnsHostKey{client.Unmap(), address.Unmap()}
		element := index.entries[key]
		if element == nil {
			if len(index.entries) >= maxDNSHostKeys {
				old := index.lru.Back()
				delete(index.entries, old.Value.(*dnsHostEntry).key)
				index.lru.Remove(old)
			}
			element = index.lru.PushFront(&dnsHostEntry{key: key, names: make(map[string]time.Time)})
			index.entries[key] = element
		}
		index.lru.MoveToFront(element)
		entry := element.Value.(*dnsHostEntry)
		domain := strings.TrimSuffix(name, ".")
		if _, exists := entry.names[domain]; !exists && len(entry.names) >= 4 {
			var oldest string
			for candidate, expires := range entry.names {
				if oldest == "" || expires.Before(entry.names[oldest]) {
					oldest = candidate
				}
			}
			delete(entry.names, oldest)
		}
		entry.names[domain] = now.Add(time.Duration(ttl) * time.Second)
	}
}

func (index *dnsHostIndex) lookup(client, address string, now time.Time) []string {
	if index == nil {
		return nil
	}
	source, err := netip.ParseAddr(client)
	if err != nil {
		return nil
	}
	destination, err := netip.ParseAddr(address)
	if err != nil {
		return nil
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	key := dnsHostKey{source.Unmap(), destination.Unmap()}
	element := index.entries[key]
	if element == nil {
		return nil
	}
	entry := element.Value.(*dnsHostEntry)
	var names []string
	for name, expires := range entry.names {
		if !expires.After(now) {
			delete(entry.names, name)
		} else {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		delete(index.entries, key)
		index.lru.Remove(element)
	} else {
		index.lru.MoveToFront(element)
	}
	sort.Strings(names)
	return names
}

// ConnectionDNSHosts returns recent client-scoped DNS display associations.
// Callers must not use these hints to infer an observed connection hostname.
func (manager *Manager) ConnectionDNSHosts(client, address string) []string {
	manager.mu.Lock()
	server := manager.dns
	manager.mu.Unlock()
	if server == nil {
		return nil
	}
	return server.hosts.lookup(client, address, time.Now())
}
