package fabric

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestExclusiveSetWriterFailsClosedOnLiveCrossPoolIP(t *testing.T) {
	now := time.Date(2026, 7, 28, 5, 0, 0, 0, time.UTC)
	memory := &MemorySetWriter{}
	writer := &ExclusiveSetWriter{Next: memory, Clock: func() time.Time { return now }}
	address := netip.MustParseAddr("203.0.113.12")
	if err := writer.Add(context.Background(), "general", address, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := writer.Add(context.Background(), "ai", address, time.Minute); !errors.Is(err, ErrAddressClassificationConflict) {
		t.Fatalf("expected classification conflict, got %v", err)
	}
	if writer.Conflicts() != 1 || len(memory.Snapshot()) != 1 {
		t.Fatalf("conflict was not isolated: conflicts=%d entries=%+v", writer.Conflicts(), memory.Snapshot())
	}
	now = now.Add(61 * time.Second)
	if err := writer.Add(context.Background(), "ai", address, time.Minute); err != nil {
		t.Fatalf("expired ownership was not released: %v", err)
	}
}

func TestExclusiveSetWriterSkipsExpiredRequests(t *testing.T) {
	memory := &MemorySetWriter{}
	writer := &ExclusiveSetWriter{Next: memory}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writer.Add(ctx, "general", netip.MustParseAddr("203.0.113.7"), time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired request = %v, want cancellation", err)
	}
	if len(memory.Snapshot()) != 0 {
		t.Fatal("expired request wrote a classification")
	}
}
