package fabric

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

var ErrAddressClassificationConflict = errors.New("address classification conflict")

type addressOwner struct {
	pool    string
	expires time.Time
}

type ExclusiveSetWriter struct {
	Next  AddressSetWriter
	Clock func() time.Time

	mu        sync.Mutex
	owners    map[netip.Addr]addressOwner
	conflicts atomic.Uint64
}

func (writer *ExclusiveSetWriter) Add(ctx context.Context, poolID string, address netip.Addr, ttl time.Duration) error {
	if writer.Next == nil {
		return fmt.Errorf("exclusive set writer target is required")
	}
	address = address.Unmap()
	if !address.IsValid() {
		return fmt.Errorf("invalid classification address")
	}
	now := time.Now().UTC()
	if writer.Clock != nil {
		now = writer.Clock().UTC()
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if writer.owners == nil {
		writer.owners = map[netip.Addr]addressOwner{}
	}
	if current, ok := writer.owners[address]; ok && current.expires.After(now) && current.pool != poolID {
		writer.conflicts.Add(1)
		return fmt.Errorf("%w: address %s is still owned by pool %s", ErrAddressClassificationConflict, address, current.pool)
	}
	if err := writer.Next.Add(ctx, poolID, address, ttl); err != nil {
		return err
	}
	writer.owners[address] = addressOwner{pool: poolID, expires: now.Add(ttl)}
	return nil
}

func (writer *ExclusiveSetWriter) Conflicts() uint64 {
	return writer.conflicts.Load()
}
