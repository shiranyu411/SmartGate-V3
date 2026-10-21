package fabric

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"smartgate/internal/netgate"
	"smartgate/internal/state"
)

type PoolSetNames struct {
	IPv4 string `json:"ipv4"`
	IPv6 string `json:"ipv6"`
}

type NftSetWriter struct {
	Runner netgate.Runner
	Family string
	Table  string
	Sets   map[string]PoolSetNames
}

func NftSetWriterForGraph(runner netgate.Runner, graph state.FabricGraph) NftSetWriter {
	sets := make(map[string]PoolSetNames, len(graph.DNS.PoolSets))
	for target, names := range graph.DNS.PoolSets {
		sets[target] = PoolSetNames{IPv4: names.IPv4, IPv6: names.IPv6}
	}
	return NftSetWriter{Runner: runner, Family: NftFamily, Table: NftTable, Sets: sets}
}

func (writer NftSetWriter) Add(ctx context.Context, poolID string, address netip.Addr, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !address.IsValid() {
		return fmt.Errorf("invalid classification address")
	}
	names, ok := writer.Sets[poolID]
	if !ok {
		return fmt.Errorf("pool %s has no nft classification set", poolID)
	}
	setName := names.IPv6
	if address.Is4() || address.Is4In6() {
		setName = names.IPv4
		address = address.Unmap()
	}
	if !validNftIdentifier(setName) {
		return fmt.Errorf("pool %s has invalid nft set name", poolID)
	}
	family := strings.TrimSpace(writer.Family)
	if family == "" {
		family = "inet"
	}
	table := strings.TrimSpace(writer.Table)
	if table == "" {
		table = "smartgate"
	}
	if !validNftIdentifier(family) || !validNftIdentifier(table) {
		return fmt.Errorf("invalid nft family or table")
	}
	seconds := int64(ttl.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	runner := writer.Runner
	if runner == nil {
		runner = netgate.ShellRunner{}
	}
	switch runner.(type) {
	case netgate.ShellRunner, *netgate.ShellRunner:
		if handled, err := addNativeSetElement(ctx, family, table, setName, address, time.Duration(seconds)*time.Second); handled {
			return err
		}
	}
	command := []string{
		"sh", "-c",
		"nft replace element \"$1\" \"$2\" \"$3\" \"{ $4 timeout ${5}s }\" 2>/dev/null || nft add element \"$1\" \"$2\" \"$3\" \"{ $4 timeout ${5}s }\"",
		"sh", family, table, setName, address.String(), strconv.FormatInt(seconds, 10),
	}
	_, err := runner.Run(ctx, command)
	return err
}

func validNftIdentifier(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

type MemorySetWriter struct {
	mu      sync.Mutex
	Entries []SetEntry
	Err     error
}

type SetEntry struct {
	PoolID  string
	Address netip.Addr
	TTL     time.Duration
}

func (writer *MemorySetWriter) Add(_ context.Context, poolID string, address netip.Addr, ttl time.Duration) error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.Err != nil {
		return writer.Err
	}
	writer.Entries = append(writer.Entries, SetEntry{PoolID: poolID, Address: address, TTL: ttl})
	return nil
}

func (writer *MemorySetWriter) Snapshot() []SetEntry {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return append([]SetEntry(nil), writer.Entries...)
}
